package cli

import "github.com/spf13/cobra"

// touchHeartbeat records that this session is orchestrating (see the root's
// PersistentPreRun). The heartbeat store belongs to the isolate package.
func touchHeartbeat() {}

func addIsolateCoreCommands(parent *cobra.Command) {
	parent.AddCommand(
		newIsolateSimpleCmd("create", "create (or return) the task's worktree"),
		newIsolateSimpleCmd("list", "orchestra-created worktrees in this repo"),
		newIsolateSimpleCmd("diff", "what the worker changed, measured from the worktree's baseline"),
		newIsolateSimpleCmd("remove", "delete the worktree and branch (refuses uncollected changes)"),
		newIsolateCollectCmd(),
		newIsolateSweepCmd(),
	)
}

// newIsolateSimpleCmd builds create/list/diff/remove, which share one flag
// set in the Python parser (each rejects the flags it cannot use itself).
func newIsolateSimpleCmd(name, short string) *cobra.Command {
	cmd := withPrefix(&cobra.Command{
		Use:   name,
		Short: short,
		Args:  noArgs,
		RunE:  notImplemented("isolate " + name),
	}, "isolate")
	strFlag(cmd, "task", "task id")
	strFlag(cmd, "repo", "repository path (default cwd)")
	strFlag(cmd, "backend", "auto|gtr|git")
	strFlag(cmd, "session", "session id")
	boolFlag(cmd, "no-carry", "do not carry uncommitted work and dependency dirs")
	boolFlag(cmd, "force", "remove even with uncollected changes")
	boolFlag(cmd, "names-only", "omit the patch text")
	boolFlag(cmd, "json", "JSON output (the default)")
	return cmd
}

func newIsolateCollectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "collect",
		Short: "mark a task's (or a session's) worktree changes as collected",
		Args:  noArgs,
		RunE:  notImplemented("isolate collect"),
	}
	strFlag(cmd, "task", "task id")
	strFlag(cmd, "session", "session id")
	strFlag(cmd, "repo", "repository path (default cwd)")
	formatFlags(cmd, "json", "text")
	return cmd
}

func newIsolateSweepCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sweep",
		Short: "reclaim leftover worktrees repo-wide",
		Args:  noArgs,
		RunE:  notImplemented("isolate sweep"),
	}
	strFlag(cmd, "repo", "repository path (default cwd)")
	strFlag(cmd, "older-than", "only worktrees older than DAYS")
	strFlag(cmd, "live-window", "minutes a session counts as live (default 120)")
	boolFlag(cmd, "include-current", "include the running session's worktrees")
	boolFlag(cmd, "include-live", "include sessions seen within the live window")
	boolFlag(cmd, "branches", "also delete orphaned orchestra branches")
	boolFlag(cmd, "dry-run", "report only")
	boolFlag(cmd, "force", "remove even with uncollected changes")
	formatFlags(cmd, "json", "text")
	return cmd
}
