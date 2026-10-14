package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/ui"
)

func newUICmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "start (or reuse) the local dashboard and print its URL",
		Args:  cobra.NoArgs,
	}
	fl := cmd.Flags()
	open := fl.Bool("open", false, "open the URL in a browser")
	stop := fl.Bool("stop", false, "stop the dashboard")
	asJSON := fl.Bool("json", false, "JSON output")
	port := fl.Int("port", 0, "port of --serve (default: any free port)")
	idle := fl.Float64("idle-minutes", ui.IdleMinutesDefault, "exit after N idle minutes")
	serve := fl.Bool("serve", false, "run the server in the foreground")
	_ = fl.MarkHidden("serve")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if *port < 0 {
			return exitf(2, "ui: --port must not be negative")
		}
		if !finiteNonNegative(*idle) {
			return exitf(2, "ui: --idle-minutes must be a non-negative number")
		}
		out := cmd.OutOrStdout()
		fail := func(err error) error {
			return &ExitError{Code: 1, Msg: "ui: " + err.Error()}
		}
		switch {
		case *serve:
			if err := ui.ServeIdle(*port, *idle); err != nil {
				return fail(err)
			}
			return nil
		case *stop:
			res, err := ui.Stop()
			if err != nil {
				return fail(err)
			}
			if *asJSON {
				return writeJSONLine(out, map[string]any{"stopped": res["stopped"]})
			}
			if res["stopped"] == true {
				fmt.Fprintln(out, "stopped")
			} else {
				fmt.Fprintln(out, "not running")
			}
			return nil
		}
		info, err := ui.StartOrReuseIdle(*open, 0, *idle)
		if err != nil {
			return fail(err)
		}
		if *asJSON {
			return writeJSONLine(out, info)
		}
		fmt.Fprintln(out, info.URL)
		return nil
	}
	return cmd
}
