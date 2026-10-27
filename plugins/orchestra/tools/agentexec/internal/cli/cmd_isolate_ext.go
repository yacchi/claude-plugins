package cli

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/integrate"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
)

func addIsolateExtCommands(parent *cobra.Command) {
	parent.AddCommand(
		newIsolateIntegrateCmd(),
		newIsolateRefreshCmd(),
		newIsolateLandCmd(),
		newIsolateAdoptCmd("adopt", "register a worktree created outside orchestra as the task's"),
		newIsolateAdoptCmd("unadopt", "drop an adoption (markers only)"),
	)
}

// progressFile appends each progress line to path, creating its directory
// first. The file is reopened per event, so every line is on disk as soon as
// it is emitted and no handle is held across a long integration.
type progressFile struct{ path string }

func (p progressFile) Write(b []byte) (int, error) {
	abs, err := filepath.Abs(p.path)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(p.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return 0, err
	}
	n, werr := f.Write(b)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return n, werr
}

func newIsolateIntegrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "integrate",
		Short: "replay each task's diff onto one integration worktree",
		Args:  cobra.NoArgs,
	}
	fl := cmd.Flags()
	tasks := fl.String("tasks", "", "comma-separated task ids, in order")
	repo := repoFlag(cmd)
	var o integrate.Options
	fl.String("onto", "", "removed: see the error")
	_ = fl.MarkHidden("onto")
	fl.StringVar(&o.Into, "into", integrate.DefaultInto, "integration task id")
	fl.StringVar(&o.Uncommitted, "uncommitted", "",
		"include|ignore: what the request's assembly does with your uncommitted changes when this integrate creates it")
	fl.StringVar(&o.OnConflict, "on-conflict", integrate.OnConflictKeep, "keep|skip|stop")
	fl.StringVar(&o.Verify, "verify", "", "command run in the integration worktree")
	verifyTimeout := fl.Int("verify-timeout", 3600, "seconds")
	fl.BoolVar(&o.Bisect, "bisect", false, "bisect a verify failure and revert the culprit")
	fl.IntVar(&o.BisectMax, "bisect-max", 3, "bisect rounds")
	progress := fl.String("progress-jsonl", "", "append progress events to this file")
	fl.StringVar(&o.Lock, "lock", integrate.LockWait, "wait|fail")
	lockTimeout := fl.Float64("lock-timeout", 900, "seconds")
	f := formatFlags(cmd, "json", "text")
	_ = cmd.MarkFlagRequired("tasks")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		bad := func(format string, args ...any) error {
			return exitf(2, "isolate integrate: "+format, args...)
		}
		if cmd.Flags().Changed("onto") {
			return exitf(2, "isolate integrate: --onto was removed: an integration tree starts from its request's assembly (see agent-exec isolate request show)")
		}
		switch o.Uncommitted {
		case "", isolate.UncommittedInclude, isolate.UncommittedIgnore:
		default:
			return bad("--uncommitted must be include or ignore, got %q", o.Uncommitted)
		}
		var ids []string
		for _, t := range strings.Split(*tasks, ",") {
			if t = strings.TrimSpace(t); t != "" {
				ids = append(ids, t)
			}
		}
		if len(ids) == 0 {
			return bad("--tasks is empty")
		}
		switch o.OnConflict {
		case integrate.OnConflictKeep, integrate.OnConflictSkip, integrate.OnConflictStop:
		default:
			return bad("invalid --on-conflict value: %s", o.OnConflict)
		}
		if *verifyTimeout <= 0 {
			return bad("--verify-timeout must be positive, got %d", *verifyTimeout)
		}
		o.VerifyTimeout = time.Duration(*verifyTimeout) * time.Second
		if o.BisectMax <= 0 {
			return bad("--bisect-max must be positive, got %d", o.BisectMax)
		}
		if o.Bisect && !cmd.Flags().Changed("verify") {
			return bad("--bisect requires --verify")
		}
		if o.Lock != integrate.LockWait && o.Lock != integrate.LockFail {
			return bad("invalid --lock value: %s", o.Lock)
		}
		if !finiteNonNegative(*lockTimeout) {
			return bad("--lock-timeout must be a non-negative number of seconds, got %g", *lockTimeout)
		}
		o.LockTimeout = duration(*lockTimeout, time.Second)
		if *progress != "" {
			o.Progress = progressFile{path: *progress}
		}

		r, err := integrate.Integrate(*repo, ids, o)
		if err != nil {
			return usageErr(cmd, err)
		}
		if err := writeResult(cmd.OutOrStdout(), f.is("text"), r, func() string {
			return integrate.FormatText(r)
		}); err != nil {
			return err
		}
		if code := integrate.ExitCode(r); code != 0 {
			return &ExitError{Code: code}
		}
		return nil
	}
	return cmd
}

func newIsolateRefreshCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "refresh",
		Short: "move a task worktree's changes onto a newer base",
		Args:  cobra.NoArgs,
	}
	task := cmd.Flags().String("task", "", "task id")
	onto := cmd.Flags().String("onto", "", "new base ref (default HEAD)")
	repo := repoFlag(cmd)
	session := cmd.Flags().String("session", "", "session id")
	f := formatFlags(cmd, "json", "text")
	_ = cmd.MarkFlagRequired("task")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		r, err := isolate.Refresh(*repo, *task, *onto, *session)
		if err != nil {
			return usageErr(cmd, err)
		}
		if err := writeResult(cmd.OutOrStdout(), f.is("text"), r, func() string {
			return isolate.FormatRefreshText(r)
		}); err != nil {
			return err
		}
		switch r.Status {
		case "ok", "unchanged":
			return nil
		case "conflicted":
			return &ExitError{Code: 1}
		}
		return &ExitError{Code: 3}
	}
	return cmd
}

// newIsolateAdoptCmd builds adopt and unadopt.
func newIsolateAdoptCmd(name, short string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   name,
		Short: short,
		Args:  cobra.NoArgs,
	}
	fl := cmd.Flags()
	task := fl.String("task", "", "task id")
	repo := repoFlag(cmd)
	session := fl.String("session", "", "session id")
	var path, parent string
	var o isolate.AdoptOptions
	if name == "adopt" {
		fl.StringVar(&path, "path", "", "the worktree to adopt")
		fl.StringVar(&o.Baseline, "baseline", "", "baseline ref")
		fl.StringVar(&parent, "parent", "", "parent integration tree's task id")
		_ = cmd.MarkFlagRequired("path")
	}
	noCarry := fl.Bool("no-carry", false, "do not carry dependency dirs and local files")
	f := formatFlags(cmd, "json", "text")
	_ = cmd.MarkFlagRequired("task")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		var r isolate.AdoptResult
		var err error
		if name == "unadopt" {
			r, err = isolate.Unadopt(*repo, *task, *session)
		} else {
			o.Session, o.Carry = *session, !*noCarry
			if o.Carry {
				if o.CarryExtra, o.CarryFiles, err = carryConfig(); err != nil {
					return err
				}
			}
			if parent != "" {
				p, _, ok := isolate.ResolveWorktree(*repo, parent, "")
				if !ok {
					return exitf(2, "isolate adopt: --parent %s: no such tree", parent)
				}
				o.Parent = p
			}
			r, err = isolate.Adopt(*repo, *task, absPath(path), o)
		}
		if err != nil {
			return usageErr(cmd, err)
		}
		if err := writeResult(cmd.OutOrStdout(), f.is("text"), r, func() string {
			return isolate.FormatAdoptText(r)
		}); err != nil {
			return err
		}
		if r.Status == "error" {
			return &ExitError{Code: 3}
		}
		return nil
	}
	return cmd
}
