package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/capture"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/dispatch"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/ledger"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/readiness"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/tools"
)

type dispatchFlags struct {
	class, archetype, exhausted, workdir, isolate, task, runID, resume, token *optString
	executor, model, effort                                                   *optString
	promptFiles                                                               *[]string
	noResume, noCooldown, capture, detach, summary                            *optBool
}

func newDispatchCmd() *cobra.Command {
	cmd := withPrefix(&cobra.Command{
		Use:   "dispatch",
		Short: "one-call resolve + dispatch (or, with --executor, a direct run)",
		Args:  noArgs,
	}, "dispatch")
	f := dispatchFlags{
		class:       strFlag(cmd, "class", "capability class"),
		archetype:   strFlag(cmd, "archetype", "task archetype"),
		exhausted:   strFlag(cmd, "exhausted", "comma-separated executors already tried"),
		promptFiles: listFlag(cmd, "prompt-file", "prompt file (repeatable)"),
		workdir:     strFlag(cmd, "workdir", "working directory of the worker"),
		isolate:     strFlag(cmd, "isolate", "auto|always|never"),
		task:        strFlag(cmd, "task", "task id (names the worktree and the resumable session)"),
		runID:       strFlag(cmd, "run-id", "run ledger id"),
		resume:      strFlag(cmd, "resume", "session id to resume"),
		noResume:    boolFlag(cmd, "no-resume", "start cold even when the task has a session"),
		noCooldown:  boolFlag(cmd, "no-cooldown", "ignore executor cooldowns"),
		token:       strFlag(cmd, "token", "dispatch token from `dispatch prepare`"),
		capture:     boolFlag(cmd, "capture", "accepted for compatibility; dispatch always captures"),
		detach:      boolFlag(cmd, "detach", "run as a detached background child"),
		summary:     boolFlag(cmd, "summary", "print one compact line with the answer cut to 400 characters"),
		executor:    strFlag(cmd, "executor", "direct mode: run this executor without routing"),
		model:       strFlag(cmd, "model", "direct mode: model"),
		effort:      strFlag(cmd, "effort", "direct mode: effort"),
	}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error { return runDispatch(cmd, f) }
	cmd.AddCommand(newDispatchPrepareCmd(), newDispatchWaitCmd())
	return cmd
}

// checkIsolateRunID validates the two values the parser checked on sight.
func checkIsolateRunID(isolate, runID *optString) error {
	if v, ok := isolate.Get(); ok && !dispatch.ValidIsolate(v) {
		return exitf(2, "dispatch: --isolate must be auto|always|never")
	}
	if v, ok := runID.Get(); ok && !ledger.RunIDPattern.MatchString(v) {
		return exitf(2, "dispatch: invalid --run-id: %s", v)
	}
	return nil
}

// given lists the flags among names that were passed, as --name.
func given(cmd *cobra.Command, names ...string) []string {
	var out []string
	for _, n := range names {
		if cmd.Flags().Changed(n) {
			out = append(out, "--"+n)
		}
	}
	return out
}

// resolveForDispatch is the routed path's config load: a broken layer is a
// JSON error line on stdout, exit 1.
func resolveForDispatch(cmd *cobra.Command) (*config.Resolved, error) {
	cfg, err := config.Resolve()
	if err != nil {
		if werr := writeJSONLine(cmd.OutOrStdout(), jsonx.NewObject().Set("error", "agent-exec: "+err.Error())); werr != nil {
			return nil, werr
		}
		return nil, &ExitError{Code: 1}
	}
	tools.MarkAvailability(cfg)
	return cfg, nil
}

func runDispatch(cmd *cobra.Command, f dispatchFlags) error {
	if err := checkIsolateRunID(f.isolate, f.runID); err != nil {
		return err
	}
	var exhausted []string
	for _, e := range strings.Split(f.exhausted.Or(""), ",") {
		if e != "" {
			exhausted = append(exhausted, e)
		}
	}
	if token, ok := f.token.Get(); ok {
		return runTokenDispatch(cmd, f, token, exhausted)
	}
	if f.detach.On() {
		return exitf(2, "dispatch: --detach requires --token")
	}
	if f.summary.On() {
		return exitf(2, "dispatch: --summary requires --token")
	}
	if _, ok := f.executor.Get(); ok {
		return runDirectDispatch(cmd, f)
	}
	for _, name := range []string{"model", "effort"} {
		if cmd.Flags().Changed(name) {
			return exitf(2, "dispatch: --%s requires --executor", name)
		}
	}
	cls, ok := f.class.Get()
	if !ok {
		return exitf(2, "dispatch: missing required option: --class")
	}
	if len(*f.promptFiles) == 0 {
		return exitf(2, "dispatch: missing required option: --prompt-file")
	}
	workdir, ok := f.workdir.Get()
	if !ok {
		return exitf(2, "dispatch: missing required option: --workdir")
	}
	// The prompt files are checked before the config, as the parser did.
	if _, err := capture.ReadPromptFiles(*f.promptFiles); err != nil {
		return exitf(2, "dispatch: %s", err)
	}
	cfg, err := resolveForDispatch(cmd)
	if err != nil {
		return err
	}
	return emitOutcome(cmd, dispatch.Execute(context.Background(), cfg, dispatch.Request{
		Class: cls, Archetype: f.archetype.Or("default"), Workdir: workdir,
		Isolate: f.isolate.Or(dispatch.IsolateAuto), Task: f.task.Or(""), RunID: f.runID.Or(""),
		Resume: f.resume.Or(""), PromptFiles: *f.promptFiles, Exhausted: exhausted,
		NoCooldown: f.noCooldown.On(), NoResume: f.noResume.On(),
	}))
}

// runTokenDispatch runs (or detaches, or summarizes) a prepared token. Only
// --class (to escalate), --exhausted, --no-resume and the output switches
// may accompany it.
func runTokenDispatch(cmd *cobra.Command, f dispatchFlags, token string, exhausted []string) error {
	if conflicts := given(cmd, "prompt-file", "workdir", "archetype", "run-id", "isolate", "task",
		"resume", "no-cooldown", "executor", "model", "effort"); len(conflicts) > 0 {
		return exitf(2, "dispatch: --token conflicts with %s", strings.Join(conflicts, ", "))
	}
	cfg, err := resolveForDispatch(cmd)
	if err != nil {
		return err
	}
	dispatch.SweepRetention(cfg)
	spec, err := dispatch.LoadToken(cfg, token)
	if err != nil {
		return exitf(2, "dispatch: invalid token: %s", token)
	}
	if spec.ConfigRoot != "" {
		// Pin the project layers recorded at prepare time; the detached
		// child inherits the variable.
		os.Setenv("ORCHESTRA_CONFIG_ROOT", spec.ConfigRoot)
		if cfg, err = loadConfig(); err != nil {
			return err
		}
	}
	if spec.RunID != nil && !ledger.RunIDPattern.MatchString(*spec.RunID) {
		return exitf(2, "dispatch: invalid --run-id: %s", *spec.RunID)
	}
	req := dispatch.Request{
		Class: f.class.Or(spec.Class), Archetype: spec.Archetype, Workdir: spec.Workdir,
		Isolate: spec.Isolate, PromptFiles: spec.PromptFiles, NoResume: f.noResume.On(),
		Exhausted: exhausted,
	}
	if spec.RunID != nil {
		req.RunID = *spec.RunID
	}
	if spec.Task != nil {
		req.Task = *spec.Task
	}
	switch {
	case f.detach.On():
		return writeJSONLine(cmd.OutOrStdout(), dispatch.Detach(cfg, token, req.Args()))
	case f.summary.On():
		o := dispatch.Execute(context.Background(), cfg, req)
		if o.Out == nil {
			return emitOutcome(cmd, o)
		}
		dispatch.AttachAnswerPath(cfg, token, o.Out)
		o.Out = dispatch.Summarize(o.Out)
		return emitSummary(cmd, o)
	}
	return emitOutcome(cmd, dispatch.Execute(context.Background(), cfg, req))
}

// runDirectDispatch is `dispatch --executor E --model M --effort E`: the
// executor runs as given, without routing (the old `run --capture`).
func runDirectDispatch(cmd *cobra.Command, f dispatchFlags) error {
	executor, _ := f.executor.Get()
	if readiness.KnownExecutors[executor] != "cli" {
		return exitf(2, "dispatch: unknown executor: %s", executor)
	}
	if conflicts := given(cmd, "archetype", "exhausted", "no-cooldown", "no-resume"); len(conflicts) > 0 {
		return exitf(2, "dispatch: --executor conflicts with %s", strings.Join(conflicts, ", "))
	}
	for _, name := range []string{"model", "effort", "workdir"} {
		if !cmd.Flags().Changed(name) {
			return exitf(2, "dispatch: missing required option: --%s", name)
		}
	}
	if len(*f.promptFiles) == 0 {
		return exitf(2, "dispatch: missing required option: --prompt-file")
	}
	if _, err := capture.ReadPromptFiles(*f.promptFiles); err != nil {
		return exitf(2, "dispatch: %s", err)
	}
	// A direct run never fails on a broken config layer: the defaults apply.
	cfg := configOrDefaults()
	return emitOutcome(cmd, dispatch.Execute(context.Background(), cfg, dispatch.Request{
		Executor: executor, Model: f.model.Or(""), Effort: f.effort.Or(""), Class: f.class.Or(""),
		Workdir: f.workdir.Or(""), Isolate: f.isolate.Or(dispatch.IsolateNever), Task: f.task.Or(""),
		RunID: f.runID.Or(""), Resume: f.resume.Or(""), PromptFiles: *f.promptFiles,
	}))
}

func emitOutcome(cmd *cobra.Command, o dispatch.Outcome) error {
	if o.Text != "" {
		if _, err := io.WriteString(cmd.OutOrStdout(), o.Text); err != nil {
			return err
		}
	}
	if o.Out != nil {
		if err := writeJSONLine(cmd.OutOrStdout(), o.Out); err != nil {
			return err
		}
	}
	return outcomeExit(cmd, o)
}

func emitSummary(cmd *cobra.Command, o dispatch.Outcome) error {
	line, err := dispatch.CompactLine(o.Out)
	if err != nil {
		return err
	}
	if _, err := cmd.OutOrStdout().Write(append(line, '\n')); err != nil {
		return err
	}
	return outcomeExit(cmd, o)
}

func outcomeExit(cmd *cobra.Command, o dispatch.Outcome) error {
	if o.Diag != "" {
		fmt.Fprintf(cmd.ErrOrStderr(), "agent-exec: %s\n", o.Diag)
	}
	if o.Exit != 0 {
		return &ExitError{Code: o.Exit}
	}
	return nil
}

func newDispatchPrepareCmd() *cobra.Command {
	cmd := withPrefix(&cobra.Command{
		Use:   "prepare",
		Short: "mint a dispatch token for the run",
		Args:  noArgs,
	}, "dispatch")
	class := strFlag(cmd, "class", "capability class")
	promptFiles := listFlag(cmd, "prompt-file", "prompt file (repeatable; may not exist yet)")
	workdir := strFlag(cmd, "workdir", "working directory of the worker")
	archetype := strFlag(cmd, "archetype", "task archetype")
	isolate := strFlag(cmd, "isolate", "auto|always|never")
	task := strFlag(cmd, "task", "task id")
	runID := strFlag(cmd, "run-id", "run ledger id")
	boolFlag(cmd, "json", "JSON output")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if err := checkIsolateRunID(isolate, runID); err != nil {
			return err
		}
		cls, ok := class.Get()
		if !ok {
			return exitf(2, "dispatch: missing required option: --class")
		}
		if len(*promptFiles) == 0 {
			return exitf(2, "dispatch: missing required option: --prompt-file")
		}
		wd, ok := workdir.Get()
		if !ok {
			return exitf(2, "dispatch: missing required option: --workdir")
		}
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		spec := dispatch.PrepareSpec{
			Class: cls, Archetype: archetype.Or("default"), Workdir: wd,
			Isolate: isolate.Or(dispatch.IsolateAuto), PromptFiles: *promptFiles,
		}
		if v, ok := runID.Get(); ok {
			spec.RunID = &v
		}
		if v, ok := task.Get(); ok {
			spec.Task = &v
		}
		token, err := dispatch.Prepare(cfg, spec)
		if err != nil {
			return exitf(1, "dispatch: %s", err)
		}
		layers := make([]any, len(cfg.Layers))
		for i, l := range cfg.Layers {
			layers[i] = absPath(l.Path)
		}
		return writeJSONLine(cmd.OutOrStdout(), jsonx.NewObject().Set("token", token).Set("config_layers", layers))
	}
	return cmd
}

func newDispatchWaitCmd() *cobra.Command {
	cmd := withPrefix(&cobra.Command{
		Use:   "wait",
		Short: "poll a --detach'd run",
		Args:  noArgs,
	}, "dispatch")
	token := strFlag(cmd, "token", "dispatch token")
	maxWait := strFlag(cmd, "max-wait", "seconds to poll (default 560)")
	summary := boolFlag(cmd, "summary", "print one compact line with the answer cut to 400 characters")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		wait := dispatch.DefaultMaxWait
		if v, ok := maxWait.Get(); ok {
			n, valid := pyInt(v)
			if !valid {
				return exitf(2, "dispatch: --max-wait must be an integer")
			}
			wait = time.Duration(n) * time.Second
		}
		tok, ok := token.Get()
		if !ok {
			return exitf(2, "dispatch: missing required option: --token")
		}
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		if !dispatch.TokenPattern.MatchString(tok) {
			return exitf(2, "dispatch: invalid token: %s", tok)
		}
		out, code := dispatch.Wait(cfg, tok, wait, summary.On())
		if summary.On() {
			return emitSummary(cmd, dispatch.Outcome{Out: out, Exit: code})
		}
		return emitOutcome(cmd, dispatch.Outcome{Out: out, Exit: code})
	}
	return cmd
}
