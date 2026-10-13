package cli

import (
	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "print the resolved orchestra config (defaults + user + project + local layers)",
		Args:  noArgs,
	}
	boolFlag(cmd, "json", "JSON output (the default and only format)")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		out := cfg.Values.Clone()
		// Legacy-config warnings were removed with the legacy compatibility
		// layer; only a learned-grants problem is still reported.
		warnings := []any{}
		out.Set("warnings", warnings)
		learned, warning := learnedGrants()
		if sb := out.Object("sandbox"); sb != nil {
			sb.Set("learned", learned)
		}
		if warning != "" {
			out.Set("warnings", append(warnings,
				jsonx.NewObject().Set("type", "sandbox_learned").Set("detail", warning)))
		}
		return writeJSON(cmd.OutOrStdout(), out)
	}
	return cmd
}
