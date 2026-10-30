package slot

import (
	"regexp"
	"slices"
	"strings"
)

// Heavy commands are told apart here and nowhere else: the worker hook asks
// `agent-exec slot classify` and `slot acquire`, which ask Classify.

// Kinds of heavy command; each kind is admitted on its own machine-wide pool.
const (
	// KindTest is a test, lint or typecheck run (the test pool).
	KindTest = "test"
	// KindBuild is an install, a build or a heavy_extra match (the heavy pool).
	KindBuild = "build"
)

// Scopes of a command line: a full one runs the test suite of a whole
// repository or workspace, which a worker leaves to the run after
// integration.
const (
	ScopeFull     = "full"
	ScopeTargeted = "targeted"
)

var (
	quotedRE   = regexp.MustCompile(`'[^']*'|"(?:[^"\\]|\\.)*"`)
	dupRE      = regexp.MustCompile(`[0-9]*>&[0-9-]*|&>>?`)
	splitRE    = regexp.MustCompile(`&&|\|\||[;&|\n()]`)
	heredocRE  = regexp.MustCompile(`<<-?\s*['"]?([A-Za-z_][A-Za-z0-9_]*)['"]?`)
	assignRE   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	versionArg = map[string]bool{"--version": true, "-v": true, "-V": true, "--help": true, "-h": true}
)

// Programs that are heavy whatever follows them, and their kind.
var heavyPrograms = map[string]string{
	"tsc": KindTest, "vue-tsc": KindTest, "vitest": KindTest, "jest": KindTest, "mocha": KindTest,
	"pytest": KindTest, "tox": KindTest, "mypy": KindTest, "pyright": KindTest, "eslint": KindTest,
	"biome": KindTest, "ruff": KindTest, "golangci-lint": KindTest, "esbuild": KindBuild,
}

// Programs heavy only with one of these subcommands (first word that is not
// a flag), and the subcommand's kind.
var heavySubcommands = map[string]map[string]string{
	"next":       {"build": KindBuild},
	"vite":       {"build": KindBuild},
	"cdk":        {"synth": KindBuild},
	"playwright": {"test": KindTest},
	"go":         {"test": KindTest, "vet": KindTest, "build": KindBuild},
	"cargo": {
		"fetch": KindBuild, "build": KindBuild, "test": KindTest, "check": KindTest, "clippy": KindTest,
	},
	"make":   {"test": KindTest},
	"pip":    {"install": KindBuild},
	"pip3":   {"install": KindBuild},
	"uv":     {"sync": KindBuild},
	"bundle": {"install": KindBuild},
	"poetry": {"install": KindBuild},
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

// Script names (before any `:suffix`) that typecheck, lint, test or build,
// and their kind.
var heavyScripts = map[string]string{
	"test": KindTest, "lint": KindTest, "typecheck": KindTest, "type-check": KindTest, "tsc": KindTest,
	"build": KindBuild,
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

// Value flags of the test runners, so that a flag's value is not read as a
// path or name argument.
var (
	pytestFlags = map[string]bool{
		"-m": true, "-c": true, "-p": true, "-n": true, "-o": true, "-W": true, "--rootdir": true,
		"--ignore": true, "--deselect": true, "--confcutdir": true, "--basetemp": true, "--maxfail": true,
		"--durations": true, "--junitxml": true, "--tb": true,
	}
	jsRunnerFlags = map[string]bool{
		"-c": true, "--config": true, "--reporter": true, "--root": true, "--environment": true,
		"--pool": true, "--maxWorkers": true, "--outputFile": true, "--workers": true, "-j": true,
	}
	toxFlags = map[string]bool{"-e": true, "-c": true, "-p": true, "--workdir": true}
)

// Flags that pick tests by name or path, or a package by name or directory:
// the run is targeted however wide its directory.
var (
	jsFilterFlags = []string{
		"-t", "--testNamePattern", "--grep", "-g", "--project", "--selectProjects", "--testPathPattern",
		"--changed", "--related", "--findRelatedTests", "--onlyChanged", "-o",
	}
	pmScopeFlags = []string{"--filter", "-F", "-C", "--dir", "-w", "--workspace", "--prefix", "--cwd"}
)

// Class is what Classify tells of a command line.
type Class struct {
	// Op names the first heavy command ("pnpm test", "tsc"), or the first
	// command when none is heavy.
	Op    string
	Heavy bool
	// Kind is KindBuild when any heavy command of the line is one, else
	// KindTest; "" when none is heavy.
	Kind string
	// Scope is ScopeFull when any test run of the line covers a whole
	// repository or workspace, else ScopeTargeted.
	Scope string
}

// Pool is the name of the machine-wide pool the line is admitted on: the
// test pool for a line of tests, lints and typechecks only, else the heavy
// pool (also for what is not heavy at all: a check command of unknown kind).
func (c Class) Pool() string {
	if c.Kind == KindTest {
		return Test
	}
	return Heavy
}

// Classify tells what command, a Bash command line, runs: whether it is a
// heavy local workload, of which kind, and whether it runs a whole suite. A
// compound line is heavy when any simple command in it is; quoted strings
// and heredoc bodies are data, never commands. extra are the
// `throttle.heavy_extra` patterns, matched against each simple command with
// its env assignments and wrappers removed; a command only they match is of
// KindBuild and never full.
func Classify(command string, extra []*regexp.Regexp) Class {
	c := Class{Scope: ScopeTargeted}
	movedIn := false // a relative `cd` earlier in the line
	for _, seg := range segments(command) {
		words := unwrap(strings.Fields(seg))
		if len(words) == 0 {
			continue
		}
		o := opOf(words)
		if c.Op == "" {
			c.Op = o
		}
		if base(words[0]) == "cd" {
			movedIn = intoSubdir(words)
			continue
		}
		kind := kindOf(words)
		if kind == "" && matchesAny(extra, strings.Join(words, " ")) {
			kind = KindBuild
		}
		if kind == "" {
			continue
		}
		if !c.Heavy {
			c.Op, c.Heavy, c.Kind = o, true, kind
		}
		if kind == KindBuild {
			c.Kind = KindBuild
		}
		if kind == KindTest && fullRun(words, movedIn) {
			c.Scope = ScopeFull
		}
	}
	if c.Op == "" {
		c.Op = "bash"
	}
	return c
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
		next, ok := unwrapOnce(w)
		if !ok {
			return w
		}
		w = next
	}
	return w
}

// unwrapOnce drops the outermost env assignment, wrapper or runner of w: one
// layer of unwrap. false when w starts with none of them.
func unwrapOnce(w []string) ([]string, bool) {
	head := base(w[0])
	switch {
	case assignRE.MatchString(w[0]):
		return w[1:], true
	case head == "time" || head == "exec" || head == "nohup" || head == "command":
		if head == "command" && len(w) > 1 && (w[1] == "-v" || w[1] == "-V") {
			return nil, true // a lookup, not a run
		}
		return w[1:], true
	case wrapperFlags[head] != nil:
		i := skipFlags(w, 1, wrapperFlags[head])
		if head == "timeout" {
			i++ // the duration
		}
		return w[min(i, len(w)):], true
	case head == "npx" || head == "bunx" || head == "pnpx":
		return w[min(skipFlags(w, 1, runnerFlags), len(w)):], true
	case (head == "pnpm" || head == "yarn" || head == "npm" || head == "bun") && runnerVerb(w):
		i := skipFlags(w, 1, packageManagers[head])
		return w[min(skipFlags(w, i+1, runnerFlags), len(w)):], true
	case (head == "uv" || head == "poetry" || head == "pipenv") && len(w) > 1 && w[1] == "run":
		return w[min(skipFlags(w, 2, runnerFlags), len(w)):], true
	case strings.HasPrefix(head, "python") && len(w) > 2 && w[1] == "-m":
		return w[2:], true
	case head == "mise" && len(w) > 1 && (w[1] == "exec" || w[1] == "x"):
		// `mise exec go@1.26 node@24 -- go test`: the command follows --.
		i := 2
		for i < len(w) && w[i] != "--" {
			i++
		}
		return w[min(i+1, len(w)):], true
	}
	return w, false
}

// RunsProgram reports whether a simple command of command runs the program
// name (a base name; a `@version` suffix is ignored): the command's own
// program, or one that an npx, bunx, pnpx or corepack launcher runs. Each
// layer is checked as unwrap strips it, so `pnpm exec tsc` runs pnpm as well
// as tsc. Quoted strings and heredoc bodies are data, as in Classify.
func RunsProgram(command, name string) bool {
	for _, seg := range segments(command) {
		w := strings.Fields(seg)
		for len(w) > 0 {
			if prog, _, _ := strings.Cut(base(w[0]), "@"); prog == name {
				return true
			}
			if base(w[0]) == "corepack" {
				w = w[1:]
				continue
			}
			next, ok := unwrapOnce(w)
			if !ok {
				break
			}
			w = next
		}
	}
	return false
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

// kindOf is the kind of a simple command, "" when it is not heavy.
func kindOf(w []string) string {
	program := base(w[0])
	if len(w) == 2 && versionArg[w[1]] {
		return ""
	}
	if kind := heavyPrograms[program]; kind != "" {
		return kind
	}
	if subs := heavySubcommands[program]; subs != nil {
		sub, i := arg(w, 1, nil)
		if program == "go" && sub == "mod" {
			if next, _ := arg(w, i+1, nil); next == "download" {
				return KindBuild
			}
			return ""
		}
		if program == "uv" && sub == "pip" {
			if next, _ := arg(w, i+1, nil); next == "install" {
				return KindBuild
			}
			return ""
		}
		return subs[sub]
	}
	if program == "turbo" {
		return scriptKind(turboTask(w))
	}
	if valued, ok := packageManagers[program]; ok {
		sub, i := arg(w, 1, valued)
		switch {
		case sub == "":
			if program == "yarn" {
				return KindBuild // bare `yarn` installs
			}
			return ""
		case installVerbs[sub]:
			return KindBuild
		case sub == "t":
			return KindTest
		case sub == "run" || sub == "run-script":
			script, _ := arg(w, i+1, valued)
			return scriptKind(script)
		case program == "npm":
			if sub == "test" { // npm runs other scripts only through `run`
				return KindTest
			}
			return ""
		}
		if kind := scriptKind(sub); kind != "" {
			return kind
		}
		return heavyPrograms[sub] // `pnpm vitest` runs the bin
	}
	return ""
}

func scriptKind(name string) string {
	name, _, _ = strings.Cut(name, ":")
	return heavyScripts[name]
}

// isTestScript: `test`, its `test:suffix` variants, or the `t` alias.
func isTestScript(name string) bool {
	name, _, _ = strings.Cut(name, ":")
	return name == "test" || name == "t"
}

// turboTask is the task of `turbo run <task>` or `turbo <task>`.
func turboTask(w []string) string {
	task, i := arg(w, 1, nil)
	if task == "run" {
		task, _ = arg(w, i+1, nil)
	}
	return task
}

// intoSubdir reports whether `cd` words move below the directory the line
// started in: a relative target other than `.`, `-` or one going up (an
// unreadable target, such as a quoted one, counts as below).
func intoSubdir(w []string) bool {
	if len(w) < 2 {
		return false
	}
	to := w[1]
	switch {
	case to == "." || to == ".." || to == "-" || to == "--":
		return false
	case strings.HasPrefix(to, "/") || strings.HasPrefix(to, "~") || strings.HasPrefix(to, "../"):
		return false
	}
	return true
}

// hasFlag reports whether a word is one of flags, alone or as `flag=value`.
func hasFlag(w []string, flags ...string) bool {
	return slices.ContainsFunc(w, func(word string) bool {
		name, _, _ := strings.Cut(word, "=")
		return slices.Contains(flags, name)
	})
}

// positional reports whether a word from i on is an argument rather than a
// flag or the value of one of valued's flags (`--` passes arguments on, so
// the words after it count too).
func positional(w []string, i int, valued map[string]bool) bool {
	for ; i < len(w); i++ {
		switch {
		case w[i] == "--":
		case valued[w[i]]:
			i++
		case strings.HasPrefix(w[i], "-"):
		default:
			return true
		}
	}
	return false
}

// fullRun reports whether a KindTest simple command runs the whole suite of
// a repository or workspace. It errs toward targeted, since a false full
// blocks real work: a path, package or test-name argument scopes the run,
// and so does a relative `cd` earlier in the line for a command whose reach
// is the directory it runs in. go's `./...` is read as is: it names every
// package below, conventionally from the module root a worker cds into.
// Lints and typechecks (tsc, eslint, cargo check) are never full: most have
// no narrower form to point a worker to.
func fullRun(w []string, movedIn bool) bool {
	program := base(w[0])
	sub, i := arg(w, 1, nil)
	switch {
	case program == "go":
		if hasFlag(w, "-run", "--run") {
			return false
		}
		return slices.ContainsFunc(w[1:], func(a string) bool { return a == "./..." || a == "..." || a == "all" })
	case movedIn:
		return false
	case program == "make":
		return !hasFlag(w, "-C", "--directory", "-f", "--file", "--makefile") &&
			!slices.ContainsFunc(w[1:], assignRE.MatchString)
	case program == "cargo":
		if sub != "test" {
			return false
		}
		if hasFlag(w, "--workspace", "--all") {
			return true
		}
		return !hasFlag(w, "-p", "--package", "--manifest-path") && !positional(w[:dashes(w)], i+1, nil)
	case program == "pytest":
		return !hasFlag(w, "-k") && !positional(w, 1, pytestFlags)
	case program == "tox":
		return !positional(w, 1, toxFlags)
	case program == "vitest" || program == "jest" || program == "mocha":
		from := 1
		if program == "vitest" && (sub == "run" || sub == "watch") {
			from = i + 1
		}
		return !hasFlag(w, jsFilterFlags...) && !positional(w, from, jsRunnerFlags)
	case program == "playwright":
		return !hasFlag(w, jsFilterFlags...) && !positional(w, i+1, jsRunnerFlags)
	case program == "turbo":
		return isTestScript(turboTask(w)) && !hasFlag(w, "--filter", "-F")
	}
	valued, ok := packageManagers[program]
	if !ok {
		return false
	}
	sub, i = arg(w, 1, valued)
	if sub == "run" || sub == "run-script" {
		sub, i = arg(w, i+1, valued)
	}
	return isTestScript(sub) && !hasFlag(w, pmScopeFlags...) && !positional(w, i+1, nil)
}

// dashes is the index of the first `--`, or len(w).
func dashes(w []string) int {
	if i := slices.Index(w, "--"); i >= 0 {
		return i
	}
	return len(w)
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
