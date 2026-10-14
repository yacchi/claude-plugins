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

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/plan"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/state"
)

// ApprovalsPending reports how many approvals await a decision for a wave's
// state file. The approvals package (which depends on wave/state) installs
// the real counter; until then status shows none.
var ApprovalsPending = func(statePath string) int { return 0 }

type waveStatusOptions struct {
	statePath string
	format    format
	live      bool
	watch     float64
	events    int
}

func addWaveStatusCommands(parent *cobra.Command) {
	status := &cobra.Command{
		Use:   "status",
		Short: "render a wave's state.json",
		Args:  cobra.NoArgs,
	}
	var so waveStatusOptions
	status.Flags().StringVar(&so.statePath, "state", "", "state.json path")
	so.format = formatFlags(status, "json", "line", "text")
	status.Flags().BoolVar(&so.live, "live", false, "include live worktree changes")
	status.Flags().Float64Var(&so.watch, "watch", 0, "redraw every SEC seconds")
	status.Flags().IntVar(&so.events, "events", 0, "show the last N events")
	_ = status.MarkFlagRequired("state")
	status.RunE = func(cmd *cobra.Command, _ []string) error {
		if cmd.Flags().Changed("watch") && !finiteNonNegative(so.watch) {
			return exitf(2, "wave status: --watch must be a non-negative number of seconds")
		}
		return runWaveStatus(cmd.OutOrStdout(), cmd.ErrOrStderr(), so, cmd.Flags().Changed("watch"))
	}

	stop := &cobra.Command{
		Use:   "stop",
		Short: "ask a wave's runner to stop",
		Args:  cobra.NoArgs,
	}
	stopState := stop.Flags().String("state", "", "state.json path")
	reason := stop.Flags().String("reason", "", "why")
	_ = stop.MarkFlagRequired("state")
	stop.RunE = func(cmd *cobra.Command, _ []string) error {
		if err := state.RequestStop(*stopState, *reason); err != nil {
			return &ExitError{Code: 1, Msg: "wave stop: " + err.Error()}
		}
		_, err := fmt.Fprintln(cmd.OutOrStdout(), `{"stop_requested": true}`)
		return err
	}

	lint := &cobra.Command{
		Use:   "lint",
		Short: "check a wave plan",
		Args:  cobra.NoArgs,
	}
	planPath := lint.Flags().String("plan", "", "plan path")
	lintFormat := formatFlags(lint, "json", "text")
	_ = lint.MarkFlagRequired("plan")
	lint.RunE = func(cmd *cobra.Command, _ []string) error {
		return runWaveLint(cmd.OutOrStdout(), *planPath, lintFormat.is("json"))
	}

	parent.AddCommand(status, stop, lint)
}

func runWaveStatus(out, errw io.Writer, o waveStatusOptions, watch bool) error {
	renderOnce := func() (string, error) {
		if info, err := os.Stat(o.statePath); err != nil || !info.Mode().IsRegular() {
			fmt.Fprintf(errw, "agent-exec: wave status: no state file at %s\n", o.statePath)
			return "", &ExitError{Code: 3}
		}
		store, err := state.Open(o.statePath)
		if err != nil {
			return "", &ExitError{Code: 3, Msg: "wave status: " + err.Error()}
		}
		st, err := store.Load()
		if err != nil {
			return "", &ExitError{Code: 1, Msg: "wave status: cannot read state: " + err.Error()}
		}
		now := float64(time.Now().UnixNano()) / 1e9
		switch {
		case o.format.is("json"):
			m := st.ToMap()
			if o.live {
				m["live_changes"] = state.LiveChanges(&st)
			}
			b, err := fsutil.MarshalJSON(m)
			return string(b), err
		case o.format.is("line"):
			return state.StatusLine(&st, now, o.live), nil
		}
		ro := state.RenderOptions{Live: o.live, StatePath: o.statePath, ApprovalsPending: ApprovalsPending(o.statePath)}
		if o.events != 0 {
			ro.Events = state.ReadEvents(o.statePath, o.events)
		}
		return state.RenderStatus(&st, now, ro), nil
	}

	if !watch {
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
	clearScreen := !o.format.is("json") && !o.format.is("line")
	for {
		s, err := renderOnce()
		if err != nil {
			return err
		}
		if clearScreen {
			fmt.Fprint(out, "\x1b[H\x1b[2J")
		}
		fmt.Fprintln(out, s)
		select {
		case <-sigs:
			return nil
		case <-time.After(duration(o.watch, time.Second)):
		}
	}
}

func runWaveLint(out io.Writer, planPath string, asJSON bool) error {
	p, err := plan.Load(planPath)
	if err != nil {
		var pe *plan.Error
		if errors.As(err, &pe) {
			return &ExitError{Code: 3, Msg: "wave lint: " + pe.Msg}
		}
		return err
	}
	warnings := plan.Lint(p, "")
	if asJSON {
		list := make([]any, 0, len(warnings))
		for _, w := range warnings {
			var pkg any
			if w.Pkg != nil {
				pkg = *w.Pkg
			}
			list = append(list, map[string]any{"pkg": pkg, "message": w.Message})
		}
		if err := writeJSONLine(out, list); err != nil {
			return err
		}
	} else {
		for _, w := range warnings {
			if w.Pkg != nil {
				fmt.Fprintf(out, "plan-warning: %s: %s\n", *w.Pkg, w.Message)
			} else {
				fmt.Fprintf(out, "plan-warning: %s\n", w.Message)
			}
		}
	}
	if len(warnings) > 0 {
		return &ExitError{Code: 1}
	}
	return nil
}
