package herdr

import (
	"runtime"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
)

// Where herdr does not exist the host is unavailable and Start refuses with
// a need before it touches the repo or spawns anything.
func TestUnsupportedOSRefuses(t *testing.T) {
	if (runtime.GOOS == "windows") != (refusal != "") {
		t.Fatalf("refusal on %s = %q", runtime.GOOS, refusal)
	}
	msg := refusal
	if msg == "" {
		msg = "herdr is not supported on this test's OS"
	}
	unsupportedOS = msg
	t.Cleanup(func() { unsupportedOS = "" })
	e := newEnv(t)
	e.scenario(map[string]any{}, true)
	x := e.executor("")
	if ok, why := x.Available(); ok || why != msg {
		t.Errorf("Available = %v %q, want false %q", ok, why, msg)
	}
	_, err := e.start(x, "a")
	if n := needOf(t, err); n.Kind != host.KindError || n.Detail != msg {
		t.Errorf("need %+v, want error %q", n, msg)
	}
	if len(e.spawns) != 0 || len(e.calls()) != 0 {
		t.Errorf("spawns %v calls %v, want none", e.spawns, e.calls())
	}
	if out := gitRun(t, e.repo, "worktree", "list"); strings.Count(out, "\n") != 1 {
		t.Errorf("a worktree was made:\n%s", out)
	}
}
