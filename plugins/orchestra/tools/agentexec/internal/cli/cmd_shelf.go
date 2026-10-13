package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/shelf"
)

const shelfUsage = "usage: agent-exec shelf push [-m MSG] [--keep-index] [--repo P] [-- PATH...]\n" +
	"       agent-exec shelf pop|apply|drop [ID] [--repo P]\n" +
	"       agent-exec shelf list [--repo P]\n"

// The shelf commands parse their own arguments: the Python parser took a
// bare entry id, everything after "--" as paths, and reported anything else
// as "unknown argument" followed by the usage text, which pflag cannot
// express.
func newShelfCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:                "shelf",
		Short:              "a per-worktree `git stash`",
		Args:               cobra.ArbitraryArgs,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Reached bare or with an unknown subcommand.
			fmt.Fprint(cmd.ErrOrStderr(), shelfUsage)
			return &ExitError{Code: 2}
		},
	}
	for _, sub := range []struct{ name, short string }{
		{"push", "set changes aside (-- PATH... limits them)"},
		{"pop", "bring an entry back and drop it"},
		{"apply", "bring an entry back and keep it"},
		{"drop", "delete an entry"},
		{"list", "list entries"},
	} {
		name := sub.name
		cmd.AddCommand(&cobra.Command{
			Use:                name,
			Short:              sub.short,
			Args:               cobra.ArbitraryArgs,
			DisableFlagParsing: true,
			RunE: func(cmd *cobra.Command, args []string) error {
				return runShelf(cmd.OutOrStdout(), cmd.ErrOrStderr(), name, args)
			},
		})
	}
	return cmd
}

type shelfArgs struct {
	dir, message, id           string
	dirSet, keepIndex, allowMT bool
	paths                      []string
}

func parseShelfArgs(sub string, args []string) (shelfArgs, string) {
	a := shelfArgs{}
	for i := 0; i < len(args); i++ {
		tok := args[i]
		switch {
		case tok == "--":
			a.paths = args[i+1:]
			return a, ""
		case tok == "--repo" || tok == "-m" || tok == "--message":
			if i+1 >= len(args) {
				return a, fmt.Sprintf("agent-exec: shelf: missing value for %s\n", tok)
			}
			if tok == "--repo" {
				a.dir, a.dirSet = args[i+1], true
			} else {
				a.message = args[i+1]
			}
			i++
		case tok == "--keep-index" && sub == "push":
			a.keepIndex = true
		case tok == "--allow-main-tree":
			a.allowMT = true
		case (sub == "pop" || sub == "apply" || sub == "drop") && a.id == "" && isDigits(tok):
			a.id = tok
		default:
			return a, fmt.Sprintf("agent-exec: shelf: unknown argument: %s\n%s", tok, shelfUsage)
		}
	}
	return a, ""
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func runShelf(stdout, stderr io.Writer, sub string, args []string) error {
	a, problem := parseShelfArgs(sub, args)
	if problem != "" {
		fmt.Fprint(stderr, problem)
		return &ExitError{Code: 2}
	}
	if !a.dirSet {
		wd, err := os.Getwd()
		if err != nil {
			wd = "."
		}
		a.dir = wd
	}
	notRepo := map[string]string{"status": "error", "reason": "not a git repository"}
	root, ok := gitx.RepoRoot(a.dir)
	if !ok {
		if err := writeJSONLine(stdout, notRepo); err != nil {
			return err
		}
		return &ExitError{Code: 3}
	}

	var res shelf.Result
	var err error
	switch sub {
	case "list":
		entries, _ := shelf.List(root)
		return writeJSONLine(stdout, map[string]any{"status": "ok", "entries": entries})
	case "push":
		// The main tree may hold other tasks' and the user's uncommitted
		// work; shelving it would make those files vanish from under them.
		if !shelf.IsLinkedWorktree(root) && !a.allowMT {
			if err := writeJSONLine(stdout, map[string]string{"status": "error", "reason": "refusing to shelve in the main working tree, which other tasks and the user share; " +
				"work in your own worktree, or pass --allow-main-tree if you are alone in it"}); err != nil {
				return err
			}
			return &ExitError{Code: 3}
		}
		res, err = shelf.Push(root, a.paths, a.message, a.keepIndex)
	case "drop":
		res, err = shelf.Drop(root, a.id)
	default:
		res, err = shelf.Apply(root, a.id, sub == "pop")
	}
	if err != nil {
		return &ExitError{Code: 3, Msg: "shelf: " + err.Error()}
	}
	if err := writeJSONLine(stdout, res); err != nil {
		return err
	}
	switch res.Status {
	case "conflicted":
		return &ExitError{Code: 1}
	case "ok", "ok-3way", "empty":
		return nil
	}
	return &ExitError{Code: 3}
}
