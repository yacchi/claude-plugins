package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/pilogin"
)

const piLoginUsage = `Usage:
  agent-exec pi login [--provider ID] [--json]
  agent-exec pi login --wait [--timeout SEC] [--json]
  agent-exec pi login --paste TEXT [--timeout SEC] [--json]
  agent-exec pi login --cancel [--json]
`

func newPiCmd() *cobra.Command {
	pi := groupCommand("pi", "pi helpers")
	// pi login parses its own arguments, as the Python parser did
	// ("agent-exec: pi login: <error>" and the usage on stderr, exit 2).
	login := &cobra.Command{
		Use:                "login",
		Short:              "log pi in through orchestra's own herdr",
		Args:               cobra.ArbitraryArgs,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPiLogin(cmd.OutOrStdout(), cmd.ErrOrStderr(), args)
		},
	}
	// Declared for --help only; DisableFlagParsing means they are never parsed.
	strFlag(login, "provider", "provider id")
	boolFlag(login, "wait", "poll until the login lands")
	strFlag(login, "paste", "send the final redirect URL")
	boolFlag(login, "cancel", "cancel a pending login")
	strFlag(login, "timeout", "seconds")
	boolFlag(login, "json", "JSON output (the default)")
	pi.AddCommand(login)
	return pi
}

// parsePiLoginArgs is (options, error text).
func parsePiLoginArgs(args []string) (pilogin.Options, string) {
	o := pilogin.Options{Mode: pilogin.ModeStart, Timeout: pilogin.DefaultTimeout}
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; arg {
		case "--provider", "--paste", "--timeout":
			if i+1 >= len(args) {
				return o, arg + " needs a value"
			}
			value := args[i+1]
			i++
			switch arg {
			case "--provider":
				o.Provider = value
			case "--paste":
				o.Mode, o.Paste = pilogin.ModePaste, value
			default:
				v, ok := pyFloat(value)
				if !ok {
					return o, "--timeout needs a number"
				}
				o.Timeout = v
			}
		case "--wait":
			o.Mode = pilogin.ModeWait
		case "--cancel":
			o.Mode = pilogin.ModeCancel
		case "--json":
		default:
			return o, "unknown argument: " + arg
		}
	}
	return o, ""
}

func runPiLogin(out, errw io.Writer, args []string) error {
	o, msg := parsePiLoginArgs(args)
	if msg != "" {
		fmt.Fprintf(errw, "agent-exec: pi login: %s\n%s", msg, piLoginUsage)
		return &ExitError{Code: 2}
	}
	o.Cfg = configOrDefaults()
	result, code := pilogin.Run(o)
	if err := writeJSONLine(out, result); err != nil {
		return err
	}
	if code != 0 {
		return &ExitError{Code: code}
	}
	return nil
}
