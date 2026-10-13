package cli

import "github.com/spf13/cobra"

func newDispatchCmd() *cobra.Command {
	cmd := withPrefix(&cobra.Command{
		Use:   "dispatch",
		Short: "one-call resolve + dispatch (or, with --executor, a direct run)",
		Args:  noArgs,
		RunE:  notImplemented("dispatch"),
	}, "dispatch")
	strFlag(cmd, "class", "capability class")
	strFlag(cmd, "archetype", "task archetype")
	strFlag(cmd, "exhausted", "comma-separated executors already tried")
	listFlag(cmd, "prompt-file", "prompt file (repeatable)")
	strFlag(cmd, "workdir", "working directory of the worker")
	strFlag(cmd, "isolate", "auto|always|never")
	strFlag(cmd, "task", "task id (names the worktree and the resumable session)")
	strFlag(cmd, "run-id", "run ledger id")
	strFlag(cmd, "resume", "session id to resume")
	boolFlag(cmd, "no-resume", "start cold even when the task has a session")
	boolFlag(cmd, "no-cooldown", "ignore executor cooldowns")
	strFlag(cmd, "token", "dispatch token from `dispatch prepare`")
	boolFlag(cmd, "capture", "accepted for compatibility; dispatch always captures")
	boolFlag(cmd, "detach", "run as a detached background child")
	boolFlag(cmd, "summary", "print one compact line with the answer cut to 400 characters")
	strFlag(cmd, "executor", "direct mode: run this executor without routing")
	strFlag(cmd, "model", "direct mode: model")
	strFlag(cmd, "effort", "direct mode: effort")
	cmd.AddCommand(newDispatchPrepareCmd(), newDispatchWaitCmd())
	return cmd
}

func newDispatchPrepareCmd() *cobra.Command {
	cmd := withPrefix(&cobra.Command{
		Use:   "prepare",
		Short: "mint a dispatch token for the run",
		Args:  noArgs,
		RunE:  notImplemented("dispatch prepare"),
	}, "dispatch")
	strFlag(cmd, "class", "capability class")
	listFlag(cmd, "prompt-file", "prompt file (repeatable; may not exist yet)")
	strFlag(cmd, "workdir", "working directory of the worker")
	strFlag(cmd, "archetype", "task archetype")
	strFlag(cmd, "isolate", "auto|always|never")
	strFlag(cmd, "task", "task id")
	strFlag(cmd, "run-id", "run ledger id")
	boolFlag(cmd, "json", "JSON output")
	return cmd
}

func newDispatchWaitCmd() *cobra.Command {
	cmd := withPrefix(&cobra.Command{
		Use:   "wait",
		Short: "poll a --detach'd run",
		Args:  noArgs,
		RunE:  notImplemented("dispatch wait"),
	}, "dispatch")
	strFlag(cmd, "token", "dispatch token")
	strFlag(cmd, "max-wait", "seconds to poll (default 560)")
	boolFlag(cmd, "summary", "print one compact line with the answer cut to 400 characters")
	return cmd
}
