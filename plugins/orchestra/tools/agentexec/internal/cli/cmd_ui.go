package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/ui"
)

const uiUsage = "Usage:\n  agent-exec ui [--open] [--stop] [--idle-minutes N] [--json]\n"

// The ui command parses its own arguments, as the Python parser did
// ("agent-exec ui: <error>" and the usage on stderr, exit 2).
func newUICmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:                "ui",
		Short:              "start (or reuse) the local dashboard and print its URL",
		Args:               cobra.ArbitraryArgs,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUI(cmd.OutOrStdout(), cmd.ErrOrStderr(), args)
		},
	}
	// Declared for --help only; DisableFlagParsing means they are never parsed.
	boolFlag(cmd, "open", "open the URL in a browser")
	boolFlag(cmd, "stop", "stop the dashboard")
	boolFlag(cmd, "json", "JSON output")
	strFlag(cmd, "port", "port of --serve (default: any free port)")
	strFlag(cmd, "idle-minutes", "exit after N idle minutes")
	boolFlag(cmd, "serve", "run the server in the foreground")
	_ = cmd.Flags().MarkHidden("serve")
	return cmd
}

type uiOptions struct {
	open, stop, json, serve bool
	port                    int
	idle                    float64
}

// parseUIArgs is (options, error text); both empty is --help.
func parseUIArgs(args []string) (*uiOptions, string) {
	o := &uiOptions{idle: ui.IdleMinutesDefault}
	for i := 0; i < len(args); i++ {
		switch tok := args[i]; tok {
		case "--open":
			o.open = true
		case "--stop":
			o.stop = true
		case "--json":
			o.json = true
		case "--serve":
			o.serve = true
		case "--idle-minutes", "--port":
			if i+1 >= len(args) {
				return nil, tok + " needs a value"
			}
			raw := args[i+1]
			if tok == "--port" {
				v, ok := pyInt(raw)
				if !ok {
					return nil, fmt.Sprintf("%s: invalid value %s", tok, isolate.PyRepr(raw))
				}
				if v < 0 {
					return nil, tok + " must not be negative"
				}
				o.port = v
			} else {
				v, ok := pyFloat(raw)
				if !ok {
					return nil, fmt.Sprintf("%s: invalid value %s", tok, isolate.PyRepr(raw))
				}
				if v < 0 {
					return nil, tok + " must not be negative"
				}
				o.idle = v
			}
			i++
		case "-h", "--help":
			return nil, ""
		default:
			return nil, "unknown argument " + isolate.PyRepr(tok)
		}
	}
	return o, ""
}

func runUI(out, errw io.Writer, args []string) error {
	o, msg := parseUIArgs(args)
	if o == nil {
		if msg != "" {
			fmt.Fprintf(errw, "agent-exec ui: %s\n", msg)
		}
		fmt.Fprint(errw, uiUsage)
		if msg != "" {
			return &ExitError{Code: 2}
		}
		return nil
	}
	fail := func(err error) error {
		fmt.Fprintf(errw, "agent-exec ui: %s\n", err)
		return &ExitError{Code: 1}
	}
	switch {
	case o.serve:
		if err := ui.ServeIdle(o.port, o.idle); err != nil {
			return fail(err)
		}
		return nil
	case o.stop:
		res, err := ui.Stop()
		if err != nil {
			return fail(err)
		}
		if o.json {
			return writeJSONLine(out, map[string]any{"stopped": res["stopped"]})
		}
		if res["stopped"] == true {
			fmt.Fprintln(out, "stopped")
		} else {
			fmt.Fprintln(out, "not running")
		}
		return nil
	}
	info, err := ui.StartOrReuseIdle(o.open, 0, o.idle)
	if err != nil {
		return fail(err)
	}
	if o.json {
		return writeJSONLine(out, info)
	}
	fmt.Fprintln(out, info.URL)
	return nil
}
