package cli

import "github.com/spf13/cobra"

func addWaveRunCommands(parent *cobra.Command) {
	run := &cobra.Command{
		Use:   "run",
		Short: "dispatch, check and integrate a plan wave by wave",
		Args:  noArgs,
		RunE:  notImplemented("wave run"),
	}
	for _, f := range []struct{ name, usage string }{
		{"plan", "plan path"},
		{"state", "state.json path"},
		{"into", "integration task id"},
		{"repo", "repository path (default cwd)"},
		{"max-in-flight", "packages dispatched at once"},
		{"gate", "command run after each integration"},
		{"full", "full verification command"},
		{"full-every", "run --full every N waves"},
		{"full-timeout", "seconds"},
		{"full-retry", "command re-run on a red --full's failing files"},
		{"on-green", "command run on a green --full"},
		{"notify-cmd", "command run on notable events"},
		{"stop-at", "epoch seconds to stop at"},
		{"max-waves", "stop after N waves"},
		{"max-packages", "stop after N packages"},
		{"run-id", "run ledger id"},
	} {
		strFlag(run, f.name, f.usage)
	}
	boolFlag(run, "resume-on-reset", "resume packages after a usage-limit reset")
	boolFlag(run, "no-ui", "do not start the dashboard")
	formatFlags(run, "json", "text")

	mark := &cobra.Command{
		Use:   "mark",
		Short: "hand a package back to the runner and clear its needs",
		Args:  noArgs,
		RunE:  notImplemented("wave mark"),
	}
	strFlag(mark, "state", "state.json path")
	strFlag(mark, "pkg", "package id")
	strFlag(mark, "status", "ready|pending|failed")
	strFlag(mark, "detail", "note")
	strFlag(mark, "widen", "paths to widen the package's scope by")
	strFlag(mark, "recheck", "recheck selector")
	strFlag(mark, "await", "package id to wait for")
	boolFlag(mark, "clear-environment", "clear the package's environment need")

	parent.AddCommand(run, mark)
}
