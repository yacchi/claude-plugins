package cli

import "github.com/spf13/cobra"

func newUsageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "aggregate token usage and estimated cost",
		Args:  noArgs,
		RunE:  notImplemented("usage"),
	}
	strFlag(cmd, "since", "<N>m|<N>h|<N>d or an ISO8601 timestamp (default 24h)")
	listFlag(cmd, "run", "run id(s), comma-separated (repeatable)")
	listFlag(cmd, "session", "session id(s), comma-separated (repeatable)")
	strFlag(cmd, "source", "comma-separated sources (claude,pi)")
	boolFlag(cmd, "all-projects", "do not scope to this working directory")
	boolFlag(cmd, "list-runs", "list the recorded runs")
	formatFlags(cmd, "json", "text")
	return cmd
}
