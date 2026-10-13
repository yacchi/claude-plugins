package cli

import "github.com/spf13/cobra"

func newCheckCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check",
		Short: "run the config-driven checks over a tree's changed files",
		Args:  noArgs,
		RunE:  notImplemented("check"),
	}
	strFlag(cmd, "task", "task id (its worktree)")
	strFlag(cmd, "path", "an arbitrary directory")
	strFlag(cmd, "since", "ref the changed files are measured from (default HEAD)")
	strFlag(cmd, "files", "comma-separated changed-file list override")
	strFlag(cmd, "repo", "repository path (default cwd)")
	strFlag(cmd, "session", "session id")
	boolFlag(cmd, "all", "do not stop at the first failure")
	boolFlag(cmd, "baseline", "re-run failures at the baseline to spot preexisting breakage")
	boolFlag(cmd, "no-retry", "do not retry failed checks alone")
	formatFlags(cmd, "json", "text")
	return cmd
}
