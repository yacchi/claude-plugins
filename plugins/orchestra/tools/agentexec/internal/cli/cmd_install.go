package cli

import "github.com/spf13/cobra"

func newInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "interactive installer (adds a shim to PATH)",
		Args:  cobra.ArbitraryArgs,
		RunE:  notImplemented("install"),
	}
}
