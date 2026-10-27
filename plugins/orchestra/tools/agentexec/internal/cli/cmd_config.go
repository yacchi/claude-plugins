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
		out["warnings"] = warnings
		return writeJSON(cmd.OutOrStdout(), out)
	}
	return cmd
}
