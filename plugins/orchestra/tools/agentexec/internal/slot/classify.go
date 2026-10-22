package slot

import (
	"regexp"
	"strings"
)

// Heavy commands are told apart here and nowhere else: the worker hook asks
// `agent-exec slot acquire --command`, which asks Classify.

var (
	quotedRE   = regexp.MustCompile(`'[^']*'|"(?:[^"\\]|\\.)*"`)
	dupRE      = regexp.MustCompile(`[0-9]*>&[0-9-]*|&>>?`)
	splitRE    = regexp.MustCompile(`&&|\|\||[;&|\n()]`)
	heredocRE  = regexp.MustCompile(`<<-?\s*['"]?([A-Za-z_][A-Za-z0-9_]*)['"]?`)
	assignRE   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	versionArg = map[string]bool{"--version": true, "-v": true, "-V": true, "--help": true, "-h": true}
)

// Programs that are heavy whatever follows them.
var heavyPrograms = map[string]bool{
	"tsc": true, "vue-tsc": true, "vitest": true, "jest": true, "mocha": true, "pytest": true,
	"tox": true, "mypy": true, "pyright": true, "eslint": true, "biome": true, "ruff": true,
	"golangci-lint": true, "esbuild": true,
}

// Programs heavy only with one of these subcommands (first word that is not
// a flag).
var heavySubcommands = map[string]map[string]bool{
	"next":       {"build": true},
	"vite":       {"build": true},
	"cdk":        {"synth": true},
	"playwright": {"test": true},
	"go":         {"test": true, "build": true},
	"cargo":      {"fetch": true, "build": true, "test": true, "check": true, "clippy": true},
	"pip":        {"install": true},
	"pip3":       {"install": true},
	"uv":         {"sync": true},
	"bundle":     {"install": true},
	"poetry":     {"install": true},
}

// Package managers: install verbs, `test`, and the heavy scripts (run
// directly or through `run`).
var packageManagers = map[string]map[string]bool{
	// Flags that take their value as the next word.
	"pnpm": {"--filter": true, "-F": true, "-C": true, "--dir": true, "--reporter": true},
	"npm":  {"-w": true, "--workspace": true, "--prefix": true, "-C": true},
	"yarn": {"--cwd": true},
	"bun":  {"--cwd": true, "--filter": true, "-F": true},
}

var installVerbs = map[string]bool{"install": true, "i": true, "add": true, "ci": true}

// Script names (before any `:suffix`) that typecheck, lint, test or build.
var heavyScripts = map[string]bool{
	"test": true, "lint": true, "typecheck": true, "type-check": true, "tsc": true, "build": true,
}

// Wrapper value flags: `nice -n 5`, `timeout -s KILL 300`, `sudo -u root`.
var wrapperFlags = map[string]map[string]bool{
	"sudo":    {"-u": true, "-g": true, "-h": true, "-p": true, "-C": true, "-D": true, "-R": true, "-T": true},
	"nice":    {"-n": true},
	"timeout": {"-s": true, "-k": true, "--signal": true, "--kill-after": true},
	"env":     {"-u": true, "-C": true, "-S": true, "--unset": true, "--chdir": true},
}

// Runner value flags: `npx -p pkg tsc`, `uv run --with x pytest`.
var runnerFlags = map[string]bool{
	"-p": true, "--package": true, "--with": true, "--project": true, "--directory": true,
	"--python": true, "--env-file": true, "-C": true, "--filter": true, "-F": true, "--dir": true,
}

// Classify reports whether command, a Bash command line, runs a heavy local
// workload. A compound line is heavy when any simple command in it is;
// quoted strings and heredoc bodies are data, never commands. extra are the
// `throttle.heavy_extra` patterns, matched against each simple command with
// its env assignments and wrappers removed. op names the first heavy command
// ("pnpm test", "tsc"), or the first command when none is heavy.
func Classify(command string, extra []*regexp.Regexp) (op string, heavy bool) {
	for _, seg := range segments(command) {
		words := unwrap(strings.Fields(seg))
		if len(words) == 0 {
			continue
		}
		o := opOf(words)
		if op == "" {
			op = o
		}
		if isHeavy(words) || matchesAny(extra, strings.Join(words, " ")) {
			return o, true
		}
	}
	if op == "" {
		op = "bash"
	}
	return op, false
}

// CompileExtra compiles `throttle.heavy_extra`; an invalid pattern is
// skipped.
func CompileExtra(patterns []string) []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, p := range patterns {
		if re, err := regexp.Compile(p); err == nil {
			out = append(out, re)
		}
	}
	return out
}

func matchesAny(res []*regexp.Regexp, s string) bool {
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// segments are the simple commands of a line: heredoc bodies dropped, each
// quoted string reduced to one placeholder word (so a quoted flag value is
// still one word), split on && || ; & | ( ) and newlines.
func segments(command string) []string {
	text := stripHeredocs(command)
	text = quotedRE.ReplaceAllString(text, " _ ")
	text = dupRE.ReplaceAllString(text, " ")
	return splitRE.Split(text, -1)
}

// stripHeredocs removes each heredoc body up to its delimiter line.
func stripHeredocs(command string) string {
	var b strings.Builder
	lines := strings.Split(command, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		m := heredocRE.FindStringSubmatchIndex(line)
		if m == nil {
			b.WriteString(line)
			b.WriteByte('\n')
			continue
		}
		b.WriteString(line[:m[0]])
		b.WriteByte('\n')
		delim := line[m[2]:m[3]]
		for i+1 < len(lines) && strings.TrimSpace(lines[i+1]) != delim {
			i++
		}
		i++ // the delimiter line
	}
	return b.String()
}

// unwrap drops env assignments, wrappers (time, exec, nohup, nice, timeout,
// env, sudo) and runners (npx, bunx, pnpm exec/dlx, uv/poetry run, python
// -m, mise exec), leaving the command that does the work.
func unwrap(w []string) []string {
	for len(w) > 0 {
		head := base(w[0])
		switch {
		case assignRE.MatchString(w[0]):
			w = w[1:]
		case head == "time" || head == "exec" || head == "nohup" || head == "command":
			if head == "command" && len(w) > 1 && (w[1] == "-v" || w[1] == "-V") {
				return nil // a lookup, not a run
			}
			w = w[1:]
		case wrapperFlags[head] != nil:
			i := skipFlags(w, 1, wrapperFlags[head])
			if head == "timeout" {
				i++ // the duration
			}
			w = w[min(i, len(w)):]
		case head == "npx" || head == "bunx" || head == "pnpx":
			w = w[min(skipFlags(w, 1, runnerFlags), len(w)):]
		case (head == "pnpm" || head == "yarn" || head == "npm" || head == "bun") && runnerVerb(w):
			i := skipFlags(w, 1, packageManagers[head])
			w = w[min(skipFlags(w, i+1, runnerFlags), len(w)):]
		case (head == "uv" || head == "poetry" || head == "pipenv") && len(w) > 1 && w[1] == "run":
			w = w[min(skipFlags(w, 2, runnerFlags), len(w)):]
		case strings.HasPrefix(head, "python") && len(w) > 2 && w[1] == "-m":
			w = w[2:]
		case head == "mise" && len(w) > 1 && (w[1] == "exec" || w[1] == "x"):
			// `mise exec go@1.26 node@24 -- go test`: the command follows --.
			i := 2
			for i < len(w) && w[i] != "--" {
				i++
			}
			w = w[min(i+1, len(w)):]
		default:
			return w
		}
	}
	return w
}

// runnerVerb: `pnpm exec x`, `pnpm dlx x`, `yarn dlx x`, `npm exec x`, `bun x x`.
func runnerVerb(w []string) bool {
	i := skipFlags(w, 1, packageManagers[base(w[0])])
	if i >= len(w) {
		return false
	}
	switch w[i] {
	case "exec", "dlx", "x":
		return true
	}
	return false
}

// skipFlags is the index of the first word from i on that is not a flag,
// stepping over the value of each flag in valued.
func skipFlags(w []string, i int, valued map[string]bool) int {
	for i < len(w) && (strings.HasPrefix(w[i], "-") || strings.HasPrefix(w[i], "+")) {
		if valued[w[i]] {
			i++
		}
		i++
	}
	return i
}

func base(word string) string {
	return word[strings.LastIndex(word, "/")+1:]
}

// arg is the first non-flag word from i on, and its index ("" when none).
func arg(w []string, i int, valued map[string]bool) (string, int) {
	i = skipFlags(w, i, valued)
	if i >= len(w) {
		return "", i
	}
	return w[i], i
}

func isHeavy(w []string) bool {
	program := base(w[0])
	if len(w) == 2 && versionArg[w[1]] {
		return false
	}
	if heavyPrograms[program] {
		return true
	}
	if subs := heavySubcommands[program]; subs != nil {
		sub, i := arg(w, 1, nil)
		if program == "go" && sub == "mod" {
			next, _ := arg(w, i+1, nil)
			return next == "download"
		}
		if program == "uv" && sub == "pip" {
			next, _ := arg(w, i+1, nil)
			return next == "install"
		}
		return subs[sub]
	}
	if program == "turbo" {
		task, i := arg(w, 1, nil)
		if task == "run" {
			task, _ = arg(w, i+1, nil)
		}
		return heavyScript(task)
	}
	if valued, ok := packageManagers[program]; ok {
		sub, i := arg(w, 1, valued)
		switch {
		case sub == "":
			return program == "yarn" // bare `yarn` installs
		case installVerbs[sub] || sub == "t":
			return true
		case sub == "run" || sub == "run-script":
			script, _ := arg(w, i+1, valued)
			return heavyScript(script)
		case program == "npm":
			return sub == "test" // npm runs other scripts only through `run`
		}
		return heavyScript(sub) || heavyPrograms[sub] // `pnpm vitest` runs the bin
	}
	return false
}

func heavyScript(name string) bool {
	name, _, _ = strings.Cut(name, ":")
	return heavyScripts[name]
}

// opOf names a simple command the way the trace does: the program, plus the
// subcommand for tool front-ends ("pnpm test", "go test", "tsc").
func opOf(w []string) string {
	program := base(w[0])
	valued := packageManagers[program]
	if valued == nil && heavySubcommands[program] == nil && program != "turbo" {
		return program
	}
	sub, i := arg(w, 1, valued)
	if sub == "run" || sub == "run-script" {
		if script, _ := arg(w, i+1, valued); script != "" {
			sub = script
		}
	}
	if sub == "" {
		return program
	}
	return program + " " + sub
}
