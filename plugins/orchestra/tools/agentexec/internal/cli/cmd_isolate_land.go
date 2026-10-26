package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
)

// newIsolateLandCmd builds `isolate land`, the one sanctioned way to take a
// run's work into the checked-out branch. Doing it by hand (a merge of the
// integration branch, or of an Agent-tool worktree's branch) skips the checks
// Land makes, such as the dirty-checkout and excluded-path refusals.
//
// The source is either an integration task (--task) or any other linked
// worktree of the repository (--path), such as an Agent-tool tree.
func newIsolateLandCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "land",
		Short: "squash an integration task or a worktree onto the checked-out branch as one commit of yours",
		Args:  cobra.NoArgs,
	}
	fl := cmd.Flags()
	task := fl.String("task", "", "integration task id")
	path := fl.String("path", "", "linked worktree to land, e.g. an Agent-tool tree")
	baseline := fl.String("baseline", "", "ref to measure the --path change from (default: the fork point with the checkout)")
	messages := fl.StringArrayP("message", "m", nil, "commit message paragraph (repeatable)")
	repo := repoFlag(cmd)
	f := formatFlags(cmd, "json", "text")
	_ = cmd.MarkFlagRequired("message")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if (*task == "") == (*path == "") {
			return exitf(2, "isolate land: give exactly one of --task and --path")
		}
		if *baseline != "" && *path == "" {
			return exitf(2, "isolate land: --baseline goes with --path")
		}
		// Paragraphs are joined like `git commit -m`: a blank line between them.
		message := strings.Join(*messages, "\n\n")
		var r isolate.LandResult
		var err error
		if *path != "" {
			r, err = isolate.LandPath(*repo, *path, *baseline, message)
		} else {
			r, err = isolate.Land(*repo, *task, message)
		}
		if err != nil {
			return usageErr(cmd, err)
		}
		if err := writeResult(cmd.OutOrStdout(), f.is("text"), r, func() string {
			return landRender(r)
		}); err != nil {
			return err
		}
		switch r.Status {
		case "landed", "empty":
			return nil
		}
		return &ExitError{Code: 1}
	}
	return cmd
}

// landRender is the one-line text form of a land result. It names the source
// the way the result was asked for: the path for --path, the task otherwise.
func landRender(r isolate.LandResult) string {
	source := r.Task
	if r.Path != "" {
		source = r.Path
	}
	switch r.Status {
	case "landed":
		return fmt.Sprintf("landed %s on %s: %d files (from %s)",
			r.Commit[:min(8, len(r.Commit))], r.Branch, len(r.Files), source)
	case "empty":
		if r.Path != "" {
			return fmt.Sprintf("nothing to land: %s has no change", source)
		}
		return fmt.Sprintf("nothing to land: %s has no commits past its base", source)
	}
	line := r.Status
	if r.Reason != "" {
		line += ": " + r.Reason
	}
	if len(r.Files) > 0 {
		shown, more := r.Files, ""
		if len(shown) > 5 {
			shown, more = shown[:5], ", ..."
		}
		line += ": " + strings.Join(shown, ", ") + more
	}
	return line
}
