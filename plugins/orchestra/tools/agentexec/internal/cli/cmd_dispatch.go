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
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/ledger"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/readiness"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/tools"
)

type dispatchFlags struct {
	class, archetype, exhausted, workdir, isolate, task, runID, resume, token string
	executor, model, effort                                                   string
	promptFiles                                                               []string
	noResume, noCooldown, noUsage, capture, detach, summary                   bool
}

func newDispatchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dispatch",
		Short: "one-call resolve + dispatch (or, with --executor, a direct run)",
		Args:  cobra.NoArgs,
	}
	f := &dispatchFlags{}
	fl := cmd.Flags()
	fl.StringVar(&f.class, "class", "", "capability class")
	fl.StringVar(&f.archetype, "archetype", "default", "task archetype")
	fl.StringVar(&f.exhausted, "exhausted", "", "comma-separated executors already tried")
	fl.StringArrayVar(&f.promptFiles, "prompt-file", nil, "prompt file (repeatable)")
	fl.StringVar(&f.workdir, "workdir", "", "working directory of the worker")
	fl.StringVar(&f.isolate, "isolate", "", dispatch.IsolateModes+" (default auto; never with --executor)")
	fl.StringVar(&f.task, "task", "", "task id (names the worktree and the resumable session)")
	fl.StringVar(&f.runID, "run-id", "", "run ledger id")
	fl.StringVar(&f.resume, "resume", "", "session id to resume")
	fl.BoolVar(&f.noResume, "no-resume", false, "start cold even when the task has a session")
	fl.BoolVar(&f.noCooldown, "no-cooldown", false, "ignore executor cooldowns")
	fl.BoolVar(&f.noUsage, "no-usage", false, "ignore subscription usage limits")
	fl.StringVar(&f.token, "token", "", "dispatch token from `dispatch prepare`")
	fl.BoolVar(&f.capture, "capture", false, "accepted for compatibility; dispatch always captures")
	fl.BoolVar(&f.detach, "detach", false, "run as a detached background child")
	fl.BoolVar(&f.summary, "summary", false, "print one compact line with the answer cut to 400 characters")
	fl.StringVar(&f.executor, "executor", "", "direct mode: run this executor without routing")
	fl.StringVar(&f.model, "model", "", "direct mode: model")
	fl.StringVar(&f.effort, "effort", "", "direct mode: effort")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error { return runDispatch(cmd, f) }
	cmd.AddCommand(newDispatchPrepareCmd(), newDispatchWaitCmd())
	return cmd
}

// checkIsolateRunID validates --isolate and --run-id when they are given.
func checkIsolateRunID(cmd *cobra.Command, isolate, runID string) error {
	if cmd.Flags().Changed("isolate") && !dispatch.ValidIsolate(isolate) {
		return exitf(2, "dispatch: --isolate must be "+dispatch.IsolateModes)
	}
	if cmd.Flags().Changed("run-id") && !ledger.RunIDPattern.MatchString(runID) {
		return exitf(2, "dispatch: invalid --run-id: %s", runID)
	}
	return nil
}

// isolateOr is --isolate, or def when it was not given.
func isolateOr(cmd *cobra.Command, isolate, def string) string {
	if cmd.Flags().Changed("isolate") {
		return isolate
	}
	return def
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

// requireFlags is the missing-flag usage error for the first of names not
// given; dispatch's required flags depend on its mode, so cobra's static
// MarkFlagRequired cannot express them.
func requireFlags(cmd *cobra.Command, names ...string) error {
	for _, n := range names {
		if !cmd.Flags().Changed(n) {
			return exitf(2, "%s: missing required flag --%s", cmdName(cmd), n)
		}
	}
	return nil
}

// resolveForDispatch is the routed path's config load: a broken layer is a
// JSON error line on stdout, exit 1.
func resolveForDispatch(cmd *cobra.Command) (*config.Resolved, error) {
	cfg, err := config.Resolve()
	if err != nil {
		if werr := writeJSONLine(cmd.OutOrStdout(), map[string]any{"error": "agent-exec: " + err.Error()}); werr != nil {
			return nil, werr
		}
		return nil, &ExitError{Code: 1}
	}
	tools.MarkAvailability(cfg)
	return cfg, nil
}

func runDispatch(cmd *cobra.Command, f *dispatchFlags) error {
	if err := checkIsolateRunID(cmd, f.isolate, f.runID); err != nil {
		return err
	}
	var exhausted []string
	for _, e := range strings.Split(f.exhausted, ",") {
		if e != "" {
			exhausted = append(exhausted, e)
		}
	}
	if cmd.Flags().Changed("token") {
		return runTokenDispatch(cmd, f, exhausted)
	}
	if f.detach {
		return exitf(2, "dispatch: --detach requires --token")
	}
	if f.summary {
		return exitf(2, "dispatch: --summary requires --token")
	}
	if cmd.Flags().Changed("executor") {
		return runDirectDispatch(cmd, f)
	}
	for _, name := range []string{"model", "effort"} {
		if cmd.Flags().Changed(name) {
			return exitf(2, "dispatch: --%s requires --executor", name)
		}
	}
	if err := requireFlags(cmd, "class", "prompt-file", "workdir"); err != nil {
		return err
	}
	// Unreadable prompt files are a usage error even when the config is
	// broken: they are checked first.
	if _, err := capture.ReadPromptFiles(f.promptFiles); err != nil {
		return exitf(2, "dispatch: %s", err)
	}
	cfg, err := resolveForDispatch(cmd)
	if err != nil {
		return err
	}
	return emitOutcome(cmd, dispatch.Execute(context.Background(), cfg, dispatch.Request{
		Class: f.class, Archetype: f.archetype, Workdir: f.workdir,
		Isolate: isolateOr(cmd, f.isolate, dispatch.IsolateAuto), Task: f.task, RunID: f.runID,
		Resume: f.resume, PromptFiles: f.promptFiles, Exhausted: exhausted,
		NoCooldown: f.noCooldown, NoUsage: f.noUsage, NoResume: f.noResume,
	}))
}

// runTokenDispatch runs (or detaches, or summarizes) a prepared token. Only
// --class (to escalate), --exhausted, --no-resume and the output switches
// may accompany it.
func runTokenDispatch(cmd *cobra.Command, f *dispatchFlags, exhausted []string) error {
	if conflicts := given(cmd, "prompt-file", "workdir", "archetype", "run-id", "isolate", "task",
		"resume", "no-cooldown", "no-usage", "executor", "model", "effort"); len(conflicts) > 0 {
		return exitf(2, "dispatch: --token conflicts with %s", strings.Join(conflicts, ", "))
	}
	cfg, err := resolveForDispatch(cmd)
	if err != nil {
		return err
	}
	dispatch.SweepRetention(cfg)
	spec, err := dispatch.LoadToken(cfg, f.token)
	if err != nil {
		return exitf(2, "dispatch: invalid token: %s", f.token)
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
		Class: spec.Class, Archetype: spec.Archetype, Workdir: spec.Workdir,
		Isolate: spec.Isolate, PromptFiles: spec.PromptFiles, NoResume: f.noResume,
		Exhausted: exhausted,
	}
	if cmd.Flags().Changed("class") {
		req.Class = f.class
	}
	if spec.RunID != nil {
		req.RunID = *spec.RunID
	}
	if spec.Task != nil {
		req.Task = *spec.Task
	}
	switch {
	case f.detach:
		return writeJSONLine(cmd.OutOrStdout(), dispatch.Detach(cfg, f.token, req.Args()))
	case f.summary:
		o := dispatch.Execute(context.Background(), cfg, req)
		if o.Out == nil {
			return emitOutcome(cmd, o)
		}
		dispatch.AttachAnswerPath(cfg, f.token, o.Out)
		o.Out = dispatch.Summarize(o.Out)
		return emitSummary(cmd, o)
	}
	return emitOutcome(cmd, dispatch.Execute(context.Background(), cfg, req))
}

// runDirectDispatch is `dispatch --executor E --model M --effort E`: the
// executor runs as given, without routing (the old `run --capture`).
func runDirectDispatch(cmd *cobra.Command, f *dispatchFlags) error {
	if readiness.KnownExecutors[f.executor] != "cli" {
		return exitf(2, "dispatch: unknown executor: %s", f.executor)
	}
	if conflicts := given(cmd, "archetype", "exhausted", "no-cooldown", "no-usage", "no-resume"); len(conflicts) > 0 {
		return exitf(2, "dispatch: --executor conflicts with %s", strings.Join(conflicts, ", "))
	}
	if err := requireFlags(cmd, "model", "effort", "workdir", "prompt-file"); err != nil {
		return err
	}
	if _, err := capture.ReadPromptFiles(f.promptFiles); err != nil {
		return exitf(2, "dispatch: %s", err)
	}
	// A direct run never fails on a broken config layer: the defaults apply.
	cfg := configOrDefaults()
	return emitOutcome(cmd, dispatch.Execute(context.Background(), cfg, dispatch.Request{
		Executor: f.executor, Model: f.model, Effort: f.effort, Class: f.class,
		Workdir: f.workdir, Isolate: isolateOr(cmd, f.isolate, dispatch.IsolateNever), Task: f.task,
		RunID: f.runID, Resume: f.resume, PromptFiles: f.promptFiles,
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
	cmd := &cobra.Command{
		Use:   "prepare",
		Short: "mint a dispatch token for the run",
		Args:  cobra.NoArgs,
	}
	fl := cmd.Flags()
	class := fl.String("class", "", "capability class")
	promptFiles := fl.StringArray("prompt-file", nil, "prompt file (repeatable; may not exist yet)")
	workdir := fl.String("workdir", "", "working directory of the worker")
	archetype := fl.String("archetype", "default", "task archetype")
	isolate := fl.String("isolate", dispatch.IsolateAuto, dispatch.IsolateModes)
	task := fl.String("task", "", "task id")
	runID := fl.String("run-id", "", "run ledger id")
	fl.Bool("json", false, "JSON output (the default and only format)")
	for _, name := range []string{"class", "prompt-file", "workdir"} {
		_ = cmd.MarkFlagRequired(name)
	}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if err := checkIsolateRunID(cmd, *isolate, *runID); err != nil {
			return err
		}
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		spec := dispatch.PrepareSpec{
			Class: *class, Archetype: *archetype, Workdir: *workdir,
			Isolate: *isolate, PromptFiles: *promptFiles,
		}
		if cmd.Flags().Changed("run-id") {
			spec.RunID = runID
		}
		if cmd.Flags().Changed("task") {
			spec.Task = task
		}
		token, err := dispatch.Prepare(cfg, spec)
		if err != nil {
			return exitf(1, "dispatch prepare: %s", err)
		}
		layers := make([]any, len(cfg.Layers))
		for i, l := range cfg.Layers {
			layers[i] = absPath(l.Path)
		}
		return writeJSONLine(cmd.OutOrStdout(), map[string]any{"token": token, "config_layers": layers})
	}
	return cmd
}

func newDispatchWaitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "wait",
		Short: "poll a --detach'd run",
		Args:  cobra.NoArgs,
	}
	token := cmd.Flags().String("token", "", "dispatch token")
	maxWait := cmd.Flags().Int("max-wait", int(dispatch.DefaultMaxWait/time.Second), "seconds to poll")
	summary := cmd.Flags().Bool("summary", false, "print one compact line with the answer cut to 400 characters")
	_ = cmd.MarkFlagRequired("token")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		if !dispatch.TokenPattern.MatchString(*token) {
			return exitf(2, "dispatch wait: invalid token: %s", *token)
		}
		wait := time.Duration(*maxWait) * time.Second
		out, code := dispatch.Wait(cfg, *token, wait, *summary)
		if *summary {
			return emitSummary(cmd, dispatch.Outcome{Out: out, Exit: code})
		}
		return emitOutcome(cmd, dispatch.Outcome{Out: out, Exit: code})
	}
	return cmd
}
