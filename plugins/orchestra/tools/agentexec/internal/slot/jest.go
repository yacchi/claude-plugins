package slot

import (
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
)

// cpuCount is runtime.NumCPU, a variable so that tests can name a machine.
var cpuCount = runtime.NumCPU

// JestWorkerCap is how many jest workers one jest run may use while it holds
// the test pool: the cores shared out over the pool's slots (at least 1), or
// 0 when the pool is off. Each tree running jest under the pool would
// otherwise start about cores-1 workers of its own.
func JestWorkerCap(cfg *config.Resolved) int {
	return workerCap(Configured(cfg, Test).Size)
}

// ProcessJestWorkerCap is JestWorkerCap for the test pool as this process
// admits on it (see TestPool).
func ProcessJestWorkerCap() int {
	return workerCap(TestPool().Size)
}

func workerCap(size int) int {
	if size <= 0 {
		return 0
	}
	return max(1, cpuCount()/size)
}

var wordRE = regexp.MustCompile(`\S+`)

// CapJestWorkers adds ` --maxWorkers=n` to every jest run in command that
// names no worker flag of its own (--maxWorkers, -w, --runInBand, -i), and
// reports whether it changed command. A jest run is jest itself or jest
// behind a runner (npx, pnpm exec, yarn jest, ...). The flag goes right after
// the run's last argument, before a bare `--` (the words after it are passed
// on, not read by jest) and before the separator that ends the simple
// command. n <= 0 changes nothing.
func CapJestWorkers(command string, n int) (string, bool) {
	if n <= 0 {
		return command, false
	}
	masked := maskLiteral(command)
	var ats []int
	start := 0
	for _, sep := range splitRE.FindAllStringIndex(masked, -1) {
		if at, ok := jestCapAt(command, masked, start, sep[0]); ok {
			ats = append(ats, at)
		}
		start = sep[1]
	}
	if at, ok := jestCapAt(command, masked, start, len(masked)); ok {
		ats = append(ats, at)
	}
	if len(ats) == 0 {
		return command, false
	}
	flag := " --maxWorkers=" + strconv.Itoa(n)
	var b strings.Builder
	prev := 0
	for _, at := range ats {
		b.WriteString(command[prev:at])
		b.WriteString(flag)
		prev = at
	}
	b.WriteString(command[prev:])
	return b.String(), true
}

// jestCapAt is where the worker flag goes in the simple command command[s:e]
// when that command runs jest without a worker flag of its own.
func jestCapAt(command, masked string, s, e int) (int, bool) {
	spans := wordRE.FindAllStringIndex(masked[s:e], -1)
	words := make([]string, len(spans))
	for k, sp := range spans {
		words[k] = masked[s+sp[0] : s+sp[1]]
	}
	rest := unwrap(words)
	if len(rest) == 0 {
		return 0, false
	}
	j, ok := jestAt(rest)
	if !ok {
		return 0, false
	}
	jest := len(words) - len(rest) + j
	stop := e
	var args []string
	for k := jest + 1; k < len(words); k++ {
		if words[k] == "--" {
			stop = s + spans[k][0]
			break
		}
		args = append(args, command[s+spans[k][0]:s+spans[k][1]])
	}
	if hasWorkerFlag(args) {
		return 0, false
	}
	return s + len(strings.TrimRight(command[s:stop], " \t\r")), true
}

// jestAt is the index of the jest program in a simple command the unwrapper
// has already stripped of runners: `jest`, or `pnpm jest` / `yarn jest`
// (not `jest-junit`, a different program).
func jestAt(w []string) (int, bool) {
	if base(w[0]) == "jest" {
		return 0, true
	}
	if prog := base(w[0]); prog == "pnpm" || prog == "yarn" {
		if sub, i := arg(w, 1, packageManagers[prog]); base(sub) == "jest" {
			return i, true
		}
	}
	return 0, false
}

// hasWorkerFlag reports whether one of args (as typed, quotes included) is a
// jest worker flag, alone or as `flag=value`.
func hasWorkerFlag(args []string) bool {
	for _, a := range args {
		name, _, _ := strings.Cut(strings.Trim(a, `"'`), "=")
		if slices.Contains([]string{"--maxWorkers", "-w", "--runInBand", "-i"}, name) {
			return true
		}
	}
	return false
}

// maskLiteral is command with its data blanked to `_` at the same byte
// offsets, so that a position found in the mask is the same in command:
// heredoc bodies, quoted strings (newlines included), redirections such as
// 2>&1 (their `&` is no separator), and backslash-newline continuations
// (which join the lines into one simple command).
func maskLiteral(command string) string {
	b := []byte(command)
	maskHeredocBodies(b, command)
	for _, m := range quotedRE.FindAllStringIndex(string(b), -1) {
		blank(b, m[0], m[1])
	}
	for _, m := range dupRE.FindAllStringIndex(string(b), -1) {
		blank(b, m[0], m[1])
	}
	for i := 0; i+1 < len(b); i++ {
		if b[i] == '\\' && b[i+1] == '\n' {
			b[i], b[i+1] = ' ', ' '
		}
	}
	return string(b)
}

// maskHeredocBodies blanks each heredoc body and its delimiter line in b (a
// copy of command), keeping their newlines.
func maskHeredocBodies(b []byte, command string) {
	lines := strings.SplitAfter(command, "\n")
	starts := make([]int, len(lines))
	for i, off := 0, 0; i < len(lines); i++ {
		starts[i] = off
		off += len(lines[i])
	}
	for i := 0; i < len(lines); i++ {
		m := heredocRE.FindStringSubmatchIndex(lines[i])
		if m == nil {
			continue
		}
		delim := lines[i][m[2]:m[3]]
		j := i + 1
		for j < len(lines) && strings.TrimSpace(lines[j]) != delim {
			j++
		}
		for k := i + 1; k <= j && k < len(lines); k++ {
			blankLines(b, starts[k], starts[k]+len(lines[k]))
		}
		i = j
	}
}

// blank sets b[from:to] to `_`.
func blank(b []byte, from, to int) {
	for i := from; i < to; i++ {
		b[i] = '_'
	}
}

// blankLines is blank but keeps the newlines of b[from:to], which still split
// the commands around a heredoc body.
func blankLines(b []byte, from, to int) {
	for i := from; i < to; i++ {
		if b[i] != '\n' {
			b[i] = '_'
		}
	}
}
