package cli

import (
	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/dispatch"
)

func newWriteFileCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "write-file",
		Short: "write stdin atomically to --path, optionally checking its length",
		Args:  noArgs,
	}
	path := strFlag(cmd, "path", "destination file")
	expect := strFlag(cmd, "expect-chars", "expected Unicode code points (one trailing newline excluded)")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		expected := dispatch.NoExpect
		if v, ok := expect.Get(); ok {
			n, valid := pyInt(v)
			if !valid || n == dispatch.NoExpect {
				return exitf(2, "write-file: --expect-chars must be an integer")
			}
			expected = n
		}
		p, ok := path.Get()
		if !ok {
			return exitf(2, "write-file: missing required option: --path")
		}
		out, code := dispatch.WriteFile(p, expected, cmd.InOrStdin())
		if code == 2 {
			reason := out["error"]
			target := out["path"]
			return exitf(2, "write-file: failed to write %s: %s", target, reason)
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
