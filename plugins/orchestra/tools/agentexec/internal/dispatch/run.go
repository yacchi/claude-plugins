package dispatch

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/capture"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/cooldown"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/executor/pi"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/ledger"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/quota"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/readiness"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/route"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/sandbox"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/telemetry"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/tools"
)

// DryRunEnv set (non-empty) makes a dispatch print the executor argv it
// would run instead of running it.
const DryRunEnv = "AGENT_EXEC_DRYRUN"

// Request is one dispatch. Without Executor it is routed: Class (with
// Archetype, Exhausted, the cooldowns unless NoCooldown and the
// subscription usage unless NoUsage) picks the
// executor. With Executor it is a direct run of that executor with Model
// and Effort (no route, no session store; Class only picks the watchdog
// budget and the ledger class). Empty strings are "not given": Archetype
// "default", Isolate "auto" routed and "never" direct, Task/RunID/Resume
// none. Isolate "cli" isolates only a route agent-exec runs itself; a
// direct run always is one.
type Request struct {
	Class, Archetype, Workdir, Isolate, Task, RunID, Resume, Executor, Model, Effort string
	PromptFiles, Exhausted                                                           []string
	NoCooldown, NoUsage, NoResume                                                    bool
}

// Args is r as `dispatch` flags (what a detached child is started with).
func (r Request) Args() []string {
	archetype := r.Archetype
	if archetype == "" {
		archetype = "default"
	}
	isolate := r.Isolate
	if isolate == "" {
		isolate = IsolateAuto
	}
	args := []string{"--class", r.Class, "--archetype", archetype, "--workdir", r.Workdir, "--isolate", isolate}
	for _, p := range r.PromptFiles {
		args = append(args, "--prompt-file", p)
	}
	if r.RunID != "" {
		args = append(args, "--run-id", r.RunID)
	}
	if r.Task != "" {
		args = append(args, "--task", r.Task)
	}
	if r.Resume != "" {
		args = append(args, "--resume", r.Resume)
	}
	if r.NoResume {
		args = append(args, "--no-resume")
	}
	if r.NoCooldown {
		args = append(args, "--no-cooldown")
	}
	if r.NoUsage {
		args = append(args, "--no-usage")
	}
	if len(r.Exhausted) > 0 {
		args = append(args, "--exhausted", strings.Join(r.Exhausted, ","))
	}
	return args
}

// Outcome is everything a dispatch produced: the JSON result (nil when
// there is none), the text a dry run prints instead, a diagnostic for
// stderr (without the "agent-exec: " prefix) and the exit code.
type Outcome struct {
	Out  map[string]any
	Text string
	Diag string
	Exit int
}

// Run is Execute's JSON result and exit code.
func Run(ctx context.Context, cfg *config.Resolved, r Request) (map[string]any, int) {
	o := Execute(ctx, cfg, r)
	return o.Out, o.Exit
}

// Execute dispatches r. A routed dispatch reports `unroutable` (with the
// route), `delegate` (a claude/agent route: the caller spawns the
// subagent), or the executor's result with executor/model/effort/resumed/
// route/isolation/sandbox added. A direct one reports the executor's bare
// result (plus isolation when one was asked for). A binary missing from
// PATH is no result, the not-found diagnostic and exit 127.
func Execute(ctx context.Context, cfg *config.Resolved, r Request) Outcome {
	prompt, err := capture.ReadPromptFiles(r.PromptFiles)
	if err != nil {
		return Outcome{Diag: "dispatch: " + err.Error(), Exit: 2}
	}
	if r.Executor != "" {
		return direct(ctx, cfg, r, prompt)
	}
	return routed(ctx, cfg, r, prompt)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func routed(ctx context.Context, cfg *config.Resolved, r Request, prompt string) Outcome {
	SweepRetention(cfg)
	var active map[string]cooldown.Entry
	if !r.NoCooldown && cooldown.Enabled(cfg) {
		active = cooldown.Active(cfg, time.Now())
	}
	archetype := orDefault(r.Archetype, "default")
	var usage map[string]quota.Entry
	if !r.NoUsage && quota.Enabled(cfg) {
		usage = quota.Snapshot(cfg, route.UsageGroups(cfg, r.Class, archetype), time.Now())
	}
	rt := route.Resolve(cfg, readiness.Compute(cfg).Ready, r.Class, archetype, r.Exhausted, active, usage)
	if !rt.Routable() {
		return Outcome{Out: map[string]any{"status": "unroutable", "route": rt.Object()}}
	}
	mode := orDefault(r.Isolate, IsolateAuto)
	var iso isolation
	if mode == IsolateCli && rt.Dispatch != "cli" {
		iso = callerIsolates()
	} else {
		iso = isolateFor(cfg, r.Workdir, mode, r.Task)
	}
	if iso.refused != "" {
		return Outcome{Diag: "dispatch: " + iso.refused, Exit: 1}
	}
	workdir := iso.workdir(r.Workdir)

	if rt.Dispatch != "cli" {
		// claude / agent: agent-exec cannot spawn subagents; the caller does.
		return Outcome{Out: map[string]any{
			"status":     "delegate",
			"executor":   rt.Executor,
			"model":      rt.Model,
			"effort":     rt.Effort,
			"agent_type": rt.AgentType,
			"route":      rt.Object(),
			"isolation":  iso.obj,
			"sandbox": sandbox.Report(sandbox.Unwrapped(
				"delegated to an agent; agent-exec spawns no child to sandbox")),
		}}
	}

	executor, model, effort := rt.Executor, asString(rt.Model), asString(rt.Effort)
	if executor != "pi" {
		// A configured `dispatch: cli` executor with no invocation profile.
		out := map[string]any{
			"status":     "unavailable",
			"answer":     nil,
			"session_id": nil,
			"reason":     nil,
			"exit_code":  nil,
			"executor":   executor,
			"model":      rt.Model,
			"effort":     rt.Effort,
			"route":      rt.Object(),
			"isolation":  iso.obj,
			"sandbox": sandbox.Report(sandbox.Unwrapped(
				"no invocation profile for this executor; nothing was spawned")),
		}
		cooldown.RecordUnavailable(cfg, executor, "", time.Now(), nil, "")
		ledger.Append(ledger.BuildRecord(executor, model, r.Class, out), r.RunID, cfg)
		return Outcome{Out: out}
	}

	// Session continuity, highest first: an explicit --resume; --no-resume;
	// the store record for (executor, task) when its workdir matches and its
	// context is under resume_max_tokens (a longer one is re-read every turn,
	// so the correction starts fresh); cold.
	limit := config.ResumeMaxTokensFor(cfg, model)
	resume, resumed := r.Resume, r.Resume != ""
	if resume == "" && r.Task != "" && !r.NoResume {
		if rec, ok := ReadSession(cfg, executor, r.Task); ok && sameWorkdir(rec.Workdir, workdir) &&
			(limit == 0 || rec.ContextTokens < limit) {
			resume, resumed = rec.SessionID, true
		}
	}

	// The sandbox confines the child to the task tree: the worktree root
	// when isolated (not the possibly deeper workdir), else the workdir.
	tree := iso.tree(workdir)
	spec := sandbox.Prepare(executor, tree, cfg, model, r.Task)
	if os.Getenv(DryRunEnv) != "" {
		argv := pi.BuildArgv("pi", model, effort, workdir, prompt, resume, "json", true)
		return Outcome{Text: dryRunText(argv, workdir, spec,
			"would subprocess-run pi and emit normalized JSON + route")}
	}

	result, spec, err := capture.RunWithGrants(ctx, capture.Request{
		Executor: executor, Model: model, Effort: effort, Workdir: workdir, Prompt: prompt,
		Resume: resume, Class: r.Class, Task: r.Task, SandboxTree: tree, Sandbox: spec, Cfg: cfg,
	})
	if err != nil {
		return captureFailure(cfg, err, executor, model, r.Class, r.RunID)
	}
	record(cfg, executor, model, r.Class, r.RunID, resume, result)

	// Only a run that worked (or that the instructor resumes once the cause
	// is fixed) and named its session updates the store: a failed round
	// keeps the last good session for the correction round.
	status := result["status"]
	sid := asString(get(result, "session_id"))
	lastCtx, known := resumeGateContext(result)
	if r.Task != "" && sid != "" && (status == "ok" || status == "needs-permission" || status == "runaway") {
		_, _ = WriteSession(cfg, executor, r.Task, sid, workdir, lastCtx)
	}

	out := maps.Clone(result)
	out["executor"] = executor
	out["model"] = rt.Model
	out["effort"] = rt.Effort
	out["resumed"] = resumed || get(result, "resumed") == true
	if known {
		out["context_tokens"] = lastCtx
	} else {
		out["context_tokens"] = nil
	}
	out["resumable"] = sid != "" && (limit == 0 || !known || lastCtx < limit)
	out["route"] = rt.Object()
	out["isolation"] = iso.obj
	if sb := get(result, "sandbox"); sb != nil {
		out["sandbox"] = sb
	} else {
		out["sandbox"] = sandbox.Report(spec)
	}
	return Outcome{Out: out}
}

func get(o map[string]any, k string) any {
	v := o[k]
	return v
}

// direct is `dispatch --executor`: run one named executor, no routing.
func direct(ctx context.Context, cfg *config.Resolved, r Request, prompt string) Outcome {
	mode := orDefault(r.Isolate, IsolateNever)
	workdir := r.Workdir
	var iso isolation
	if mode != IsolateNever {
		iso = isolateFor(cfg, r.Workdir, mode, r.Task)
		if iso.refused != "" {
			return Outcome{Diag: "dispatch: " + iso.refused, Exit: 1}
		}
		workdir = iso.workdir(r.Workdir)
	}
	tree := iso.tree(workdir)
	spec := sandbox.Prepare(r.Executor, tree, cfg, r.Model, r.Task)
	if os.Getenv(DryRunEnv) != "" {
		exe := tools.Resolve(r.Executor, cfg).Path
		if exe == "" {
			exe = r.Executor
		}
		argv := pi.BuildArgv(exe, r.Model, r.Effort, workdir, prompt, r.Resume, "json", tools.PiContextFiles(cfg))
		return Outcome{Text: dryRunText(argv, workdir, spec,
			"would subprocess-run "+r.Executor+" and emit normalized JSON")}
	}
	result, _, err := capture.RunWithGrants(ctx, capture.Request{
		Executor: r.Executor, Model: r.Model, Effort: r.Effort, Workdir: workdir, Prompt: prompt,
		Resume: r.Resume, Class: r.Class, Task: r.Task, SandboxTree: tree, Sandbox: spec, Cfg: cfg,
	})
	if err != nil {
		return captureFailure(cfg, err, r.Executor, r.Model, r.Class, r.RunID)
	}
	record(cfg, r.Executor, r.Model, r.Class, r.RunID, r.Resume, result)
	if iso.obj != nil {
		result["isolation"] = iso.obj
	}
	return Outcome{Out: result}
}

// record books one finished run: the cooldown of an unavailable executor,
// telemetry and the run ledger. Never fails the dispatch.
func record(cfg *config.Resolved, executor, model, cls, runID, resume string, result map[string]any) {
	if get(result, "status") == "unavailable" {
		var exitCode *int
		if n, ok := get(result, "exit_code").(int64); ok {
			c := int(n)
			exitCode = &c
		}
		cooldown.RecordUnavailable(cfg, executor, asString(get(result, "reason")), time.Now(),
			exitCode, asString(get(result, "answer")))
	}
	telemetry.Append(telemetry.BuildDispatchRecord(executor, result, resume != "", cls), cfg)
	ledger.Append(ledger.BuildRecord(executor, model, cls, result), runID, cfg)
}

// captureFailure is a run that produced no result: the binary is not on
// PATH (exit 127, an unavailable ledger line) or the spawn itself failed.
func captureFailure(cfg *config.Resolved, err error, executor, model, cls, runID string) Outcome {
	if errors.Is(err, capture.ErrExecutorNotFound) {
		ledger.Append(ledger.BuildRecord(executor, model, cls,
			map[string]any{"status": "unavailable"}), runID, cfg)
		bin := executor
		if b, ok := tools.KnownBinaries[executor]; ok {
			bin = b
		}
		return Outcome{Diag: fmt.Sprintf("executor '%s' not found on PATH", bin), Exit: capture.NotFoundExitCode}
	}
	return Outcome{Diag: "dispatch: " + err.Error(), Exit: 1}
}

func dryRunText(argv []string, workdir string, spec sandbox.Spec, capture string) string {
	cwd := ""
	if st, err := os.Stat(workdir); err == nil && st.IsDir() {
		cwd = workdir
	}
	wrapped := sandbox.WrapArgv(argv, cwd, spec)
	return fmt.Sprintf("PROFILE: pi\nMODE: headless\nENV: (none)\nSANDBOX: %s (%s)\nEXEC: %s\nCAPTURE: yes (%s)\n",
		spec.Backend, spec.Reason, strings.Join(wrapped, " "), capture)
}
