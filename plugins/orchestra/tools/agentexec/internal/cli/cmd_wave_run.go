package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host/herdr"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host/orca"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/ledger"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/ui"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/runner"
)

// WaveUIStart starts (or reuses) the dashboard for `wave run` and returns
// its URL; a dashboard it starts stops after the default idle time. A
// variable so tests can replace it; nil starts none.
var WaveUIStart = func() (url string, err error) {
	info, err := ui.StartOrReuseIdle(false, 0, ui.IdleMinutesDefault)
	return info.URL, err
}

const waveMarkUsage = "usage: agent-exec wave mark --state STATE --pkg ID --status ready|pending|failed\n" +
	"                  or --state STATE --await ID\n" +
	"                  [--widen GLOB[,GLOB...]] | --recheck ID[,ID...]\n" +
	"                  (ready enters self-verifying; pending|failed are terminal handoffs)\n" +
	"                  [--detail TEXT] | --clear-environment\n"

// newWaveCmd is the wave group.
func newWaveCmd() *cobra.Command {
	wave := groupCommand("wave", "the deterministic outer loop over a plan")
	addWaveStatusCommands(wave)
	addWaveRunCommands(wave)
	return wave
}

func addWaveRunCommands(parent *cobra.Command) {
	run := &cobra.Command{
		Use:   "run",
		Short: "dispatch, check and integrate a plan wave by wave",
		Args:  cobra.NoArgs,
	}
	var o runner.Options
	fl := run.Flags()
	fl.StringVar(&o.Plan, "plan", "", "plan path")
	fl.StringVar(&o.State, "state", "", "state.json path")
	fl.StringVar(&o.Into, "into", "", "integration task id")
	fl.StringVar(&o.Repo, "repo", "", "repository path (default cwd)")
	maxInFlight := fl.String("max-in-flight", "auto", "packages dispatched at once (N, or auto: every startable one)")
	fl.StringVar(&o.Gate, "gate", "", "command run after each integration")
	fl.StringVar(&o.Full, "full", "", "full verification command")
	fl.IntVar(&o.FullEvery, "full-every", 1, "run --full every N waves")
	fullTimeout := fl.Float64("full-timeout", 0, "seconds")
	fl.StringVar(&o.FullRetry, "full-retry", "", "command re-run on a red --full's failing files")
	fl.StringVar(&o.OnGreen, "on-green", "", "command run on a green --full")
	fl.StringVar(&o.NotifyCmd, "notify-cmd", "", "command run on notable events")
	stopAt := fl.Float64("stop-at", 0, "epoch seconds to stop at")
	maxWaves := fl.Int("max-waves", 0, "stop after N waves")
	maxPackages := fl.Int("max-packages", 0, "stop after N packages")
	budgetUSD := fl.Float64("budget-usd", 0, "start nothing new once the run's estimated cost reaches this (needs --run-id)")
	fl.StringVar(&o.RunID, "run-id", "", "run ledger id")
	fl.BoolVar(&o.ResumeOnReset, "resume-on-reset", false, "resume packages after a usage-limit reset")
	fl.BoolVar(&o.NoUI, "no-ui", false, "do not start the dashboard")
	runFormat := formatFlags(run, "json", "text")
	for _, name := range []string{"plan", "state", "into"} {
		_ = run.MarkFlagRequired(name)
	}
	run.RunE = func(cmd *cobra.Command, _ []string) error {
		bad := func(format string, args ...any) error {
			return exitf(2, "wave run: "+format, args...)
		}
		changed := cmd.Flags().Changed
		if v := strings.TrimSpace(*maxInFlight); !strings.EqualFold(v, "auto") {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return bad("--max-in-flight must be auto or at least 1, got %q", *maxInFlight)
			}
			o.MaxInFlight = n
		}
		if o.FullEvery < 1 {
			return bad("--full-every must be at least 1")
		}
		if !finiteNonNegative(*fullTimeout) {
			return bad("--full-timeout must be a non-negative number of seconds")
		}
		o.FullTimeout = duration(*fullTimeout, time.Second)
		if changed("stop-at") {
			if !finiteNonNegative(*stopAt) {
				return bad("--stop-at must be a non-negative epoch time")
			}
			o.StopAt = stopAt
		}
		if changed("max-waves") {
			if *maxWaves < 0 {
				return bad("--max-waves must not be negative")
			}
			o.MaxWaves = maxWaves
		}
		if changed("max-packages") {
			if *maxPackages < 0 {
				return bad("--max-packages must not be negative")
			}
			o.MaxPackages = maxPackages
		}
		if changed("run-id") && !ledger.RunIDPattern.MatchString(o.RunID) {
			return bad("invalid --run-id: %s", o.RunID)
		}
		if changed("budget-usd") {
			if !finiteNonNegative(*budgetUSD) {
				return bad("--budget-usd must be a non-negative number of US dollars")
			}
			if *budgetUSD > 0 && o.RunID == "" {
				return bad("--budget-usd needs --run-id (the budget is that run's estimated cost)")
			}
			o.BudgetUSD = budgetUSD
		}
		return runWaveRun(cmd.OutOrStdout(), cmd.ErrOrStderr(), o, runFormat.is("text"))
	}

	parent.AddCommand(run, newWaveMarkCmd())
}

func runWaveRun(out, errw io.Writer, o runner.Options, text bool) error {
	cfg := configOrDefaults()
	var herdrHost, orcaHost host.Host = herdr.New(cfg), orca.New(cfg)
	report, code := runner.Run(context.Background(), o, runner.Deps{
		Herdr: herdrHost, Orca: orcaHost,
		Config:  config.Resolve,
		Check:   inProcessCheck,
		StartUI: WaveUIStart,
		Stderr:  errw,
	})
	if text {
		fmt.Fprintln(out, runner.FormatText(report))
	} else if err := writeJSONLine(out, report); err != nil {
		return err
	}
	if code != 0 {
		return &ExitError{Code: code}
	}
	return nil
}

// inProcessCheck is `agent-exec check ARGS` run in this process (gated
// self-verify), its stdout captured.
func inProcessCheck(args []string) (string, int) {
	var buf bytes.Buffer
	cmd := newCheckCmd()
	rejectRepeats(cmd, args)
	cmd.SetArgs(args)
	cmd.SetOut(&buf)
	cmd.SetErr(io.Discard)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	if err := cmd.Execute(); err != nil {
		return buf.String(), asExitError(cmd, err).Code
	}
	return buf.String(), 0
}

func newWaveMarkCmd() *cobra.Command {
	mark := &cobra.Command{
		Use:   "mark",
		Short: "hand a package back to the runner and clear its needs",
		Args:  cobra.NoArgs,
	}
	var o runner.MarkOptions
	var widen, recheck string
	fl := mark.Flags()
	fl.StringVar(&o.State, "state", "", "state.json path")
	fl.StringVar(&o.Pkg, "pkg", "", "package id")
	fl.StringVar(&o.Status, "status", "", "ready|pending|failed")
	fl.StringVar(&o.Detail, "detail", "", "note")
	fl.StringVar(&widen, "widen", "", "comma-separated globs to widen the package's files_owned by")
	fl.StringVar(&recheck, "recheck", "", "comma-separated packages to send back to self-verify")
	fl.StringVar(&o.Await, "await", "", "package whose interactive session to wait for")
	fl.BoolVar(&o.ClearEnvironment, "clear-environment", false, "clear the wave-level environment need")
	_ = mark.MarkFlagRequired("state")
	mark.MarkFlagsMutuallyExclusive("await", "clear-environment")
	mark.RunE = func(cmd *cobra.Command, _ []string) error {
		changed := cmd.Flags().Changed
		split := func(name, v string) []string {
			if !changed(name) {
				return nil
			}
			list := []string{}
			for _, item := range strings.Split(v, ",") {
				if item != "" {
					list = append(list, item)
				}
			}
			return list
		}
		o.Widen, o.Recheck = split("widen", widen), split("recheck", recheck)
		usage := func() error {
			_, _ = io.WriteString(cmd.ErrOrStderr(), waveMarkUsage)
			return &ExitError{Code: 2}
		}
		switch {
		case o.State == "":
			return usage()
		case o.ClearEnvironment:
			if given := given(cmd, "pkg", "status", "detail", "widen", "recheck"); len(given) > 0 {
				return usage()
			}
		case changed("await"):
			if o.Pkg != "" || o.Status != "" || o.Detail != "" || o.Widen != nil || o.Recheck != nil {
				return usage()
			}
			if o.Await == "" {
				return exitf(2, "wave mark: --await needs a package id")
			}
		case (o.Pkg == "" && len(o.Recheck) == 0) || (o.Status == "" && o.Widen == nil && o.Recheck == nil):
			return usage()
		}
		if changed("status") && o.Status != "ready" && o.Status != "pending" && o.Status != "failed" {
			return exitf(2, "wave mark: --status must be ready|pending|failed")
		}
		res, code := runner.Mark(o)
		if code != 0 {
			return exitf(code, "wave mark: %s", res["error"])
		}
		return writeJSONLine(cmd.OutOrStdout(), res)
	}
	return mark
}
