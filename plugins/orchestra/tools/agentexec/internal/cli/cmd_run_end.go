package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/ledger"
)

// runEndExec runs agent-exec itself with args in the current directory,
// with empty stdin, and returns the child's captured stdout, stderr and
// exit code. err is non-nil only when the child could not start.
var runEndExec = runEndExecChild

func runEndExecChild(args []string) (stdout, stderr []byte, code int, err error) {
	// The test binary is os.Executable(): starting it would re-run the suite.
	if testing.Testing() {
		return nil, nil, 0, errors.New("run-end: no child processes in a test binary")
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, nil, 0, err
	}
	var outBuf, errBuf bytes.Buffer
	c := exec.Command(exe, args...)
	c.Stdout = &outBuf
	c.Stderr = &errBuf
	runErr := c.Run()
	if runErr == nil {
		return outBuf.Bytes(), errBuf.Bytes(), 0, nil
	}
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		return outBuf.Bytes(), errBuf.Bytes(), ee.ExitCode(), nil
	}
	return nil, nil, 0, runErr
}

// runEndSteps are the end-of-run steps, in the order they run. %s is the run id.
var runEndSteps = []struct {
	name string
	args func(id string) []string
}{
	{"sweep", func(string) []string { return []string{"isolate", "sweep", "--include-current", "--text"} }},
	{"usage", func(id string) []string { return []string{"usage", "--run", id, "--text"} }},
	{"run-summary", func(id string) []string { return []string{"telemetry", "run-summary", "--run", id} }},
}

func newRunEndCmd() *cobra.Command {
	var id string
	cmd := &cobra.Command{
		Use:   "run-end",
		Short: "end a run: sweep, cost report and run summary",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if id == "" {
				return exitf(2, "run-end: --run is required")
			}
			if !ledger.RunIDPattern.MatchString(id) {
				return exitf(2, "run-end: invalid --run %q", id)
			}
			var failed []string
			out := cmd.OutOrStdout()
			for _, step := range runEndSteps {
				stdout, stderr, code, err := runEndExec(step.args(id))
				if err != nil {
					// The step could not start: exit 127, the start error as its stderr.
					stderr, code = []byte(err.Error()), 127
				}
				if code != 0 {
					failed = append(failed, step.name)
				}
				fmt.Fprintf(out, "== %s (exit %d)\n", step.name, code)
				writeLine(out, stdout)
				if len(stderr) > 0 {
					fmt.Fprint(out, "-- stderr\n")
					writeLine(out, stderr)
				}
			}
			if len(failed) == 0 {
				fmt.Fprint(out, "run-end: ok\n")
				return nil
			}
			fmt.Fprintf(out, "run-end: failed: %s\n", strings.Join(failed, ", "))
			return &ExitError{Code: 1}
		},
	}
	cmd.Flags().StringVar(&id, "run", "", "run id (required)")
	return cmd
}

// writeLine writes b, adding a final newline when b is non-empty and lacks one.
func writeLine(w io.Writer, b []byte) {
	if len(b) == 0 {
		return
	}
	w.Write(b)
	if b[len(b)-1] != '\n' {
		io.WriteString(w, "\n")
	}
}
