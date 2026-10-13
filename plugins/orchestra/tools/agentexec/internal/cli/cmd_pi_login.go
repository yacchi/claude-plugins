package cli

import "github.com/spf13/cobra"

func newPiCmd() *cobra.Command {
	pi := groupCommand("pi", "pi helpers")
	login := withPrefix(&cobra.Command{
		Use:   "login",
		Short: "log pi in through orchestra's own herdr",
		Args:  noArgs,
		RunE:  notImplemented("pi login"),
	}, "pi login")
	strFlag(login, "provider", "provider id")
	boolFlag(login, "wait", "poll until the login lands")
	strFlag(login, "paste", "send the final redirect URL")
	boolFlag(login, "cancel", "cancel a pending login")
	strFlag(login, "timeout", "seconds")
	boolFlag(login, "json", "JSON output (the default)")
	pi.AddCommand(login)
	return pi
}
