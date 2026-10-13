package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/doctor"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/tools"
)

func newDoctorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:                "doctor",
		Short:              "structured readiness report (--json is the default)",
		Args:               cobra.ArbitraryArgs,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(cmd.OutOrStdout(), args)
		},
	}
	// Declared for --help only; DisableFlagParsing means they are never
	// parsed (the last of --json/--text wins, like the Python parser).
	formatFlags(cmd, "json", "text")
	return cmd
}

func runDoctor(w io.Writer, args []string) error {
	format := "--json"
	for _, a := range args {
		if a != "--json" && a != "--text" {
			return exitf(2, "doctor: unknown option: %s", a)
		}
		format = a
	}

	cfg, err := config.Resolve()
	if err == nil {
		tools.MarkAvailability(cfg)
	}
	report := doctor.Build(cfg, err)
	if format == "--text" {
		_, werr := fmt.Fprint(w, doctor.FormatText(report))
		return werr
	}
	return writeJSON(w, report.Object())
}
