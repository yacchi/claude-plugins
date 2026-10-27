package cli

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
)

// newIsolateMergeCmd builds `isolate merge`, how an integration tree completes:
// its integrated children are reaped first, and a tree that still has children
// is refused, so nothing it holds is lost with it.
func newIsolateMergeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "merge",
		Short: "squash a finished integration tree into its assembly as one commit",
		Args:  cobra.NoArgs,
	}
	fl := cmd.Flags()
	task := fl.String("task", "", "integration task id")
	messages := fl.StringArrayP("message", "m", nil, "commit message paragraph (repeatable)")
	repo := repoFlag(cmd)
	f := formatFlags(cmd, "json", "text")
	_ = cmd.MarkFlagRequired("task")
	_ = cmd.MarkFlagRequired("message")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		// Paragraphs are joined like `git commit -m`: a blank line between them.
		message := strings.Join(*messages, "\n\n")
		r, err := isolate.Merge(*repo, *task, message)
		if err != nil {
			return usageErr(cmd, err)
		}
		if err := writeResult(cmd.OutOrStdout(), f.is("text"), r, func() string {
			return isolate.FormatMergeText(r)
		}); err != nil {
			return err
		}
		switch r.Status {
		case "merged", "empty":
			return nil
		}
		return &ExitError{Code: 1}
	}
	return cmd
}
