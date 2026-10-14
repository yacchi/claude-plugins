// Package runner is `agent-exec wave run` / `wave mark`: the deterministic
// outer loop that drives a plan through dispatch -> self-verify ->
// integrate, every state change going through wave/state's Store.
//
// Scheduling is rolling: up to max_in_flight packages run as goroutines,
// and whenever one finishes, every package that reached `ready` is
// integrated (one integrate call at a time, plan order, skip on conflict,
// gate + bisect) and the scheduler is asked again, so a dependent starts as
// soon as its dependencies are integrated. A "wave" is one scheduling round
// that started something. `wave.verify_mode: final` (default) runs only
// `fast` checks per package, never gates integration on checks, and
// converges at the end: the full checks run on the integration tree and
// red ones become `fix-<n>` packages. `gated` keeps per-package checks, the
// integration gate and `--full` pausing the run on red. Anything only the
// instructor can decide lands on the state's `needs` list instead of
// stopping the loop.
//
// Dispatch goes through an Executor (CliExecutor re-runs this binary's
// `dispatch` in production, a fake in tests). A package the dispatch hands
// back as `delegate` (a Claude tier) runs in an interactive host session
// (herdr or Orca) when one is available; corrections then go to that same
// live session.
package runner

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/plan"
)

// Options are the `wave run` options. A nil pointer is "not given".
type Options struct {
	Plan, State, Into string
	// Repo is the repository (default: the cwd).
	Repo string
	// MaxInFlight is the width; 0 (or "auto" on the CLI) resolves from
	// `wave.max_in_flight`, else the herdr formula / 4.
	MaxInFlight int
	// VerifyMode / ConvergeRounds override `wave.verify_mode` /
	// `wave.converge_rounds` (not CLI flags; tests and callers).
	VerifyMode     string
	ConvergeRounds *int
	// InteractiveHost overrides the `interactive_host` config.
	InteractiveHost string
	Gate, Full      string
	FullEvery       int // 0 = 1
	FullTimeout     time.Duration
	FullRetry       string
	OnGreen         string
	ResumeOnReset   bool
	NoUI            bool
	NotifyCmd       string
	StopAt          *float64
	MaxWaves        *int
	MaxPackages     *int
	RunID           string
}

// Deps are the runner's collaborators; zero values pick the production
// ones.
type Deps struct {
	// Executor dispatches packages; nil is a CliExecutor re-running
	// AgentExec.
	Executor Executor
	// Host is an injected interactive host; it wins over the candidates.
	Host host.Host
	// Herdr and Orca are the interactive-host candidates `interactive_host`
	// selects from (either may be nil).
	Herdr, Orca host.Host
	// Config loads the config each time the runner consults it (the
	// runner re-reads it, so edits apply to a running wave); nil is
	// config.Resolve.
	Config func() (*config.Resolved, error)
	// AgentExec is the argv prefix running this agent-exec (the default
	// gate, the child dispatches, self-verify checks); nil is
	// [os.Executable()].
	AgentExec []string
	// Check runs `agent-exec check ARGS` and returns its stdout and exit
	// code (gated self-verify); nil runs AgentExec as a child process.
	Check func(args []string) (stdout string, exit int)
	// StartUI starts or reuses the dashboard and returns its URL; nil
	// starts none.
	StartUI func() (url string, err error)
	// Clock is epoch seconds; Sleep waits that many seconds.
	Clock func() float64
	Sleep func(seconds float64)
	// Stderr receives progress lines (max-in-flight, plan warnings, ui).
	Stderr io.Writer
}

// Report is the end-of-run report. A Status of "error" (with Kind "plan"
// or "environment") means the loop never started.
type Report struct {
	Status         string
	Reason         *string
	Kind           string
	Waves          int
	Approvals      int
	State          string
	Integrated     []string
	Needs          []NeedRef
	NetworkDenials []map[string]any
	Pending        []string
	Blocked        []string
	FlakyTests     []FlakyTest
}

// NeedRef is one need in the report.
type NeedRef struct {
	ID   *string `json:"id"`
	Kind string  `json:"kind"`
}

// FlakyTest is a test file seen flaky at least checks.flaky_threshold times.
type FlakyTest struct {
	File  string `json:"file"`
	Count int    `json:"count"`
}

func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func (r Report) MarshalJSON() ([]byte, error) {
	if r.Status == "error" {
		return fsutil.MarshalJSON(map[string]any{"status": r.Status, "reason": deref(r.Reason), "kind": r.Kind})
	}
	return fsutil.MarshalJSON(map[string]any{
		"status":          r.Status,
		"reason":          deref(r.Reason),
		"waves":           r.Waves,
		"approvals":       r.Approvals,
		"state":           r.State,
		"integrated":      orEmpty(r.Integrated),
		"needs":           orEmpty(r.Needs),
		"network_denials": orEmpty(r.NetworkDenials),
		"pending":         orEmpty(r.Pending),
		"blocked":         orEmpty(r.Blocked),
		"flaky_tests":     orEmpty(r.FlakyTests),
	})
}

func deref(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// ExitCode is the CLI exit status of a report: 0 done, 1 done with needs,
// 5 stopped, 2 a plan error, 3 any other error.
func ExitCode(r Report) int {
	switch r.Status {
	case "stopped":
		return 5
	case "done":
		if len(r.Needs) > 0 {
			return 1
		}
		return 0
	}
	if r.Kind == "plan" {
		return 2
	}
	return 3
}

// FormatText is the `--text` report.
func FormatText(r Report) string {
	if r.Status == "error" {
		return "wave run error: " + derefOr(r.Reason, "unknown")
	}
	line := fmt.Sprintf("wave run %s  waves=%d", r.Status, r.Waves)
	if r.Reason != nil && *r.Reason != "" {
		line += "  reason: " + *r.Reason
	}
	lines := []string{line}
	for _, kv := range []struct {
		key  string
		vals []string
	}{{"integrated", r.Integrated}, {"pending", r.Pending}, {"blocked", r.Blocked}} {
		if len(kv.vals) > 0 {
			lines = append(lines, fmt.Sprintf("  %s: %s", kv.key, strings.Join(kv.vals, ", ")))
		}
	}
	if len(r.Needs) > 0 {
		parts := make([]string, len(r.Needs))
		for i, n := range r.Needs {
			parts[i] = fmt.Sprintf("%s(%s)", derefOr(n.ID, "-"), n.Kind)
		}
		lines = append(lines, "  needs: "+strings.Join(parts, ", "))
	}
	if len(r.NetworkDenials) > 0 {
		parts := make([]string, len(r.NetworkDenials))
		for i, d := range r.NetworkDenials {
			id := d["id"]
			h := d["host"]
			port := d["port"]
			parts[i] = fmt.Sprintf("%s(%s:%s)", valText(id), valText(h), valText(port))
		}
		lines = append(lines, "  network denied: "+strings.Join(parts, ", "))
	}
	if r.Approvals > 0 {
		lines = append(lines, fmt.Sprintf("  approvals: %d pending — agent-exec approvals --state %s", r.Approvals, r.State))
	}
	return strings.Join(lines, "\n")
}

func derefOr(s *string, def string) string {
	if s == nil {
		return def
	}
	return *s
}

func errorReport(kind string, err error) Report {
	msg := err.Error()
	return Report{Status: "error", Reason: &msg, Kind: kind}
}

// Run drives the plan to done or stopped and returns the report with its
// exit code (0 / 1 / 5; 2 or 3 when the loop never started: a bad plan, or
// no repository / integration worktree).
func Run(ctx context.Context, o Options, d Deps) (Report, int) {
	r := newRunner(ctx, o, d)
	if err := r.init(); err != nil {
		kind := "environment"
		if _, ok := err.(*plan.Error); ok {
			kind = "plan"
		}
		rep := errorReport(kind, err)
		return rep, ExitCode(rep)
	}
	if !o.NoUI && os.Getenv("ORCHESTRA_WAVE_NO_UI") != "1" && d.StartUI != nil {
		if url, err := d.StartUI(); err != nil {
			fmt.Fprintf(r.stderr, "warning: could not start ui: %s\n", err)
		} else {
			fmt.Fprintf(r.stderr, "ui: %s\n", url)
			r.emit("ui", "", map[string]any{"url": url})
		}
	}
	func() {
		defer r.shutdownHost()
		r.loop()
	}()
	rep := r.report()
	if rep.Status == "done" {
		payload := reportObject(rep)
		payload["event"] = "done"
		payload["at"] = r.clock()
		r.notify("done", payload)
	}
	r.notifyWG.Wait()
	if rep.Status == "done" && isDir(r.greenPath) {
		_, _ = gitRun(r.root, "worktree", "remove", "--force", r.greenPath)
	}
	return rep, ExitCode(rep)
}

// reportObject is the report as an ordered object (the notify payload).
func reportObject(rep Report) map[string]any {
	o := map[string]any{}
	b, err := rep.MarshalJSON()
	if err != nil {
		return o
	}
	if v, err := fsutil.DecodeJSON(b); err == nil {
		if obj, ok := v.(map[string]any); ok {
			return obj
		}
	}
	return o
}
