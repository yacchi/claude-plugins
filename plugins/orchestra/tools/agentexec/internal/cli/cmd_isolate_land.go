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
// The source is either an assembly (--task, or --request, which is the same
// thing: a request's assembly is named by its request id), the tree integration
// trees are merged into with `isolate merge`, or any other linked worktree of
// the repository (--path), such as an Agent-tool tree. Without a selector, land
// takes the calling session's request's assembly for this repository, else the
// only open request that has one; --request picks among several.
func newIsolateLandCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "land",
		Short: "land the request's assembly (or a worktree) onto the checked-out branch as one commit of yours",
		Args:  cobra.NoArgs,
	}
	fl := cmd.Flags()
	task := fl.String("task", "", "assembly task id (default: the request's assembly for this repository)")
	request := fl.String("request", "", "request id: land that request's assembly (the same as --task <id>)")
	path := fl.String("path", "", "linked worktree to land, e.g. an Agent-tool tree")
	baseline := fl.String("baseline", "", "ref to measure the --path change from (default: the fork point with the checkout)")
	messages := fl.StringArrayP("message", "m", nil, "commit message paragraph (repeatable)")
	repo := repoFlag(cmd)
	f := formatFlags(cmd, "json", "text")
	_ = cmd.MarkFlagRequired("message")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		selected := 0
		for _, v := range []string{*task, *request, *path} {
			if v != "" {
				selected++
			}
		}
		if selected > 1 {
			return exitf(2, "isolate land: give at most one of --task, --request and --path")
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
			if *request != "" {
				*task = *request
			}
			r, err = landAssembly(*repo, *task, message)
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

// landAssembly lands the named assembly, or with no task the one
// isolate.AssemblyFor picks. A lookup that cannot choose is a result, not a
// usage error, so the caller gets the same JSON shape as any refusal.
func landAssembly(repo, task, message string) (isolate.LandResult, error) {
	if task == "" {
		chosen, candidates, err := isolate.AssemblyFor(repo)
		switch {
		case err != nil:
			return isolate.LandResult{Status: "error", Reason: err.Error()}, nil
		case chosen != "":
			task = chosen
		case len(candidates) == 0:
			return isolate.LandResult{Status: "absent", Reason: "no open request has an assembly for this repository"}, nil
		default:
			return isolate.LandResult{Status: "refused", Reason: fmt.Sprintf(
				"several requests have an assembly for this repository (%s); pick one with --request <id>",
				strings.Join(candidates, ", "))}, nil
		}
	}
	return isolate.Land(repo, task, message)
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
			r.Commit[:min(8, len(r.Commit))], r.Branch, len(r.Files), source) + landNote(r)
	case "empty":
		if r.Path != "" {
			return fmt.Sprintf("nothing to land: %s has no change", source)
		}
		return fmt.Sprintf("nothing to land: %s has no commits past its base", source) + landNote(r)
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

// landNote says what landing an assembly did to the assembly and its request.
// A --path land has neither, so it gets no note.
func landNote(r isolate.LandResult) string {
	if r.Path != "" {
		return ""
	}
	note := ""
	switch {
	case r.Removed:
		note += "; assembly removed"
	case r.Reason != "":
		note += "; assembly kept: " + r.Reason
	}
	if r.RequestEnded {
		note += fmt.Sprintf("; request %s ended", r.Request)
	}
	return note
}
