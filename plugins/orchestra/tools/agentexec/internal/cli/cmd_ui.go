package cli

import "github.com/spf13/cobra"

func newUICmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "start (or reuse) the local dashboard and print its URL",
		Args:  noArgs,
		RunE:  notImplemented("ui"),
	}
	boolFlag(cmd, "open", "open the URL in a browser")
	boolFlag(cmd, "stop", "stop the dashboard")
	boolFlag(cmd, "json", "JSON output")
	strFlag(cmd, "port", "port (default: any free port)")
	strFlag(cmd, "idle-minutes", "exit after N idle minutes")
	boolFlag(cmd, "serve", "run the server in the foreground")
	_ = cmd.Flags().MarkHidden("serve")
	return cmd
}
