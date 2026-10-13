package cli

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/integrate"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
)

func addIsolateExtCommands(parent *cobra.Command) {
	parent.AddCommand(
		newIsolateIntegrateCmd(),
		newIsolateRefreshCmd(),
		newIsolateAdoptCmd("adopt", "register a worktree created outside orchestra as the task's"),
		newIsolateAdoptCmd("unadopt", "drop an adoption (markers only)"),
	)
}

// counter is anything that knows how often its flag was given.
type counter interface{ Count() int }

// checkFlags applies the Python parsers' post-parse rules shared by the
// extended isolate commands: no flag twice, not both --json and --text.
func checkFlags(cmd *cobra.Command, f *format, flags map[string]counter, order ...string) error {
	counts := map[string]int{"json": f.flags["json"].Count(), "text": f.flags["text"].Count()}
	for name, c := range flags {
		counts[name] = c.Count()
	}
	if err := duplicate(cmd, counts, append(order, "json", "text")...); err != nil {
		return err
	}
	return f.exclusive(cmd)
}

// pyInt parses a CLI integer the way Python's int() does: surrounding
// whitespace, a sign, and single underscores between digits allowed.
func pyInt(s string) (int, bool) {
	s = trimSpace(s)
	body := strings.TrimLeft(s, "+-")
	if len(s)-len(body) > 1 || body == "" || body[0] == '_' || body[len(body)-1] == '_' ||
		strings.Contains(body, "__") {
		return 0, false
	}
	v, err := strconv.Atoi(s[:len(s)-len(body)] + strings.ReplaceAll(body, "_", ""))
	return v, err == nil
}

// pyFloatStr is Python's str() of a float.
func pyFloatStr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	return jsonx.FormatFloat(f)
}

// secondsDuration converts seconds, saturating instead of overflowing.
func secondsDuration(s float64) time.Duration {
	const maxSeconds = float64(1<<63-1) / float64(time.Second)
	if s >= maxSeconds {
		return time.Duration(1<<63 - 1)
	}
	return time.Duration(s * float64(time.Second))
}

// progressFile appends each progress line to path, creating its directory
// first, as the Python implementation reopened the file per event.
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
		Args:  noArgs,
	}
	tasks := strFlag(cmd, "tasks", "comma-separated task ids, in order")
	repo := strFlag(cmd, "repo", "repository path (default cwd)")
	onto := strFlag(cmd, "onto", "start ref")
	into := strFlag(cmd, "into", "integration task id (default integrate)")
	onConflict := strFlag(cmd, "on-conflict", "keep|skip|stop")
	verify := strFlag(cmd, "verify", "command run in the integration worktree")
	verifyTimeout := strFlag(cmd, "verify-timeout", "seconds (default 3600)")
	bisect := boolFlag(cmd, "bisect", "bisect a verify failure and revert the culprit")
	bisectMax := strFlag(cmd, "bisect-max", "bisect rounds (default 3)")
	progress := strFlag(cmd, "progress-jsonl", "append progress events to this file")
	lock := strFlag(cmd, "lock", "wait|fail")
	lockTimeout := strFlag(cmd, "lock-timeout", "seconds (default 900)")
	f := formatFlags(cmd, "json", "text")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		err := checkFlags(cmd, f, map[string]counter{
			"lock": lock, "lock-timeout": lockTimeout, "tasks": tasks, "repo": repo, "onto": onto,
			"into": into, "on-conflict": onConflict, "verify": verify, "verify-timeout": verifyTimeout,
			"bisect-max": bisectMax, "progress-jsonl": progress, "bisect": bisect,
		}, "lock", "lock-timeout", "tasks", "repo", "onto", "into", "on-conflict", "verify",
			"verify-timeout", "bisect-max", "progress-jsonl", "bisect")
		if err != nil {
			return err
		}
		bad := func(format string, args ...any) error {
			return exitf(2, "isolate integrate: "+format, args...)
		}
		raw, ok := tasks.Get()
		if !ok {
			return bad("missing required option: --tasks")
		}
		var ids []string
		for _, t := range strings.Split(raw, ",") {
			if t = strings.TrimSpace(t); t != "" {
				ids = append(ids, t)
			}
		}
		if len(ids) == 0 {
			return bad("--tasks is empty")
		}
		o := integrate.Options{
			Into:       into.Or(integrate.DefaultInto),
			OnConflict: onConflict.Or(integrate.OnConflictKeep),
			Lock:       lock.Or(integrate.LockWait),
			Bisect:     bisect.On(),
		}
		o.Onto, _ = onto.Get()
		switch o.OnConflict {
		case integrate.OnConflictKeep, integrate.OnConflictSkip, integrate.OnConflictStop:
		default:
			return bad("invalid --on-conflict value: %s", o.OnConflict)
		}
		// A value that parsed but is out of range is reported as Python
		// printed it: converted ("007" -> 7, "-1" -> -1.0 for a float).
		v := verifyTimeout.Or("3600")
		secs, ok := pyInt(v)
		if !ok || secs <= 0 {
			if ok {
				v = strconv.Itoa(secs)
			}
			return bad("invalid --verify-timeout value: %s", v)
		}
		o.VerifyTimeout = secondsDuration(float64(secs))
		v = bisectMax.Or("3")
		if o.BisectMax, ok = pyInt(v); !ok || o.BisectMax <= 0 {
			if ok {
				v = strconv.Itoa(o.BisectMax)
			}
			return bad("invalid --bisect-max value: %s", v)
		}
		verifyCmd, hasVerify := verify.Get()
		if o.Bisect && !hasVerify {
			return bad("--bisect requires --verify")
		}
		o.Verify = verifyCmd
		if o.Lock != integrate.LockWait && o.Lock != integrate.LockFail {
			return bad("invalid --lock value: %s", o.Lock)
		}
		v = lockTimeout.Or("900")
		lt, ok := pyFloat(v)
		if !ok || math.IsInf(lt, 0) || math.IsNaN(lt) || lt < 0 {
			if ok {
				v = pyFloatStr(lt)
			}
			return bad("invalid --lock-timeout value: %s", v)
		}
		o.LockTimeout = secondsDuration(lt)
		if p, _ := progress.Get(); p != "" {
			o.Progress = progressFile{path: p}
		}

		r, err := integrate.Integrate(repoOrCwd(repo), ids, o)
		if err != nil {
			return usageErr("isolate integrate", err)
		}
		if err := writeResult(cmd.OutOrStdout(), f.Given("text"), r, func() string {
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
		Args:  noArgs,
	}
	task := strFlag(cmd, "task", "task id")
	onto := strFlag(cmd, "onto", "new base ref (default HEAD)")
	repo := strFlag(cmd, "repo", "repository path (default cwd)")
	session := strFlag(cmd, "session", "session id")
	f := formatFlags(cmd, "json", "text")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if err := checkFlags(cmd, f, map[string]counter{"task": task, "onto": onto, "repo": repo, "session": session},
			"task", "onto", "repo", "session"); err != nil {
			return err
		}
		taskID, ok := task.Get()
		if !ok {
			return exitf(2, "isolate refresh: missing required option: --task")
		}
		ontoRef, _ := onto.Get()
		sess, _ := session.Get()
		r, err := isolate.Refresh(repoOrCwd(repo), taskID, ontoRef, sess)
		if err != nil {
			return usageErr("isolate refresh", err)
		}
		if err := writeResult(cmd.OutOrStdout(), f.Given("text"), r, func() string {
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

// newIsolateAdoptCmd builds adopt/unadopt, which share one parser in Python
// (--no-carry is accepted by both).
func newIsolateAdoptCmd(name, short string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   name,
		Short: short,
		Args:  noArgs,
	}
	task := strFlag(cmd, "task", "task id")
	flags := map[string]counter{"task": task}
	order := []string{"task"}
	var path, baseline *optString
	if name == "adopt" {
		path = strFlag(cmd, "path", "the worktree to adopt")
		baseline = strFlag(cmd, "baseline", "baseline ref")
		flags["path"], flags["baseline"] = path, baseline
		order = append(order, "path", "baseline")
	}
	repo := strFlag(cmd, "repo", "repository path (default cwd)")
	session := strFlag(cmd, "session", "session id")
	noCarry := boolFlag(cmd, "no-carry", "do not carry dependency dirs and local files")
	flags["repo"], flags["session"], flags["no-carry"] = repo, session, noCarry
	order = append(order, "repo", "session", "no-carry")
	f := formatFlags(cmd, "json", "text")
	prefix := "isolate " + name
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if err := checkFlags(cmd, f, flags, order...); err != nil {
			return err
		}
		taskID, ok := task.Get()
		if !ok {
			return exitf(2, "%s: missing required option: --task", prefix)
		}
		sess, _ := session.Get()
		dir := repoOrCwd(repo)
		var r isolate.AdoptResult
		var err error
		if name == "unadopt" {
			r, err = isolate.Unadopt(dir, taskID, sess)
		} else {
			target, ok := path.Get()
			if !ok {
				return exitf(2, "%s: missing required option: --path", prefix)
			}
			o := isolate.AdoptOptions{Session: sess, Carry: !noCarry.On()}
			o.Baseline, _ = baseline.Get()
			if o.Carry {
				cfg, cerr := config.Resolve()
				if cerr != nil {
					return &ExitError{Code: 1, Msg: cerr.Error()}
				}
				o.CarryExtra = cfg.StringList("isolation", "carry_extra")
				if v, ok := cfg.Lookup("isolation", "carry_files"); ok && v != nil {
					o.CarryFiles = nonNilStrings(cfg.StringList("isolation", "carry_files"))
				}
			}
			if abs, aerr := filepath.Abs(target); aerr == nil {
				target = abs
			}
			r, err = isolate.Adopt(dir, taskID, target, o)
		}
		if err != nil {
			return usageErr(prefix, err)
		}
		if err := writeResult(cmd.OutOrStdout(), f.Given("text"), r, func() string {
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
