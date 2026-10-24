package cli

import "github.com/spf13/cobra"

// newUICmd is the removed local dashboard, kept only to point at its
// replacement: every invocation, whatever its arguments, is a usage error.
func newUICmd() *cobra.Command {
	return &cobra.Command{
		Use:                "ui",
		Short:              "removed: see the orchestra dashboard Artifact",
		Hidden:             true,
		Args:               cobra.ArbitraryArgs,
		DisableFlagParsing: true,
		RunE: func(*cobra.Command, []string) error {
			return exitf(2, "agent-exec ui was removed: wave progress is shown in the orchestra dashboard Artifact (dashboard.artifact_url; see skills/run/references/programme.md §6)")
		},
	}
}
