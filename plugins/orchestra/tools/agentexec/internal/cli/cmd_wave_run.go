package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host/herdr"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host/orca"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/ledger"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/runner"
)

// WaveUIStart starts (or reuses) the dashboard for `wave run` and returns
// its URL. nil (the default) starts none; the ui command wires it.
var WaveUIStart func() (url string, err error)

const waveUsage = "usage: agent-exec wave status --state PATH [--json|--line|--text] [--live]\n" +
	"                        [--watch SEC] [--events N]\n" +
	"       agent-exec wave stop --state PATH [--reason TEXT]\n" +
	"       agent-exec wave lint --plan PATH [--json|--text]\n"

const waveRunUsage = "usage: agent-exec wave run --plan PLAN --state STATE --into TASK [--repo P]\n" +
	"                  [--max-in-flight N|auto] [--gate CMD] [--full CMD] [--full-every N]\n" +
	"                  [--full-timeout SEC] [--full-retry CMD] [--on-green CMD]\n" +
	"                  [--resume-on-reset]\n" +
	"                  [--no-ui] [--notify-cmd CMD]\n" +
	"                  [--stop-at EPOCH] [--max-waves N] [--max-packages N]\n" +
	"                  [--run-id ID] [--json|--text]\n"

const waveMarkUsage = "usage: agent-exec wave mark --state STATE --pkg ID --status ready|pending|failed\n" +
	"                  or --state STATE --await ID\n" +
	"                  [--widen GLOB[,GLOB...]] | --recheck ID[,ID...]\n" +
	"                  (ready enters self-verifying; pending|failed are terminal handoffs)\n" +
	"                  [--detail TEXT] | --clear-environment\n"

// newWaveCmd is the wave group: called bare, with an unknown subcommand or
// with a flag of its own it prints the wave usage (exit 2), as the Python
// dispatcher did.
func newWaveCmd() *cobra.Command {
	wave := &cobra.Command{
		Use:                "wave",
		Short:              "the deterministic outer loop over a plan",
		Args:               cobra.ArbitraryArgs,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, _ = io.WriteString(cmd.ErrOrStderr(), waveUsage)
			return &ExitError{Code: 2}
		},
	}
	addWaveStatusCommands(wave)
	addWaveRunCommands(wave)
	return wave
}

func addWaveRunCommands(parent *cobra.Command) {
	run := &cobra.Command{
		Use:                "run",
		Short:              "dispatch, check and integrate a plan wave by wave",
		Args:               cobra.ArbitraryArgs,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWaveRun(cmd.OutOrStdout(), cmd.ErrOrStderr(), args)
		},
	}
	// Declared for --help only; DisableFlagParsing means they are never parsed.
	for _, f := range []struct{ name, usage string }{
		{"plan", "plan path"},
		{"state", "state.json path"},
		{"into", "integration task id"},
		{"repo", "repository path (default cwd)"},
		{"max-in-flight", "packages dispatched at once (N or auto)"},
		{"gate", "command run after each integration"},
		{"full", "full verification command"},
		{"full-every", "run --full every N waves"},
		{"full-timeout", "seconds"},
		{"full-retry", "command re-run on a red --full's failing files"},
		{"on-green", "command run on a green --full"},
		{"notify-cmd", "command run on notable events"},
		{"stop-at", "epoch seconds to stop at"},
		{"max-waves", "stop after N waves"},
		{"max-packages", "stop after N packages"},
		{"run-id", "run ledger id"},
	} {
		strFlag(run, f.name, f.usage)
	}
	boolFlag(run, "resume-on-reset", "resume packages after a usage-limit reset")
	boolFlag(run, "no-ui", "do not start the dashboard")
	formatFlags(run, "json", "text")

	mark := &cobra.Command{
		Use:                "mark",
		Short:              "hand a package back to the runner and clear its needs",
		Args:               cobra.ArbitraryArgs,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWaveMark(cmd.OutOrStdout(), cmd.ErrOrStderr(), args)
		},
	}
	strFlag(mark, "state", "state.json path")
	strFlag(mark, "pkg", "package id")
	strFlag(mark, "status", "ready|pending|failed")
	strFlag(mark, "detail", "note")
	strFlag(mark, "widen", "globs to widen the package's files_owned by")
	strFlag(mark, "recheck", "packages to send back to self-verify")
	strFlag(mark, "await", "package whose interactive session to wait for")
	boolFlag(mark, "clear-environment", "clear the wave-level environment need")

	parent.AddCommand(run, mark)
}

type waveRunArgs struct {
	opts runner.Options
	text bool
}

// parseWaveRun parses `wave run` like the Python parser (first error in
// argument order wins); the message is "" on success.
func parseWaveRun(args []string) (waveRunArgs, string) {
	var a waveRunArgs
	a.opts.FullEvery = 1
	seen := map[string]bool{}
	invalid := func(flag, v string) string { return fmt.Sprintf("invalid %s value: %s", flag, v) }
	nonNegInt := func(flag, v string) (int, string) {
		n, ok := pyInt(v)
		if !ok || n < 0 {
			return 0, invalid(flag, v)
		}
		return n, ""
	}
	nonNegFloat := func(flag, v string) (float64, string) {
		f, ok := pyFloat(v)
		if !ok || f < 0 {
			return 0, invalid(flag, v)
		}
		return f, ""
	}
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if seen[tok] {
			return a, "duplicate option: " + tok
		}
		switch tok {
		case "--json", "--text", "--resume-on-reset", "--no-ui":
			seen[tok] = true
			switch tok {
			case "--text":
				a.text = true
			case "--resume-on-reset":
				a.opts.ResumeOnReset = true
			case "--no-ui":
				a.opts.NoUI = true
			}
			continue
		case "--plan", "--state", "--into", "--repo", "--max-in-flight", "--gate", "--full",
			"--full-every", "--full-timeout", "--full-retry", "--on-green", "--notify-cmd",
			"--stop-at", "--max-waves", "--max-packages", "--run-id":
		default:
			return a, "unknown option: " + tok
		}
		if i+1 >= len(args) {
			return a, "missing value for " + tok
		}
		v := args[i+1]
		i++
		seen[tok] = true
		var msg string
		switch tok {
		case "--plan":
			a.opts.Plan = v
		case "--state":
			a.opts.State = v
		case "--into":
			a.opts.Into = v
		case "--repo":
			a.opts.Repo = v
		case "--gate":
			a.opts.Gate = v
		case "--full":
			a.opts.Full = v
		case "--full-retry":
			a.opts.FullRetry = v
		case "--on-green":
			a.opts.OnGreen = v
		case "--notify-cmd":
			a.opts.NotifyCmd = v
		case "--run-id":
			a.opts.RunID = v
		case "--max-in-flight":
			if strings.ToLower(trimSpace(v)) == "auto" {
				a.opts.MaxInFlight = 0
				break
			}
			n, ok := pyInt(v)
			if !ok {
				return a, invalid(tok, v)
			}
			if n < 1 {
				// Reported after the whole line parsed, like Python.
				a.opts.MaxInFlight = -1
				break
			}
			a.opts.MaxInFlight = n
		case "--full-every":
			a.opts.FullEvery, msg = nonNegInt(tok, v)
		case "--full-timeout":
			var f float64
			f, msg = nonNegFloat(tok, v)
			a.opts.FullTimeout = secondsDuration(f)
		case "--stop-at":
			var f float64
			f, msg = nonNegFloat(tok, v)
			a.opts.StopAt = &f
		case "--max-waves":
			var n int
			n, msg = nonNegInt(tok, v)
			a.opts.MaxWaves = &n
		case "--max-packages":
			var n int
			n, msg = nonNegInt(tok, v)
			a.opts.MaxPackages = &n
		}
		if msg != "" {
			return a, msg
		}
	}
	if seen["--json"] && seen["--text"] {
		return a, "--json and --text are mutually exclusive"
	}
	for _, flag := range []string{"--plan", "--state", "--into"} {
		if !seen[flag] {
			return a, "missing required option: " + flag
		}
	}
	if a.opts.MaxInFlight < 0 {
		return a, "--max-in-flight must be at least 1"
	}
	if a.opts.FullEvery < 1 {
		return a, "--full-every must be at least 1"
	}
	if seen["--run-id"] && !ledger.RunIDPattern.MatchString(a.opts.RunID) {
		return a, "invalid --run-id value: " + a.opts.RunID
	}
	return a, ""
}

func runWaveRun(out, errw io.Writer, args []string) error {
	a, msg := parseWaveRun(args)
	if msg != "" {
		fmt.Fprintf(errw, "agent-exec: wave run: %s\n", msg)
		_, _ = io.WriteString(errw, waveRunUsage)
		return &ExitError{Code: 2}
	}
	cfg := configOrDefaults()
	var herdrHost, orcaHost host.Host = herdr.New(cfg), orca.New(cfg)
	report, code := runner.Run(context.Background(), a.opts, runner.Deps{
		Herdr: herdrHost, Orca: orcaHost,
		Config:  config.Resolve,
		Check:   inProcessCheck,
		StartUI: WaveUIStart,
		Stderr:  errw,
	})
	if a.text {
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
	err := runCheck(&buf, args)
	var ee *ExitError
	switch {
	case err == nil:
		return buf.String(), 0
	case errors.As(err, &ee):
		return buf.String(), ee.Code
	}
	return buf.String(), 1
}

func runWaveMark(out, errw io.Writer, args []string) error {
	values := map[string]string{}
	var o runner.MarkOptions
	hasAwait := false
	valueFlags := map[string]string{"--state": "state", "--pkg": "pkg", "--status": "status",
		"--detail": "detail", "--widen": "widen", "--recheck": "recheck"}
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if tok == "--clear-environment" {
			o.ClearEnvironment = true
			continue
		}
		key, ok := valueFlags[tok]
		if !ok && tok != "--await" {
			return exitf(2, "wave mark: unknown option: %s", tok)
		}
		if i+1 >= len(args) {
			return exitf(2, "wave mark: missing value for %s", tok)
		}
		if tok == "--await" {
			o.Await, hasAwait = args[i+1], true
		} else {
			values[key] = args[i+1]
		}
		i++
	}
	split := func(key string) []string {
		v, ok := values[key]
		if !ok {
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
	o.State, o.Pkg, o.Status, o.Detail = values["state"], values["pkg"], values["status"], values["detail"]
	o.Widen, o.Recheck = split("widen"), split("recheck")
	_, hasStatus := values["status"]
	usage := func() error {
		_, _ = io.WriteString(errw, waveMarkUsage)
		return &ExitError{Code: 2}
	}
	switch {
	case o.ClearEnvironment && hasAwait:
		return usage()
	case o.ClearEnvironment:
		for _, k := range []string{"pkg", "status", "detail", "widen", "recheck"} {
			if _, given := values[k]; given {
				return usage()
			}
		}
		if o.State == "" {
			return usage()
		}
	case hasAwait:
		if o.Pkg != "" || o.Status != "" || o.Detail != "" || o.Widen != nil || o.Recheck != nil || o.State == "" {
			return usage()
		}
		if o.Await == "" {
			o.Await = "\x00" // an empty id never names a package (Python: no such need)
		}
	case o.State == "" || (o.Pkg == "" && len(o.Recheck) == 0) || (o.Status == "" && o.Widen == nil && o.Recheck == nil):
		return usage()
	}
	if hasStatus && o.Status != "ready" && o.Status != "pending" && o.Status != "failed" {
		return exitf(2, "wave mark: --status must be ready|pending|failed")
	}
	res, code := runner.Mark(o)
	if code != 0 {
		return exitf(code, "wave mark: %s", res["error"])
	}
	return writeJSONLine(out, res)
}
