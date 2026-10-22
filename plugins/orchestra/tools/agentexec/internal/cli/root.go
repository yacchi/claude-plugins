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

	_ "github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/capture" // registers the executor env with sandbox
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
	rejectRepeats(root, args)
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	cmd, err := root.ExecuteC()
	return report(cmd, err, stderr)
}

// report prints err and returns the exit code.
func report(cmd *cobra.Command, err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	ee := asExitError(cmd, err)
	if ee.Msg != "" {
		fmt.Fprintf(stderr, "agent-exec: %s\n", ee.Msg)
	}
	return ee.Code
}

// asExitError passes an ExitError through. Any other error comes from cobra
// itself (an unknown flag or subcommand, a bad flag value, a missing
// required or conflicting flag): a usage error, exit 2, named after cmd.
func asExitError(cmd *cobra.Command, err error) *ExitError {
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee
	}
	return &ExitError{Code: 2, Msg: cmdName(cmd) + ": " + err.Error()}
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

	isolate := groupCommand("isolate", "per-task git worktrees")
	addIsolateCoreCommands(isolate)
	addIsolateExtCommands(isolate)

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
		newWaveCmd(),
		newApprovalsCmd(),
		newUICmd(),
		newPiCmd(),
		newSandboxExecCmd(),
		newTraceCmd(),
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

// cmdName is the command path below the root ("isolate sweep"); it prefixes
// the command's usage errors.
func cmdName(cmd *cobra.Command) string {
	if cmd == nil {
		return "agent-exec"
	}
	return strings.TrimPrefix(cmd.CommandPath(), "agent-exec ")
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
				return exitf(2, "%s: unknown subcommand: %s", cmdName(cmd), args[0])
			}
			cmd.SetOut(cmd.ErrOrStderr())
			_ = cmd.Usage()
			return &ExitError{Code: 2}
		},
	}
}
