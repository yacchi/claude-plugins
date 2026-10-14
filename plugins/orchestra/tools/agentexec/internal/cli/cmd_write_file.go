package cli

import (
	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/dispatch"
)

func newWriteFileCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "write-file",
		Short: "write stdin atomically to --path, optionally checking its length",
		Args:  cobra.NoArgs,
	}
	path := cmd.Flags().String("path", "", "destination file")
	expect := cmd.Flags().Int("expect-chars", 0, "expected Unicode code points (one trailing newline excluded)")
	_ = cmd.MarkFlagRequired("path")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		expected := dispatch.NoExpect
		if cmd.Flags().Changed("expect-chars") {
			expected = *expect
		}
		out, code := dispatch.WriteFile(*path, expected, cmd.InOrStdin())
		if code == 2 {
			return exitf(2, "write-file: failed to write %s: %s", out["path"], out["error"])
		}
		if err := writeJSONLine(cmd.OutOrStdout(), out); err != nil {
			return err
		}
		if code != 0 {
			return &ExitError{Code: code}
		}
		return nil
	}
	return cmd
}
