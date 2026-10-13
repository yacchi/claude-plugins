package cli

import (
	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/sandbox"
)

// newSandboxExecCmd is the Landlock backend's re-executed child: it restricts
// only itself, then execs the executor. Its argv is parsed by the sandbox
// package, not by cobra; it writes its own diagnostics.
func newSandboxExecCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "_sandbox-exec",
		Hidden:             true,
		DisableFlagParsing: true,
		Args:               cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			return &ExitError{Code: sandbox.ExecMain(args)}
		},
	}
}
