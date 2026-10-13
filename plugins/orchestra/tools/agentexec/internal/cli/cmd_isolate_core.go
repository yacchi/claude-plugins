package cli

import (
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
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
	)
}

func repoOrCwd(repo *optString) string {
	if v, ok := repo.Get(); ok {
		return v
	}
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

// usageErr is the exit-2 report of an input the domain rejected (Python's
// ValueError), under the command's prefix.
func usageErr(prefix string, err error) error {
	return exitf(2, "%s: %s", prefix, err)
}

// newIsolateSimpleCmd builds create/list/diff/remove, which share one flag
// set in the Python parser (each rejects the flags it cannot use itself).
func newIsolateSimpleCmd(name, short string) *cobra.Command {
	cmd := withPrefix(&cobra.Command{
		Use:   name,
		Short: short,
		Args:  noArgs,
	}, "isolate")
	task := strFlag(cmd, "task", "task id")
	repo := strFlag(cmd, "repo", "repository path (default cwd)")
	backend := strFlag(cmd, "backend", "auto|gtr|git")
	session := strFlag(cmd, "session", "session id")
	noCarry := boolFlag(cmd, "no-carry", "do not carry uncommitted work and dependency dirs")
	force := boolFlag(cmd, "force", "remove even with uncollected changes")
	namesOnly := boolFlag(cmd, "names-only", "omit the patch text")
	boolFlag(cmd, "json", "JSON output (the default)")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		taskID, hasTask := task.Get()
		sess, hasSession := session.Get()
		switch {
		case (name == "create" || name == "diff") && !hasTask:
			return exitf(2, "isolate %s: missing required option: --task", name)
		case name == "remove" && hasTask == hasSession:
			return exitf(2, "isolate remove: provide exactly one of --task or --session")
		case name != "remove" && name != "list" && hasSession:
			return exitf(2, "isolate %s: --session is not supported", name)
		}
		be := backend.Or(isolate.BackendAuto)
		switch be {
		case isolate.BackendAuto, isolate.BackendGtr, isolate.BackendGit:
		default:
			return exitf(2, "isolate: unknown backend: %s", be)
		}
		dir := repoOrCwd(repo)

		var result any
		status := ""
		var err error
		switch name {
		case "create":
			opts := isolate.CreateOptions{Backend: be, Carry: !noCarry.On()}
			if opts.Carry {
				cfg, cerr := config.Resolve()
				if cerr != nil {
					return &ExitError{Code: 1, Msg: cerr.Error()}
				}
				opts.CarryExtra = cfg.StringList("isolation", "carry_extra")
				if v, ok := cfg.Lookup("isolation", "carry_files"); ok && v != nil {
					opts.CarryFiles = nonNilStrings(cfg.StringList("isolation", "carry_files"))
				}
			}
			var r isolate.CreateResult
			r, err = isolate.Create(dir, taskID, opts)
			result, status = r, r.Status
		case "list":
			var entries []isolate.Entry
			entries, err = isolate.List(dir, sess)
			result = map[string]any{"worktrees": entries}
		case "diff":
			var r isolate.DiffResult
			r, err = isolate.DiffAndCollect(dir, taskID)
			r.OmitPatch = namesOnly.On()
			result, status = r, r.Status
		case "remove":
			var r isolate.RemoveResult
			if hasSession {
				r, err = isolate.RemoveSession(dir, sess, force.On())
			} else {
				r, err = isolate.Remove(dir, taskID, force.On(), "")
			}
			result, status = r, r.Status
		}
		if err != nil {
			return usageErr("isolate", err)
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

// duplicate reports the first value or presence flag given more than once,
// in Python's wording.
func duplicate(cmd *cobra.Command, counts map[string]int, order ...string) error {
	for _, name := range order {
		if counts[name] > 1 {
			return exitf(2, "%s: duplicate option: --%s", errPrefix(cmd), name)
		}
	}
	return nil
}

func newIsolateCollectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "collect",
		Short: "mark a task's (or a session's) worktree changes as collected",
		Args:  noArgs,
	}
	task := strFlag(cmd, "task", "task id")
	session := strFlag(cmd, "session", "session id")
	repo := strFlag(cmd, "repo", "repository path (default cwd)")
	f := formatFlags(cmd, "json", "text")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		counts := map[string]int{"task": task.Count(), "session": session.Count(), "repo": repo.Count(),
			"json": f.flags["json"].Count(), "text": f.flags["text"].Count()}
		if err := duplicate(cmd, counts, "task", "session", "repo", "json", "text"); err != nil {
			return err
		}
		if err := f.exclusive(cmd); err != nil {
			return err
		}
		taskID, hasTask := task.Get()
		sess, hasSession := session.Get()
		if hasTask == hasSession {
			return exitf(2, "isolate collect: provide exactly one of --task or --session")
		}
		dir := repoOrCwd(repo)
		var r isolate.CollectResult
		var err error
		if hasSession {
			r, err = isolate.CollectSession(dir, sess)
		} else {
			r, err = isolate.Collect(dir, taskID, "")
		}
		if err != nil {
			return usageErr("isolate collect", err)
		}
		if err := writeResult(cmd.OutOrStdout(), f.Given("text"), r, func() string {
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
		Args:  noArgs,
	}
	repo := strFlag(cmd, "repo", "repository path (default cwd)")
	olderThan := strFlag(cmd, "older-than", "only worktrees older than DAYS")
	liveWindow := strFlag(cmd, "live-window", "minutes a session counts as live (default 120)")
	includeCurrent := boolFlag(cmd, "include-current", "include the running session's worktrees")
	includeLive := boolFlag(cmd, "include-live", "include sessions seen within the live window")
	branches := boolFlag(cmd, "branches", "also delete orphaned orchestra branches")
	dryRun := boolFlag(cmd, "dry-run", "report only")
	force := boolFlag(cmd, "force", "remove even with uncollected changes")
	f := formatFlags(cmd, "json", "text")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		counts := map[string]int{
			"repo": repo.Count(), "older-than": olderThan.Count(), "live-window": liveWindow.Count(),
			"include-current": includeCurrent.Count(), "include-live": includeLive.Count(),
			"branches": branches.Count(), "dry-run": dryRun.Count(), "force": force.Count(),
			"json": f.flags["json"].Count(), "text": f.flags["text"].Count(),
		}
		if err := duplicate(cmd, counts, "repo", "older-than", "live-window", "include-current",
			"include-live", "branches", "dry-run", "force", "json", "text"); err != nil {
			return err
		}
		if err := f.exclusive(cmd); err != nil {
			return err
		}
		opts := isolate.SweepOptions{
			IncludeCurrent: includeCurrent.On(), IncludeLive: includeLive.On(),
			Branches: branches.On(), DryRun: dryRun.On(), Force: force.On(),
		}
		if v, ok := olderThan.Get(); ok {
			days, ok := pyFloat(v)
			if !ok || days < 0 {
				return exitf(2, "isolate sweep: --older-than takes a number of days")
			}
			if !math.IsNaN(days) { // NaN compares false: no tree is "newer"
				opts.OlderThanDays = &days
			}
		}
		minutes := isolate.DefaultLiveWindow.Minutes()
		if v, ok := liveWindow.Get(); ok {
			m, ok := pyFloat(v)
			if !ok || m < 0 {
				return exitf(2, "isolate sweep: --live-window takes a number of minutes")
			}
			minutes = m
			if math.IsNaN(m) { // NaN compares false: no session is live
				minutes = 0
			}
		}
		opts.LiveWindow = minutesDuration(minutes)
		r, err := isolate.Sweep(repoOrCwd(repo), opts)
		if err != nil {
			return usageErr("isolate sweep", err)
		}
		if err := writeResult(cmd.OutOrStdout(), f.Given("text"), r, func() string {
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

// minutesDuration converts, saturating instead of overflowing (Python's
// float window has no upper bound; "inf" means "always live").
func minutesDuration(m float64) time.Duration {
	const maxMinutes = float64(1<<63-1) / float64(time.Minute)
	if m >= maxMinutes {
		return time.Duration(1<<63 - 1)
	}
	return time.Duration(m * float64(time.Minute))
}

// pyFloat parses a number the way Python's float() does for CLI input:
// surrounding whitespace allowed, inf/nan spellings, decimal only.
func pyFloat(s string) (float64, bool) {
	s = trimSpace(s)
	if strings.ContainsAny(s, "xXpP") {
		return 0, false // Go would read hex floats; Python does not
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && isPySpace(s[start]) {
		start++
	}
	for end > start && isPySpace(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isPySpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}
