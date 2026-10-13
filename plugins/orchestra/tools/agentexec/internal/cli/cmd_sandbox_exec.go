package cli

import "github.com/spf13/cobra"

// newSandboxExecCmd is the Landlock backend's re-executed child: it restricts
// only itself, then execs the executor. Its argv is parsed by the sandbox
// package, not by cobra.
func newSandboxExecCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "_sandbox-exec",
		Hidden:             true,
		DisableFlagParsing: true,
		Args:               cobra.ArbitraryArgs,
		RunE:               notImplemented("_sandbox-exec"),
	}
}
