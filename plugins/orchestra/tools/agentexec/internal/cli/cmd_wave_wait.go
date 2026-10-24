package cli

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/approvals"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/ledger"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/slot"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/usage"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/watch"
)

func newWaveWaitCmd() *cobra.Command {
	wait := &cobra.Command{
		Use:   "wait",
		Short: "block until a wave's runner ends, a new need appears, a change (--until-change), or a timeout",
		Args:  cobra.NoArgs,
	}
	fl := wait.Flags()
	statePath := fl.String("state", "", "state.json path")
	maxWait := fl.Float64("max-wait", watch.DefaultMaxWait.Seconds(), "seconds to wait at most")
	since := fl.Int("since", 0, "seq of the previous answer: needs and changes recorded before it are not new")
	untilChange := fl.Bool("until-change", false, "also answer \"changed\" on any event a viewer sees (status, dispatch, integrate, need, stop) after --since")
	_ = wait.MarkFlagRequired("state")
	wait.RunE = func(cmd *cobra.Command, _ []string) error {
		if !finiteNonNegative(*maxWait) {
			return exitf(2, "wave wait: --max-wait must be a non-negative number of seconds")
		}
		if *since < 0 {
			return exitf(2, "wave wait: --since must not be negative")
		}
		answer := watch.Wait(watch.WaitOptions{
			State: *statePath, Since: *since, MaxWait: duration(*maxWait, time.Second),
			UntilChange: *untilChange, Cost: waveRunCost, Approvals: waveApprovals,
			Machine: func() watch.Machine { return watch.ProbeMachine(slot.HeavyPool(), slot.TestPool()) },
		})
		return writeJSONLine(cmd.OutOrStdout(), answer)
	}
	return wait
}

// waveApprovals are the items `agent-exec approvals --state` lists; none
// when the state cannot be read.
func waveApprovals(statePath string) []watch.Approval {
	items, err := approvals.PendingIn(statePath, configOrDefaults())
	if err != nil {
		return nil
	}
	return watch.ApprovalsOf(items)
}

// waveRunCost is a run's estimated cost in USD, the figure `agent-exec
// usage --run ID` prints.
func waveRunCost(runID string) (float64, bool) {
	cfg := configOrDefaults()
	if resolved, ok := ledger.ResolveRunSelector(ledger.Dir(cfg), runID); ok {
		runID = resolved
	}
	now := time.Now().UTC()
	cutoff, _ := usage.ParseSince("24h", now)
	report := usage.BuildReport(usage.ReportOptions{
		Since: cutoff, Now: now, Sources: append([]string(nil), usage.Sources...), Cfg: cfg,
		RunIDs: []string{runID},
	})
	return float64(report.Cost.EstimatedMicroUSD) / 1e6, true
}
