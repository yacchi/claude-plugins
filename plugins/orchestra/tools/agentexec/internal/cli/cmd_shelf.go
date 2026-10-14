package cli

import (
	"strconv"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/shelf"
)

func newShelfCmd() *cobra.Command {
	cmd := groupCommand("shelf", "a per-worktree `git stash`")

	push := &cobra.Command{
		Use:   "push [-- PATH...]",
		Short: "set changes aside (-- PATH... limits them)",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && cmd.ArgsLenAtDash() != 0 {
				return exitf(2, "shelf push: paths go after --: %s", args[0])
			}
			return nil
		},
	}
	pushRepo := repoFlag(push)
	message := push.Flags().StringP("message", "m", "", "entry message")
	keepIndex := push.Flags().Bool("keep-index", false, "leave staged changes in place")
	allowMain := push.Flags().Bool("allow-main-tree", false, "shelve in the main working tree")
	push.RunE = func(cmd *cobra.Command, args []string) error {
		root, err := shelfRoot(cmd, *pushRepo)
		if err != nil {
			return err
		}
		// The main tree may hold other tasks' and the user's uncommitted
		// work; shelving it would make those files vanish from under them.
		if !shelf.IsLinkedWorktree(root) && !*allowMain {
			if err := writeJSONLine(cmd.OutOrStdout(), map[string]string{"status": "error", "reason": "refusing to shelve in the main working tree, which other tasks and the user share; " +
				"work in your own worktree, or pass --allow-main-tree if you are alone in it"}); err != nil {
				return err
			}
			return &ExitError{Code: 3}
		}
		res, err := shelf.Push(root, args, *message, *keepIndex)
		return emitShelf(cmd, res, err)
	}
	cmd.AddCommand(push)

	for _, sub := range []struct{ name, short string }{
		{"pop", "bring an entry back and drop it"},
		{"apply", "bring an entry back and keep it"},
		{"drop", "delete an entry"},
	} {
		name := sub.name
		c := &cobra.Command{
			Use:   name + " [ID]",
			Short: sub.short,
			Args: func(cmd *cobra.Command, args []string) error {
				if err := cobra.MaximumNArgs(1)(cmd, args); err != nil {
					return err
				}
				if len(args) == 1 {
					if _, err := strconv.ParseUint(args[0], 10, 0); err != nil {
						return exitf(2, "shelf %s: entry id must be a number: %s", name, args[0])
					}
				}
				return nil
			},
		}
		repo := repoFlag(c)
		c.RunE = func(cmd *cobra.Command, args []string) error {
			root, err := shelfRoot(cmd, *repo)
			if err != nil {
				return err
			}
			id := ""
			if len(args) == 1 {
				id = args[0]
			}
			var res shelf.Result
			if name == "drop" {
				res, err = shelf.Drop(root, id)
			} else {
				res, err = shelf.Apply(root, id, name == "pop")
			}
			return emitShelf(cmd, res, err)
		}
		cmd.AddCommand(c)
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "list entries",
		Args:  cobra.NoArgs,
	}
	listRepo := repoFlag(list)
	list.RunE = func(cmd *cobra.Command, _ []string) error {
		root, err := shelfRoot(cmd, *listRepo)
		if err != nil {
			return err
		}
		entries, _ := shelf.List(root)
		return writeJSONLine(cmd.OutOrStdout(), map[string]any{"status": "ok", "entries": entries})
	}
	cmd.AddCommand(list)
	return cmd
}

// shelfRoot is the repository root of dir; outside a repository it prints
// the error result and fails with exit 3.
func shelfRoot(cmd *cobra.Command, dir string) (string, error) {
	root, ok := gitx.RepoRoot(dir)
	if ok {
		return root, nil
	}
	if err := writeJSONLine(cmd.OutOrStdout(), map[string]string{"status": "error", "reason": "not a git repository"}); err != nil {
		return "", err
	}
	return "", &ExitError{Code: 3}
}

// emitShelf prints an operation's result and maps its status to the exit code.
func emitShelf(cmd *cobra.Command, res shelf.Result, err error) error {
	if err != nil {
		return &ExitError{Code: 3, Msg: cmdName(cmd) + ": " + err.Error()}
	}
	if err := writeJSONLine(cmd.OutOrStdout(), res); err != nil {
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
