package isolate

import (
	"fmt"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
)

// Land modes, the values of isolation.land.
const (
	LandModeAuto = "auto"
	LandModeAsk  = "ask"
)

// LandMode is isolation.land of cfg. It defaults to auto so a finished run
// lands without a question; only an explicit "ask" keeps the land with the
// user.
func LandMode(cfg *config.Resolved) string {
	if v, ok := cfg.Lookup("isolation", "land"); ok && v == LandModeAsk {
		return LandModeAsk
	}
	return LandModeAuto
}

// AutoLand is Land through OnFeatureBranch: the assembly task lands on the
// checked-out branch, or on a new feature/<slug> when that is the default
// branch, so nothing lands on the default branch by itself.
func AutoLand(root, task, message, slug string) (LandResult, error) {
	res, created, err := OnFeatureBranch(root, slug, func() (LandResult, error) {
		return Land(root, task, message)
	})
	res.Created = created
	if res.Task == "" {
		res.Task = task
	}
	return res, err
}

// AutoLandPath is LandPath through OnFeatureBranch, for an Agent-tool
// worktree that lands the same way an assembly does.
func AutoLandPath(root, path, baseline, message, slug string) (LandResult, error) {
	res, created, err := OnFeatureBranch(root, slug, func() (LandResult, error) {
		return LandPath(root, path, baseline, message)
	})
	res.Created = created
	if res.Path == "" {
		res.Path = path
	}
	return res, err
}

// FinishLand is the land that ends a finished run, by mode. It never returns
// an error: a run's land is reported in the result, so an unusable input
// comes back as Status "error" for the run to show, not as a failure of the
// run itself.
func FinishLand(mode, root, task, message, slug string) LandResult {
	if task == "" {
		return LandResult{Status: "absent", Reason: "no assembly to land"}
	}
	if mode == LandModeAsk {
		return LandResult{Status: "ask", Task: task,
			Reason: fmt.Sprintf("isolation.land is ask: when the user asks, land it from their checkout with 'agent-exec isolate land --request %s -m <message>'", task)}
	}
	// Checked before AutoLand so a blank message creates no feature branch.
	if strings.TrimSpace(message) == "" {
		return LandResult{Status: "refused", Task: task,
			Reason: fmt.Sprintf("no commit message to land with: land it from the checkout with 'agent-exec isolate land --request %s -m <message in the repository's commit convention>'", task)}
	}
	res, err := AutoLand(root, task, message, slug)
	if err != nil {
		return LandResult{Status: "error", Task: task, Reason: err.Error()}
	}
	return res
}
