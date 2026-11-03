package cli

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// runEndResult is what the fake child returns for one step.
type runEndResult struct {
	stdout, stderr string
	code           int
	err            error
}

// runEndFake replaces runEndExec for the test and records every child call.
// Steps are keyed by their joined args; an unlisted step exits 0 with no output.
func runEndFake(t *testing.T, results map[string]runEndResult) *[][]string {
	t.Helper()
	calls := &[][]string{}
	prev := runEndExec
	runEndExec = func(args []string) ([]byte, []byte, int, error) {
		*calls = append(*calls, args)
		r := results[strings.Join(args, " ")]
		return []byte(r.stdout), []byte(r.stderr), r.code, r.err
	}
	t.Cleanup(func() { runEndExec = prev })
	return calls
}

func runEndRun(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(append([]string{"run-end"}, args...), &out, &errb)
	return out.String(), errb.String(), code
}

const (
	runEndSweepArgs   = "isolate sweep --include-current --text"
	runEndUsageArgs   = "usage --run r1 --text"
	runEndSummaryArgs = "telemetry run-summary --run r1"
)

func TestRunEndStepsRunInOrderAndContinueAfterFailure(t *testing.T) {
	calls := runEndFake(t, map[string]runEndResult{
		runEndSweepArgs:   {stdout: "swept\n", code: 1, stderr: "boom"},
		runEndUsageArgs:   {err: errors.New("no exe")},
		runEndSummaryArgs: {stdout: "{}\n"},
	})

	stdout, stderr, code := runEndRun(t, "--run", "r1")

	want := [][]string{
		{"isolate", "sweep", "--include-current", "--text"},
		{"usage", "--run", "r1", "--text"},
		{"telemetry", "run-summary", "--run", "r1"},
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("calls = %v, want %v", *calls, want)
	}
	wantOut := "== sweep (exit 1)\nswept\n-- stderr\nboom\n" +
		"== usage (exit 127)\n-- stderr\nno exe\n" +
		"== run-summary (exit 0)\n{}\n" +
		"run-end: failed: sweep, usage\n"
	if stdout != wantOut {
		t.Errorf("stdout = %q, want %q", stdout, wantOut)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
}

func TestRunEndAllOKOutputIsExact(t *testing.T) {
	runEndFake(t, map[string]runEndResult{
		runEndSweepArgs:   {stdout: "swept\n"},
		runEndUsageArgs:   {stdout: "usage total"},
		runEndSummaryArgs: {stdout: "{\"recorded\":true}\n"},
	})

	stdout, stderr, code := runEndRun(t, "--run", "r1")

	wantOut := "== sweep (exit 0)\nswept\n" +
		"== usage (exit 0)\nusage total\n" +
		"== run-summary (exit 0)\n{\"recorded\":true}\n" +
		"run-end: ok\n"
	if stdout != wantOut {
		t.Errorf("stdout = %q, want %q", stdout, wantOut)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
}

func TestRunEndOnlyRunSummaryFailingReportsItself(t *testing.T) {
	runEndFake(t, map[string]runEndResult{
		runEndSummaryArgs: {code: 3},
	})

	stdout, stderr, code := runEndRun(t, "--run", "r1")

	if !strings.HasSuffix(stdout, "run-end: failed: run-summary\n") {
		t.Errorf("stdout = %q, want last line %q", stdout, "run-end: failed: run-summary")
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
}

func TestRunEndInvalidRunIDIsRejectedBeforeAnyStep(t *testing.T) {
	cases := []struct {
		name string
		args []string
		msg  string
	}{
		{"missing", []string{}, "agent-exec: run-end: --run is required\n"},
		{"bad characters", []string{"--run", "bad/id"}, "agent-exec: run-end: invalid --run \"bad/id\"\n"},
		{"too long", []string{"--run", strings.Repeat("a", 65)}, "agent-exec: run-end: invalid --run \"" + strings.Repeat("a", 65) + "\"\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := runEndFake(t, nil)

			stdout, stderr, code := runEndRun(t, tc.args...)

			if code != 2 {
				t.Errorf("exit = %d, want 2", code)
			}
			if stderr != tc.msg {
				t.Errorf("stderr = %q, want %q", stderr, tc.msg)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if len(*calls) != 0 {
				t.Errorf("child calls = %v, want none", *calls)
			}
		})
	}
}

func TestRunEndAccepts64CharRunID(t *testing.T) {
	id := strings.Repeat("a", 64)
	calls := runEndFake(t, nil)

	_, _, code := runEndRun(t, "--run", id)

	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if len(*calls) != 3 {
		t.Errorf("child calls = %d, want 3", len(*calls))
	}
}

func TestRunEndRejectsExtraArgs(t *testing.T) {
	calls := runEndFake(t, nil)

	_, _, code := runEndRun(t, "--run", "r1", "extra")

	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if len(*calls) != 0 {
		t.Errorf("child calls = %v, want none", *calls)
	}
}
