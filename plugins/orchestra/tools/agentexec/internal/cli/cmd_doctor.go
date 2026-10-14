package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/doctor"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/tools"
)

func newDoctorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "structured readiness report (--json is the default)",
		Args:  cobra.NoArgs,
	}
	format := formatFlags(cmd, "json", "text")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		cfg, err := config.Resolve()
		if err == nil {
			tools.MarkAvailability(cfg)
		}
		report := doctor.Build(cfg, err)
		if format.is("text") {
			_, werr := fmt.Fprint(cmd.OutOrStdout(), doctor.FormatText(report))
			return werr
		}
		return writeJSON(cmd.OutOrStdout(), report.Object())
	}
	return cmd
}
