package cli

import "github.com/spf13/cobra"

func newShelfCmd() *cobra.Command {
	shelf := groupCommand("shelf", "a per-worktree `git stash`")
	for _, sub := range []struct{ name, short string }{
		{"push", "set changes aside (-- PATH... limits them)"},
		{"pop", "bring an entry back and drop it"},
		{"apply", "bring an entry back and keep it"},
		{"drop", "delete an entry"},
		{"list", "list entries"},
	} {
		cmd := withPrefix(&cobra.Command{
			Use:   sub.name,
			Short: sub.short,
			Args:  cobra.ArbitraryArgs,
			RunE:  notImplemented("shelf " + sub.name),
		}, "shelf")
		strFlag(cmd, "repo", "repository path (default cwd)")
		shortStrFlag(cmd, "message", "m", "entry message")
		boolFlag(cmd, "allow-main-tree", "allow shelving in the main working tree")
		if sub.name == "push" {
			boolFlag(cmd, "keep-index", "leave the staged state in the tree")
		}
		shelf.AddCommand(cmd)
	}
	return shelf
}
