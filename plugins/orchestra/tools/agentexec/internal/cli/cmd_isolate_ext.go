package cli

import "github.com/spf13/cobra"

func addIsolateExtCommands(parent *cobra.Command) {
	parent.AddCommand(
		newIsolateIntegrateCmd(),
		newIsolateRefreshCmd(),
		newIsolateAdoptCmd("adopt", "register a worktree created outside orchestra as the task's"),
		newIsolateAdoptCmd("unadopt", "drop an adoption (markers only)"),
	)
}

func newIsolateIntegrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "integrate",
		Short: "replay each task's diff onto one integration worktree",
		Args:  noArgs,
		RunE:  notImplemented("isolate integrate"),
	}
	strFlag(cmd, "tasks", "comma-separated task ids, in order")
	strFlag(cmd, "repo", "repository path (default cwd)")
	strFlag(cmd, "onto", "start ref")
	strFlag(cmd, "into", "integration task id (default integrate)")
	strFlag(cmd, "on-conflict", "keep|skip|stop")
	strFlag(cmd, "verify", "command run in the integration worktree")
	strFlag(cmd, "verify-timeout", "seconds (default 3600)")
	boolFlag(cmd, "bisect", "bisect a verify failure and revert the culprit")
	strFlag(cmd, "bisect-max", "bisect rounds (default 3)")
	strFlag(cmd, "progress-jsonl", "append progress events to this file")
	strFlag(cmd, "lock", "wait|fail")
	strFlag(cmd, "lock-timeout", "seconds (default 900)")
	formatFlags(cmd, "json", "text")
	return cmd
}

func newIsolateRefreshCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "refresh",
		Short: "move a task worktree's changes onto a newer base",
		Args:  noArgs,
		RunE:  notImplemented("isolate refresh"),
	}
	strFlag(cmd, "task", "task id")
	strFlag(cmd, "onto", "new base ref (default HEAD)")
	strFlag(cmd, "repo", "repository path (default cwd)")
	strFlag(cmd, "session", "session id")
	formatFlags(cmd, "json", "text")
	return cmd
}

// newIsolateAdoptCmd builds adopt/unadopt, which share one parser in Python
// (--no-carry is accepted by both).
func newIsolateAdoptCmd(name, short string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   name,
		Short: short,
		Args:  noArgs,
		RunE:  notImplemented("isolate " + name),
	}
	strFlag(cmd, "task", "task id")
	if name == "adopt" {
		strFlag(cmd, "path", "the worktree to adopt")
		strFlag(cmd, "baseline", "baseline ref")
	}
	strFlag(cmd, "repo", "repository path (default cwd)")
	strFlag(cmd, "session", "session id")
	boolFlag(cmd, "no-carry", "do not carry dependency dirs and local files")
	formatFlags(cmd, "json", "text")
	return cmd
}
