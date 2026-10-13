package cli

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/install"
)

func newInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "interactive installer (adds a shim to PATH)",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			code := install.Run(cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr(),
				os.Getenv("AGENT_EXEC_BOOTSTRAP"))
			if code != 0 {
				return &ExitError{Code: code}
			}
			return nil
		},
	}
}
