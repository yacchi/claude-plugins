package cli

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
)

// touchHeartbeat records that this session is orchestrating (see the root's
// PersistentPreRun). Without an ORCHESTRA_ALIVE_DIR override the heartbeat
// store follows a relocated run ledger, so the config is consulted.
func touchHeartbeat() {
	if os.Getenv("ORCHESTRA_ALIVE_DIR") == "" {
		if cfg, err := config.Resolve(); err == nil {
			isolate.SetAliveDir(isolate.AliveDirFor(cfg))
		}
	}
	isolate.HeartbeatTouch("")
}

func addIsolateCoreCommands(parent *cobra.Command) {
	parent.AddCommand(
		newIsolateSimpleCmd("create", "create (or return) the task's worktree"),
		newIsolateSimpleCmd("list", "orchestra-created worktrees in this repo"),
		newIsolateSimpleCmd("diff", "what the worker changed, measured from the worktree's baseline"),
		newIsolateSimpleCmd("remove", "delete the worktree and branch (refuses uncollected changes)"),
		newIsolateCollectCmd(),
		newIsolateSweepCmd(),
		newIsolateMergeCmd(),
		newIsolateRequestCmd(),
	)
}

// repoFlag declares --repo; its value defaults to the working directory.
func repoFlag(cmd *cobra.Command) *string {
	wd, err := os.Getwd()
	if err != nil {
		wd = "."
	}
	return cmd.Flags().String("repo", wd, "repository path (default cwd)")
}

// usageErr is the exit-2 report of an input the domain rejected, under the
// command's name.
func usageErr(cmd *cobra.Command, err error) error {
	return exitf(2, "%s: %s", cmdName(cmd), err)
}

// carryConfig fills the carry lists of a create or adopt from the config.
func carryConfig() (extra, files []string, err error) {
	cfg, err := config.Resolve()
	if err != nil {
		return nil, nil, &ExitError{Code: 1, Msg: err.Error()}
	}
	extra = cfg.StringList("isolation", "carry_extra")
	if v, ok := cfg.Lookup("isolation", "carry_files"); ok && v != nil {
		files = nonNilStrings(cfg.StringList("isolation", "carry_files"))
	}
	return extra, files, nil
}

// newIsolateSimpleCmd builds create/list/diff/remove; each takes only the
// flags it uses.
func newIsolateSimpleCmd(name, short string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   name,
		Short: short,
		Args:  cobra.NoArgs,
	}
	fl := cmd.Flags()
	repo := repoFlag(cmd)
	fl.Bool("json", false, "JSON output (the default and only format)")
	var task, session, backend, parent string
	var noCarry, force, namesOnly, noCollect bool
	if name != "list" {
		fl.StringVar(&task, "task", "", "task id")
	}
	if name == "list" || name == "remove" {
		fl.StringVar(&session, "session", "", "session id")
	}
	switch name {
	case "create":
		fl.StringVar(&backend, "backend", isolate.BackendAuto, "auto|gtr|git")
		fl.BoolVar(&noCarry, "no-carry", false, "do not carry uncommitted work and dependency dirs")
		fl.StringVar(&parent, "parent", "",
			"parent integration tree's task id; the new tree starts from the parent's HEAD unless --repo is given")
		_ = cmd.MarkFlagRequired("task")
	case "diff":
		fl.BoolVar(&namesOnly, "names-only", false, "omit the patch text")
		fl.BoolVar(&noCollect, "no-collect", false,
			"report the diff without marking it collected (a mid-run peek; remove and sweep still keep the tree)")
		_ = cmd.MarkFlagRequired("task")
	case "remove":
		fl.BoolVar(&force, "force", false, "remove even with uncollected changes")
		cmd.MarkFlagsOneRequired("task", "session")
		cmd.MarkFlagsMutuallyExclusive("task", "session")
	}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		var result any
		status := ""
		var err error
		switch name {
		case "create":
			switch backend {
			case isolate.BackendAuto, isolate.BackendGtr, isolate.BackendGit:
			default:
				return exitf(2, "isolate create: unknown backend: %s", backend)
			}
			opts := isolate.CreateOptions{Backend: backend, Carry: !noCarry}
			if opts.Carry {
				if opts.CarryExtra, opts.CarryFiles, err = carryConfig(); err != nil {
					return err
				}
			}
			root := *repo
			if parent != "" {
				parentPath, _, ok := isolate.ResolveWorktree(*repo, parent, "")
				if !ok {
					return exitf(2, "isolate create: --parent %s: no such tree", parent)
				}
				opts.Parent = parentPath
				if !cmd.Flags().Changed("repo") {
					// Cut from the parent's HEAD, the integration tree's own state.
					root = parentPath
				}
			}
			var r isolate.CreateResult
			r, err = isolate.Create(root, task, opts)
			result, status = r, r.Status
		case "list":
			var entries []isolate.Entry
			entries, err = isolate.List(*repo, session)
			result = map[string]any{"worktrees": entries}
		case "diff":
			var r isolate.DiffResult
			if noCollect {
				// The patch is only computed when it is printed.
				r, err = isolate.Diff(*repo, task, "", !namesOnly)
			} else {
				r, err = isolate.DiffAndCollect(*repo, task)
			}
			r.OmitPatch = namesOnly
			result, status = r, r.Status
		case "remove":
			var r isolate.RemoveResult
			if cmd.Flags().Changed("session") {
				r, err = isolate.RemoveSession(*repo, session, force)
			} else {
				r, err = isolate.Remove(*repo, task, force, "")
			}
			result, status = r, r.Status
		}
		if err != nil {
			return usageErr(cmd, err)
		}
		if err := writeJSONLine(cmd.OutOrStdout(), result); err != nil {
			return err
		}
		if status == "error" {
			return &ExitError{Code: 1}
		}
		return nil
	}
	return cmd
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func newIsolateCollectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "collect",
		Short: "mark a task's (or a session's) worktree changes as collected",
		Args:  cobra.NoArgs,
	}
	task := cmd.Flags().String("task", "", "task id")
	session := cmd.Flags().String("session", "", "session id")
	repo := repoFlag(cmd)
	f := formatFlags(cmd, "json", "text")
	cmd.MarkFlagsOneRequired("task", "session")
	cmd.MarkFlagsMutuallyExclusive("task", "session")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		var r isolate.CollectResult
		var err error
		if cmd.Flags().Changed("session") {
			r, err = isolate.CollectSession(*repo, *session)
		} else {
			r, err = isolate.Collect(*repo, *task, "")
		}
		if err != nil {
			return usageErr(cmd, err)
		}
		if err := writeResult(cmd.OutOrStdout(), f.is("text"), r, func() string {
			return isolate.FormatCollectText(r)
		}); err != nil {
			return err
		}
		if r.Status == "error" {
			return &ExitError{Code: 1}
		}
		return nil
	}
	return cmd
}

// writeResult prints the --text rendering or the one-line JSON.
func writeResult(w io.Writer, text bool, v any, render func() string) error {
	if text {
		_, err := fmt.Fprintln(w, render())
		return err
	}
	return writeJSONLine(w, v)
}

func newIsolateSweepCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sweep",
		Short: "reclaim leftover worktrees repo-wide",
		Args:  cobra.NoArgs,
	}
	fl := cmd.Flags()
	repo := repoFlag(cmd)
	olderThan := fl.Float64("older-than", 0, "only worktrees older than DAYS")
	liveWindow := fl.Float64("live-window", isolate.DefaultLiveWindow.Minutes(), "minutes a session counts as live")
	var opts isolate.SweepOptions
	fl.BoolVar(&opts.IncludeCurrent, "include-current", false, "include the running session's worktrees")
	fl.BoolVar(&opts.IncludeLive, "include-live", false, "include sessions seen within the live window")
	fl.BoolVar(&opts.Branches, "branches", false, "also delete orphaned orchestra branches")
	fl.BoolVar(&opts.DryRun, "dry-run", false, "report only")
	fl.BoolVar(&opts.Force, "force", false, "remove even with uncollected changes")
	f := formatFlags(cmd, "json", "text")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if cmd.Flags().Changed("older-than") {
			if !finiteNonNegative(*olderThan) {
				return exitf(2, "isolate sweep: --older-than takes a number of days")
			}
			opts.OlderThanDays = olderThan
		}
		if !finiteNonNegative(*liveWindow) {
			return exitf(2, "isolate sweep: --live-window takes a number of minutes")
		}
		opts.LiveWindow = duration(*liveWindow, time.Minute)
		r, err := isolate.Sweep(*repo, opts)
		if err != nil {
			return usageErr(cmd, err)
		}
		if err := writeResult(cmd.OutOrStdout(), f.is("text"), r, func() string {
			return isolate.FormatSweepText(r)
		}); err != nil {
			return err
		}
		if r.Status == "error" || r.Summary.Errors > 0 {
			return &ExitError{Code: 1}
		}
		return nil
	}
	return cmd
}
