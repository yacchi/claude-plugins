package config

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// nodeCapState points the state dir (where the preload is written) at a
// temporary dir whose path needs quoting, and names a 14-core machine.
func nodeCapState(t *testing.T) string {
	t.Helper()
	state := filepath.Join(t.TempDir(), `it's "state"`)
	t.Setenv("XDG_STATE_HOME", state)
	was := CPUCount
	t.Cleanup(func() { CPUCount = was })
	CPUCount = func() int { return 14 }
	return state
}

// jest and vitest start N-1 workers, so N is one more than each slot's share
// of the cores.
func TestNodeCapCPUsSharesTheCoresOverTheTestPool(t *testing.T) {
	nodeCapState(t)
	for slots, want := range map[int]int{2: 8, 1: 15, 20: 2, 0: 0} {
		if got := NodeCapCPUs(slots); got != want {
			t.Errorf("%d slots: %d cpus, want %d", slots, got, want)
		}
	}
	if got := NodeCapCPUs(PoolSize(nil, PoolTest)); got != 8 {
		t.Errorf("auto test pool (14 cores -> 2 slots): %d cpus, want 8", got)
	}
}

func TestNodeCapEntries(t *testing.T) {
	nodeCapState(t)
	if got := NodeCapEntries(0, nil); got != nil {
		t.Errorf("pool off: %q, want none", got)
	}
	require := NodeCapRequire(NodeCapPreloadPath())
	got := NodeCapEntries(2, nil)
	if len(got) != 2 || got[0] != NodeCPUsEnv+"=8" || got[1] != "NODE_OPTIONS="+require {
		t.Fatalf("entries %q", got)
	}
	data, err := os.ReadFile(NodeCapPreloadPath())
	if err != nil || !bytes.Equal(data, nodeCapPreload) {
		t.Fatalf("preload not written (%v)", err)
	}
	got = NodeCapEntries(2, []string{"NODE_OPTIONS=--max-old-space-size=4096"})
	if got[1] != "NODE_OPTIONS="+require+" --max-old-space-size=4096" {
		t.Errorf("existing NODE_OPTIONS: %q", got[1])
	}
	again := NodeCapEntries(2, []string{got[1]})
	if again[1] != got[1] {
		t.Errorf("require added twice: %q", again[1])
	}
}

// Every process a tree starts gets the cap, once, however often the tree's
// env is built on top of itself.
func TestTreeEntriesCapNode(t *testing.T) {
	nodeCapState(t)
	was := ProcessTestSlots
	t.Cleanup(func() { ProcessTestSlots = was })
	ProcessTestSlots = func() int { return 2 }
	env := WithTreeEnv(false, "", []string{"NODE_OPTIONS=--trace-warnings"})
	env = WithTreeEnv(false, t.TempDir(), env)
	opts, _ := envValue(env, "NODE_OPTIONS")
	if strings.Count(opts, "--require") != 1 || !strings.HasSuffix(opts, " --trace-warnings") {
		t.Errorf("NODE_OPTIONS %q", opts)
	}
	if n, _ := envValue(env, NodeCPUsEnv); n != "8" {
		t.Errorf("%s=%q, want 8", NodeCPUsEnv, n)
	}
	ProcessTestSlots = func() int { return 0 }
	if got := WithNodeCap([]string{"A=1"}); len(got) != 1 {
		t.Errorf("pool off: env %q, want unchanged", got)
	}
}

// The preload caps what Node reports to CommonJS and ESM alike, padding
// os.cpus() when the cap exceeds the real list.
func TestNodeCapPreloadInNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	nodeCapState(t)
	entries := NodeCapEntries(1, nil)
	run := func(cpus string, args ...string) string {
		t.Helper()
		cmd := exec.Command(node, args...)
		cmd.Env = append(os.Environ(), entries[1], NodeCPUsEnv+"="+cpus)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("node %q: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	cjs := `const os = require("os"); console.log(os.availableParallelism(), os.cpus().length)`
	if got := run("3", "-e", cjs); got != "3 3" {
		t.Errorf("CommonJS: %q, want 3 3", got)
	}
	if got := run("4096", "-e", cjs); got != "4096 4096" {
		t.Errorf("padded: %q, want 4096 4096", got)
	}
	esm := `import {availableParallelism, cpus} from "node:os"; console.log(availableParallelism(), cpus().length)`
	if got := run("3", "--input-type=module", "-e", esm); got != "3 3" {
		t.Errorf("ESM: %q, want 3 3", got)
	}
	real := run("bogus", "-e", `console.log(require("os").availableParallelism() === require("os").cpus().length)`)
	if real != "true" {
		t.Errorf("invalid ORCHESTRA_NODE_CPUS changed os: %q", real)
	}
}

// nodeEvalForkScript forks itself exactly once and reports the child. The
// NODE_EVAL_DEPTH variable bounds it: a child that runs this script without
// the preload's stop prints and ends, it never forks again.
const nodeEvalForkScript = `if (process.env.NODE_EVAL_DEPTH) { console.log("child ran"); process.exit(0) }
const c = require("child_process").fork(process.env.NODE_EVAL_CHILD || "x", [], {
    execArgv: process.env.NODE_EVAL_CHILD ? [] : [...process.execArgv],
    silent: true, env: {...process.env, NODE_EVAL_DEPTH: "1"} })
let o = "", e = ""
c.stdout.on("data", d => o += d); c.stderr.on("data", d => e += d)
c.on("close", code => console.log(JSON.stringify({code: code ?? -1, stdout: o, stderr: e})))
""`

type nodeEvalChild struct {
	Code   int    `json:"code"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
}

// nodeEvalRun runs node with args under the preload (nodeCapState must be
// set up) and returns the JSON the top-level script printed. The shell's
// NODE_OPTIONS and cap variable are left out, so only the preload is in play.
func nodeEvalRun(t *testing.T, node string, extraEnv []string, args ...string) nodeEvalChild {
	t.Helper()
	drop := []string{NodeOptionsEnv + "=", NodeCPUsEnv + "=", "NODE_EVAL_DEPTH=", "NODE_EVAL_CHILD="}
	var env []string
	for _, kv := range os.Environ() {
		kept := true
		for _, p := range drop {
			if strings.HasPrefix(kv, p) {
				kept = false
			}
		}
		if kept {
			env = append(env, kv)
		}
	}
	env = append(env, NodeCapEntries(1, nil)[1])
	env = append(env, extraEnv...)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, args...)
	cmd.Env = env
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("node %q: %v\nstdout: %s\nstderr: %s", args, err, out, stderr)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	last := ""
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			last = lines[i]
			break
		}
	}
	var got nodeEvalChild
	if err := json.Unmarshal([]byte(last), &got); err != nil {
		t.Fatalf("node %q: parse %q: %v\nstdout: %s", args, last, err, out)
	}
	return got
}

// A forked child whose execArgv holds an eval flag stops before it runs the
// eval script again. The cap variable is unset or invalid in some cases: the
// stop does not depend on it.
func TestNodeEvalForkedChildStops(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	nodeCapState(t)
	cases := []struct {
		name string
		env  []string
		args []string
		flag string
	}{
		{"-e", nil, []string{"-e", nodeEvalForkScript}, "-e"},
		{"--eval", nil, []string{"--eval", nodeEvalForkScript}, "--eval"},
		{"--eval=", nil, []string{"--eval=" + nodeEvalForkScript}, "--eval"},
		{"-p", nil, []string{"-p", nodeEvalForkScript}, "-p"},
		{"--print", nil, []string{"--print", nodeEvalForkScript}, "--print"},
		{"-pe", nil, []string{"-pe", nodeEvalForkScript}, "-pe"},
		{"-e bogus cap", []string{NodeCPUsEnv + "=bogus"}, []string{"-e", nodeEvalForkScript}, "-e"},
		{"-e capped", []string{NodeCPUsEnv + "=3"}, []string{"-e", nodeEvalForkScript}, "-e"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := nodeEvalRun(t, node, c.env, c.args...)
			want := nodeEvalChild{
				Code:   1,
				Stdout: "",
				Stderr: "orchestra: refusing to start a forked child of 'node " + c.flag + "': it would run the eval script again and multiply (a fork bomb). Run the test runner through the package's test script, not node -e.\n",
			}
			if got != want {
				t.Errorf("child = %+v, want %+v", got, want)
			}
		})
	}
}

// The stop is for eval flags only: a forked child without one still runs, and
// the cap still applies to it.
func TestNodeEvalForkedChildWithoutEvalFlagRuns(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	nodeCapState(t)
	child := filepath.Join(t.TempDir(), "child.js")
	if err := os.WriteFile(child, []byte(`console.log(require("os").availableParallelism())`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := nodeEvalRun(t, node, []string{NodeCPUsEnv + "=3", "NODE_EVAL_CHILD=" + child}, "-e", nodeEvalForkScript)
	want := nodeEvalChild{Code: 0, Stdout: "3\n", Stderr: ""}
	if got != want {
		t.Errorf("child = %+v, want %+v", got, want)
	}
}

// The prefix is empty with the test pool off, and otherwise one line.
func TestNodeCapShellPrefixLine(t *testing.T) {
	nodeCapState(t)
	if p := NodeCapShellPrefix(0); p != "" {
		t.Errorf("test pool off: prefix %q", p)
	}
	if p := NodeCapShellPrefix(1); strings.Count(p, "\n") != 0 || !strings.HasSuffix(p, "; ") {
		t.Errorf("prefix %q: want one line ending in \"; \"", p)
	}
}

// The prefix runs in each shell before every command: it sets the CPU count,
// adds the require once in front of the shell's own NODE_OPTIONS, and exits 0.
func TestNodeCapShellPrefixInShells(t *testing.T) {
	nodeCapState(t)
	prefix := NodeCapShellPrefix(1)
	require := NodeCapRequire(NodeCapPreloadPath())
	shells := []struct {
		name string
		args []string
	}{
		{"sh", nil},
		{"bash", nil},
		{"zsh", []string{"-f"}},
	}
	cases := []struct {
		name  string
		extra []string // NODE_OPTIONS as the shell starts, when set
		times int      // how many times the prefix runs in a row
		want  string
	}{
		{"unset", nil, 1, "0|15|" + require},
		{"unset twice", nil, 2, "0|15|" + require},
		{"keeps own options", []string{"NODE_OPTIONS=--a  --b"}, 1, "0|15|" + require + " --a  --b"},
		{"keeps own options twice", []string{"NODE_OPTIONS=--a  --b"}, 2, "0|15|" + require + " --a  --b"},
		{"already has require", []string{"NODE_OPTIONS=" + require}, 1, "0|15|" + require},
		{"require after other options", []string{"NODE_OPTIONS=--x " + require}, 1, "0|15|--x " + require},
	}
	const report = `printf '%s|%s|%s' "$?" "$ORCHESTRA_NODE_CPUS" "$NODE_OPTIONS"`
	for _, sh := range shells {
		t.Run(sh.name, func(t *testing.T) {
			path, err := exec.LookPath(sh.name)
			if err != nil {
				t.Skipf("%s not installed: %v", sh.name, err)
			}
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					script := strings.Repeat(prefix, c.times) + report
					args := append(append([]string{}, sh.args...), "-c", script)
					cmd := exec.Command(path, args...)
					cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, c.extra...)
					out, err := cmd.Output()
					if err != nil {
						t.Fatal(err)
					}
					if got := string(out); got != c.want {
						t.Errorf("%s: got %q, want %q", sh.name, got, c.want)
					}
				})
			}
		})
	}
}
