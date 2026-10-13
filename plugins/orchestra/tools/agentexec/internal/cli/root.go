// Package cli is agent-exec's command tree: argument parsing, output format
// selection and exit codes. Each area lives in its own cmd_<area>.go; the
// domain work is done by the internal packages the commands call.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// ExitError ends the process with Code after printing "agent-exec: <Msg>" to
// stderr. An empty Msg prints nothing (for commands that wrote their own,
// differently prefixed, diagnostics).
type ExitError struct {
	Code int
	Msg  string
}

func (e *ExitError) Error() string { return e.Msg }

func exitf(code int, format string, args ...any) *ExitError {
	return &ExitError{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// Execute runs the command line and returns the process exit code.
func Execute() int {
	return run(os.Args[1:], os.Stdout, os.Stderr)
}

func run(args []string, stdout, stderr io.Writer) int {
	root := newRoot()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	return report(root.Execute(), stderr)
}

func report(err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	var ee *ExitError
	if !errors.As(err, &ee) {
		// cobra's own failures are usage errors.
		ee = &ExitError{Code: 2, Msg: err.Error()}
	}
	if ee.Msg != "" {
		fmt.Fprintf(stderr, "agent-exec: %s\n", ee.Msg)
	}
	return ee.Code
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:                "agent-exec",
		Short:              "dispatch external agent CLIs for orchestra",
		SilenceErrors:      true,
		SilenceUsage:       true,
		DisableSuggestions: true,
		Args:               cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return exitf(2, "unknown command: %s", args[0])
			}
			return cmd.Help()
		},
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			// A session is actively orchestrating in some repo: recording it
			// here keeps the one liveness signal `isolate sweep` uses in a
			// single place.
			switch topLevel(cmd).Name() {
			case "isolate", "dispatch", "route":
				touchHeartbeat()
			}
		},
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetFlagErrorFunc(flagError)

	isolate := groupCommand("isolate", "per-task git worktrees")
	addIsolateCoreCommands(isolate)
	addIsolateExtCommands(isolate)

	wave := groupCommand("wave", "the deterministic outer loop over a plan")
	addWaveStatusCommands(wave)
	addWaveRunCommands(wave)

	root.AddCommand(
		newInstallCmd(),
		newConfigCmd(),
		newDoctorCmd(),
		newRouteCmd(),
		newDispatchCmd(),
		newWriteFileCmd(),
		isolate,
		newShelfCmd(),
		newCheckCmd(),
		newUsageCmd(),
		newTelemetryCmd(),
		newSandboxCmd(),
		newToolsCmd(),
		wave,
		newApprovalsCmd(),
		newUICmd(),
		newPiCmd(),
		newSandboxExecCmd(),
	)
	root.SetHelpCommand(&cobra.Command{
		Use:    "help [command]",
		Hidden: true,
		Args:   cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			target, _, err := root.Find(args)
			if err != nil || target == nil {
				target = root
			}
			return target.Help()
		},
	})
	return root
}

func topLevel(cmd *cobra.Command) *cobra.Command {
	for cmd.HasParent() && cmd.Parent().HasParent() {
		cmd = cmd.Parent()
	}
	return cmd
}

// errPrefix is how a command names itself in usage errors, matching the
// Python parsers ("isolate" for the shared isolate parser, "dispatch" for
// dispatch prepare/wait, ...). Set with the "errprefix" annotation; the
// default is the command path below the root.
func errPrefix(cmd *cobra.Command) string {
	if p, ok := cmd.Annotations["errprefix"]; ok {
		return p
	}
	return strings.TrimPrefix(cmd.CommandPath(), "agent-exec ")
}

func withPrefix(cmd *cobra.Command, prefix string) *cobra.Command {
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations["errprefix"] = prefix
	return cmd
}

// flagError rewrites pflag's messages into the Python parsers' wording:
// "<cmd>: missing value for --x" and "<cmd>: unknown option: --x", exit 2.
func flagError(cmd *cobra.Command, err error) error {
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "flag needs an argument: "):
		flag := strings.TrimPrefix(msg, "flag needs an argument: ")
		if _, short, ok := strings.Cut(flag, " in "); ok {
			flag = short
		}
		return exitf(2, "%s: missing value for %s", errPrefix(cmd), flag)
	case strings.HasPrefix(msg, "unknown flag: "):
		return exitf(2, "%s: unknown option: %s", errPrefix(cmd), strings.TrimPrefix(msg, "unknown flag: "))
	case strings.HasPrefix(msg, "unknown shorthand flag: "):
		_, arg, _ := strings.Cut(msg, " in ")
		return exitf(2, "%s: unknown option: %s", errPrefix(cmd), arg)
	}
	return exitf(2, "%s: %s", errPrefix(cmd), msg)
}

// noArgs rejects positional arguments the way the Python parsers did: as an
// unknown option.
func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return exitf(2, "%s: unknown option: %s", errPrefix(cmd), args[0])
	}
	return nil
}

// groupCommand is a command that only holds subcommands: called bare it
// prints its usage to stderr and exits 2; with an unknown word it names it.
func groupCommand(use, short string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return exitf(2, "%s: unknown subcommand: %s", errPrefix(cmd), args[0])
			}
			cmd.SetOut(cmd.ErrOrStderr())
			_ = cmd.Usage()
			return &ExitError{Code: 2}
		},
	}
}

// notImplemented is the RunE of a command whose work package has not landed.
func notImplemented(name string) func(*cobra.Command, []string) error {
	return func(*cobra.Command, []string) error {
		return &ExitError{Code: 2, Msg: name + ": not implemented yet"}
	}
}
