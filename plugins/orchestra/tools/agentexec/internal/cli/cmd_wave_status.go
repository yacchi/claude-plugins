package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/plan"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/state"
)

// ApprovalsPending reports how many approvals await a decision for a wave's
// state file. The approvals package (which depends on wave/state) installs
// the real counter; until then status shows none.
var ApprovalsPending = func(statePath string) int { return 0 }

// The wave status/stop/lint commands parse their own arguments, as the Python
// parsers did ("wave: unknown argument: X", last format flag wins).
func addWaveStatusCommands(parent *cobra.Command) {
	status := &cobra.Command{
		Use:                "status",
		Short:              "render a wave's state.json",
		Args:               cobra.ArbitraryArgs,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWaveStatus(cmd.OutOrStdout(), cmd.ErrOrStderr(), args)
		},
	}
	// Declared for --help only; DisableFlagParsing means they are never parsed.
	strFlag(status, "state", "state.json path")
	formatFlags(status, "json", "line", "text")
	boolFlag(status, "live", "include live worktree changes")
	strFlag(status, "watch", "redraw every SEC seconds")
	strFlag(status, "events", "show the last N events")

	stop := &cobra.Command{
		Use:                "stop",
		Short:              "ask a wave's runner to stop",
		Args:               cobra.ArbitraryArgs,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWaveStop(cmd.OutOrStdout(), args)
		},
	}
	strFlag(stop, "state", "state.json path")
	strFlag(stop, "reason", "why")

	lint := &cobra.Command{
		Use:                "lint",
		Short:              "check a wave plan",
		Args:               cobra.ArbitraryArgs,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWaveLint(cmd.OutOrStdout(), args)
		},
	}
	strFlag(lint, "plan", "plan path")
	formatFlags(lint, "json", "text")

	parent.AddCommand(status, stop, lint)
}

func runWaveStatus(out, errw io.Writer, args []string) error {
	statePath, hasState := "", false
	format := "text"
	watch, hasWatch := 0.0, false
	events := 0
	live := false
	for i := 0; i < len(args); i++ {
		tok := args[i]
		switch tok {
		case "--state":
			if i+1 >= len(args) {
				return exitf(2, "wave: missing value for --state")
			}
			statePath, hasState = args[i+1], true
			i++
		case "--json", "--line", "--text":
			format = tok[2:]
		case "--watch":
			if i+1 >= len(args) {
				return exitf(2, "wave: missing value for --watch")
			}
			v, ok := pyFloat(args[i+1])
			if !ok {
				return exitf(2, "wave: bad --watch value: %s", args[i+1])
			}
			watch, hasWatch = v, true
			i++
		case "--live":
			live = true
		case "--events":
			if i+1 >= len(args) {
				return exitf(2, "wave: missing value for --events")
			}
			n, ok := pyInt(args[i+1])
			if !ok {
				return exitf(2, "wave: bad --events value: %s", args[i+1])
			}
			events = n
			i++
		default:
			return exitf(2, "wave: unknown argument: %s", tok)
		}
	}
	if !hasState {
		return exitf(2, "wave: status: --state is required")
	}

	renderOnce := func() (string, error) {
		if info, err := os.Stat(statePath); err != nil || !info.Mode().IsRegular() {
			fmt.Fprintf(errw, "agent-exec: wave: no state file at %s\n", statePath)
			return "", &ExitError{Code: 3}
		}
		store, err := state.Open(statePath)
		if err != nil {
			return "", &ExitError{Code: 3, Msg: "wave: " + err.Error()}
		}
		st, err := store.Load()
		if err != nil {
			return "", &ExitError{Code: 1, Msg: "wave: cannot read state: " + err.Error()}
		}
		now := float64(time.Now().UnixNano()) / 1e9
		switch format {
		case "json":
			m := st.ToMap()
			if live {
				m["live_changes"] = state.LiveChanges(&st)
			}
			b, err := jsonx.Marshal(m)
			return string(b), err
		case "line":
			return state.StatusLine(&st, now, live), nil
		}
		o := state.RenderOptions{Live: live, StatePath: statePath, ApprovalsPending: ApprovalsPending(statePath)}
		if events != 0 {
			o.Events = state.ReadEvents(statePath, events)
		}
		return state.RenderStatus(&st, now, o), nil
	}

	if !hasWatch {
		s, err := renderOnce()
		if err != nil {
			return err
		}
		fmt.Fprintln(out, s)
		return nil
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	for {
		s, err := renderOnce()
		if err != nil {
			return err
		}
		if format == "text" {
			fmt.Fprint(out, "\x1b[H\x1b[2J")
		}
		fmt.Fprintln(out, s)
		select {
		case <-sigs:
			return nil
		case <-time.After(time.Duration(max(watch, 0) * float64(time.Second))):
		}
	}
}

func runWaveStop(out io.Writer, args []string) error {
	statePath, hasState := "", false
	reason := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--state":
			if i+1 >= len(args) {
				return exitf(2, "wave: missing value for --state")
			}
			statePath, hasState = args[i+1], true
			i++
		case "--reason":
			if i+1 >= len(args) {
				return exitf(2, "wave: missing value for --reason")
			}
			reason = args[i+1]
			i++
		default:
			return exitf(2, "wave: unknown argument: %s", args[i])
		}
	}
	if !hasState {
		return exitf(2, "wave: stop: --state is required")
	}
	if err := state.RequestStop(statePath, reason); err != nil {
		return &ExitError{Code: 1, Msg: "wave: " + err.Error()}
	}
	_, err := fmt.Fprintln(out, `{"stop_requested": true}`)
	return err
}

func runWaveLint(out io.Writer, args []string) error {
	planPath, hasPlan := "", false
	format := "text"
	for i := 0; i < len(args); i++ {
		switch tok := args[i]; tok {
		case "--plan":
			if i+1 >= len(args) {
				return exitf(2, "wave lint: missing value for --plan")
			}
			planPath, hasPlan = args[i+1], true
			i++
		case "--json", "--text":
			format = tok[2:]
		default:
			return exitf(2, "wave lint: unknown argument: %s", tok)
		}
	}
	if !hasPlan {
		return exitf(2, "wave lint: --plan is required")
	}
	p, err := plan.Load(planPath)
	if err != nil {
		var pe *plan.Error
		if errors.As(err, &pe) {
			return &ExitError{Code: 3, Msg: "wave lint: " + pe.Msg}
		}
		return err
	}
	warnings := plan.Lint(p, "")
	if format == "json" {
		list := make([]any, 0, len(warnings))
		for _, w := range warnings {
			var pkg any
			if w.Pkg != nil {
				pkg = *w.Pkg
			}
			list = append(list, jsonx.NewObject().Set("pkg", pkg).Set("message", w.Message))
		}
		if err := writeJSONLine(out, list); err != nil {
			return err
		}
	} else {
		for _, w := range warnings {
			pkg := "None"
			if w.Pkg != nil {
				pkg = *w.Pkg
			}
			fmt.Fprintf(out, "plan-warning: %s: %s\n", pkg, w.Message)
		}
	}
	if len(warnings) > 0 {
		return &ExitError{Code: 1}
	}
	return nil
}
