package herdr

import (
	"context"
	"errors"
	"fmt"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/proctree"
)

// The tree guard watches the process tree under the worker pane's shell, so
// a runaway the worker starts (a fork bomb, a test runner's worker pool) is
// killed before it takes the machine down. The seams let tests replace the
// pane lookup, the measurement, the kill and the limits; none of them
// touches a real process.
var (
	treeGuardLookup  = func(e *Executor, ctx context.Context, s *host.Session) int { return e.treeGuardRoot(ctx, s) }
	treeGuardMeasure = proctree.Measure
	treeGuardKill    = proctree.KillTree
	treeGuardLimits  = proctree.DefaultLimits
)

// treeGuardShellPID is the shell_pid of a pane's process-info payload, 0
// when it is missing or not a pid.
func treeGuardShellPID(payload map[string]any) int {
	v, ok := Find(payload, "shell_pid").(float64)
	if !ok || v <= 1 {
		return 0
	}
	return int(v)
}

// treeGuardRoot is the shell pid of the session's pane, 0 when the lookup
// fails: an unknown root runs the wait unguarded.
func (e *Executor) treeGuardRoot(ctx context.Context, s *host.Session) int {
	pane := s.Str("pane")
	if pane == "" {
		return 0
	}
	payload, _, err := e.call(ctx, callTimeout, "", "pane", "process-info", "--pane", pane)
	if err != nil {
		return 0
	}
	return treeGuardShellPID(payload)
}

// treeGuardCheck measures the tree under root once. need is the failure to
// report when the tree tripped its limits (after it was killed); keep says
// whether the wait goes on guarding that root.
func treeGuardCheck(root int) (need *host.NeedError, keep bool) {
	u, err := treeGuardMeasure(root)
	if errors.Is(err, proctree.ErrUnsupported) {
		return nil, false
	}
	if err != nil {
		return nil, true
	}
	text, tripped := treeGuardLimits().Exceeded(u)
	if !tripped {
		return nil, true
	}
	killed, kerr := treeGuardKill(root, true)
	text += fmt.Sprintf("; killed the worker's process tree (%d processes)", killed)
	if kerr != nil && !errors.Is(kerr, proctree.ErrNoRoot) {
		text += "; " + kerr.Error()
	}
	return host.Need(host.KindError, text), false
}
