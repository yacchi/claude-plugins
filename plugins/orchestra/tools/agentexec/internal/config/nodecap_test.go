package config

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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

// The shell prefix survives a preload path that needs quoting and keeps the
// shell's own NODE_OPTIONS after the require.
func TestNodeCapShellPrefixQuoting(t *testing.T) {
	nodeCapState(t)
	t.Setenv("NODE_OPTIONS", "--trace-warnings")
	prefix := NodeCapShellPrefix(1)
	out, err := exec.Command("/bin/sh", "-c", prefix+`printf '%s|%s' "$ORCHESTRA_NODE_CPUS" "$NODE_OPTIONS"`).Output()
	if err != nil {
		t.Fatal(err)
	}
	want := NodeCapEntries(1, []string{"NODE_OPTIONS=--trace-warnings"})
	if got := string(out); got != strings.TrimPrefix(want[0], NodeCPUsEnv+"=")+"|"+strings.TrimPrefix(want[1], "NODE_OPTIONS=") {
		t.Errorf("shell saw %q, want entries %q", got, want)
	}
	if p := NodeCapShellPrefix(0); p != "" {
		t.Errorf("test pool off: prefix %q", p)
	}
}
