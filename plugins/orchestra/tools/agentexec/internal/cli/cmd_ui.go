package cli

import "github.com/spf13/cobra"

// newUICmd is the removed local web UI, kept only to point at the status
// file: every invocation, whatever its arguments, is a usage error.
func newUICmd() *cobra.Command {
	return &cobra.Command{
		Use:                "ui",
		Short:              "removed: see ~/.claude/orchestra/status.json",
		Hidden:             true,
		Args:               cobra.ArbitraryArgs,
		DisableFlagParsing: true,
		RunE: func(*cobra.Command, []string) error {
			return exitf(2, "agent-exec ui was removed: wave and plan progress is written to ~/.claude/orchestra/status.json (agent-exec status show prints it; see docs/status-file.md)")
		},
	}
}
