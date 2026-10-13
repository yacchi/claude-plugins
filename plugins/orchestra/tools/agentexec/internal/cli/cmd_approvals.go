package cli

import "github.com/spf13/cobra"

func newApprovalsCmd() *cobra.Command {
	flags := func(cmd *cobra.Command) *cobra.Command {
		strFlag(cmd, "state", "state.json path")
		strFlag(cmd, "reason", "why (decide)")
		strFlag(cmd, "by", "instructor|user (decide)")
		boolFlag(cmd, "json", "JSON output")
		return withPrefix(cmd, "approvals")
	}
	cmd := flags(&cobra.Command{
		Use:   "approvals",
		Short: "pending network, path and dialog approvals of a wave",
		Args:  cobra.ArbitraryArgs,
		RunE:  notImplemented("approvals"),
	})
	cmd.AddCommand(flags(&cobra.Command{
		Use:   "decide ITEM allow|deny",
		Short: "apply one decision and resume the affected package",
		Args:  cobra.ArbitraryArgs,
		RunE:  notImplemented("approvals decide"),
	}))
	return cmd
}
