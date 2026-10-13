package cli

import "github.com/spf13/cobra"

func newWriteFileCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "write-file",
		Short: "write stdin atomically to --path, optionally checking its length",
		Args:  noArgs,
		RunE:  notImplemented("write-file"),
	}
	strFlag(cmd, "path", "destination file")
	strFlag(cmd, "expect-chars", "expected Unicode code points (one trailing newline excluded)")
	return cmd
}
