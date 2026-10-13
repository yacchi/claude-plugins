package cli

import "github.com/spf13/cobra"

func newRouteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "route",
		Short: "resolve a capability class to a concrete executor",
		Args:  noArgs,
		RunE:  notImplemented("route"),
	}
	strFlag(cmd, "class", "light|standard|deep|review|independent-review")
	strFlag(cmd, "archetype", "task archetype (default|investigation)")
	strFlag(cmd, "exhausted", "comma-separated executors already tried")
	boolFlag(cmd, "no-cooldown", "ignore executor cooldowns")
	formatFlags(cmd, "json", "text")
	return cmd
}
