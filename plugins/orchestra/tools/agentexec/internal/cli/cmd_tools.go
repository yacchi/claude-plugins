package cli

import "github.com/spf13/cobra"

// newToolsCmd: bare `tools` is `tools status`.
func newToolsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tools",
		Short: "orchestra-managed executor binaries",
		Args:  noArgs,
		RunE:  notImplemented("tools"),
	}
	formatFlags(cmd, "json", "text")
	status := withPrefix(&cobra.Command{
		Use:   "status",
		Short: "where each tool resolves from",
		Args:  noArgs,
		RunE:  notImplemented("tools status"),
	}, "tools")
	formatFlags(status, "json", "text")
	install := withPrefix(&cobra.Command{
		Use:   "install [NAME...]",
		Short: "install or update the pinned tools",
		Args:  cobra.ArbitraryArgs,
		RunE:  notImplemented("tools install"),
	}, "tools")
	strFlag(install, "installer", "auto|mise|npm")
	boolFlag(install, "force", "reinstall even when current")
	formatFlags(install, "json", "text")
	cmd.AddCommand(status, install)
	return withPrefix(cmd, "tools")
}
