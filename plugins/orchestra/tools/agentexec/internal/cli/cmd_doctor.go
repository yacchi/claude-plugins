package cli

import "github.com/spf13/cobra"

func newDoctorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "structured readiness report (--json is the default)",
		Args:  noArgs,
		RunE:  notImplemented("doctor"),
	}
	formatFlags(cmd, "json", "text")
	return cmd
}
