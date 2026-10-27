package cli

import (
	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "print the resolved orchestra config (defaults + user + project + local layers)",
		Args:  cobra.NoArgs,
	}
	cmd.Flags().Bool("json", false, "JSON output (the default and only format)")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		out := config.CloneValue(cfg.Values).(map[string]any)
		// Legacy-config warnings were removed with the legacy compatibility
		// layer; only a learned-grants problem is still reported.
		warnings := []any{}
		learned, warning := learnedGrants()
		if sb := config.ObjectAt(out, "sandbox"); sb != nil {
			sb["learned"] = learned
		}
		if warning != "" {
			warnings = append(warnings,
				map[string]any{"type": "sandbox_learned", "detail": warning})
		}
		for _, w := range config.DashboardWarnings(cfg.Values) {
			warnings = append(warnings, map[string]any{"type": "dashboard_artifact_url", "detail": w})
		}
		out["warnings"] = warnings
		// artifact_url is resolved for the signed-in organization so readers
		// never get an Artifact URL that another organization cannot write.
		uuid, name := config.CurrentOrg()
		dash := config.ObjectAt(out, "dashboard")
		if dash == nil {
			dash = map[string]any{}
			out["dashboard"] = dash
		}
		dash["org"] = uuid
		dash["org_name"] = name
		dash["artifact_url"] = config.DashboardURL(cfg.Values, uuid)
		return writeJSON(cmd.OutOrStdout(), out)
	}
	return cmd
}
