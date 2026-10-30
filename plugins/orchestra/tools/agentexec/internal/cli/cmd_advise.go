package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/advise"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
)

// adviseOutput is the JSON `advise --json` prints. Advice is never null.
type adviseOutput struct {
	Advice []advise.Advice `json:"advice"`
}

func newAdviseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "advise [--json]",
		Short: "print permanent repo fixes for settings that only cost something under parallel development",
		Args:  cobra.NoArgs,
	}
	asJSON := cmd.Flags().Bool("json", false, "print {\"advice\":[...]} instead of text")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		// The repo is the working directory's; outside a repository there is no advice.
		root, _ := gitx.RepoRoot("")
		list := advise.Repo(root)
		if *asJSON {
			return writeJSONLine(cmd.OutOrStdout(), adviseOutput{Advice: list})
		}
		writeAdviseText(cmd.OutOrStdout(), list)
		return nil
	}
	return cmd
}

// writeAdviseText prints one block per advice, or "no advice" when there is none.
func writeAdviseText(w io.Writer, list []advise.Advice) {
	if len(list) == 0 {
		fmt.Fprintln(w, "no advice")
		return
	}
	for i, a := range list {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s: %s\n  why: %s\n  fix: %s\n", a.Path, a.Problem, a.Why, a.Fix)
	}
}
