package cli

import "github.com/spf13/cobra"

func addWaveStatusCommands(parent *cobra.Command) {
	status := withPrefix(&cobra.Command{
		Use:   "status",
		Short: "render a wave's state.json",
		Args:  noArgs,
		RunE:  notImplemented("wave status"),
	}, "wave")
	strFlag(status, "state", "state.json path")
	formatFlags(status, "json", "line", "text")
	boolFlag(status, "live", "include live worktree changes")
	strFlag(status, "watch", "redraw every SEC seconds")
	strFlag(status, "events", "show the last N events")

	stop := withPrefix(&cobra.Command{
		Use:   "stop",
		Short: "ask a wave's runner to stop",
		Args:  noArgs,
		RunE:  notImplemented("wave stop"),
	}, "wave")
	strFlag(stop, "state", "state.json path")
	strFlag(stop, "reason", "why")

	lint := &cobra.Command{
		Use:   "lint",
		Short: "check a wave plan",
		Args:  noArgs,
		RunE:  notImplemented("wave lint"),
	}
	strFlag(lint, "plan", "plan path")
	formatFlags(lint, "json", "text")

	parent.AddCommand(status, stop, lint)
}
