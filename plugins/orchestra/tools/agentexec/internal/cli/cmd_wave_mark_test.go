package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testStart is about when this test process started (runner.pid's
// started_at may be a few seconds off).
var testStart = time.Now().Unix()

const markState = `{"version": 1, "plan": "plan.json", "wave": 1,
 "integration": {"task": "wave-int", "path": null, "base": null},
 "needs": [{"id": "B", "kind": "escalate", "detail": "ESCALATE: unclear", "at": 1.5}],
 "packages": {"B": {"status": "needs", "since": 1.5, "attempts": 0, "tree": null, "executor": "pi",
  "session": false, "files_changed": null, "commit": null, "detail": "ESCALATE: unclear"}},
 "stopped": null, "updated": 1.5}
`

func runWaveMark(t *testing.T, args ...string) (map[string]any, int) {
	t.Helper()
	var buf bytes.Buffer
	cmd := newWaveMarkCmd()
	cmd.SetArgs(args)
	cmd.SetOut(&buf)
	cmd.SetErr(io.Discard)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	if err := cmd.Execute(); err != nil {
		return nil, asExitError(cmd, err).Code
	}
	var res map[string]any
	if err := json.Unmarshal(buf.Bytes(), &res); err != nil {
		t.Fatalf("output %q: %v", buf.String(), err)
	}
	return res, 0
}

func TestWaveMarkSaysWhenTheRunnerIsGone(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	pidPath := filepath.Join(dir, "runner.pid")
	writeRecord := func(pid int, startedAt int64) {
		t.Helper()
		rec := fmt.Sprintf(`{"pid": %d, "started_at": %d, "argv": ["agent-exec", "wave", "run", "--state", %q]}`,
			pid, startedAt, statePath)
		if err := os.WriteFile(pidPath, []byte(rec), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mark := func(record func()) map[string]any {
		t.Helper()
		if err := os.WriteFile(statePath, []byte(markState), 0o644); err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(pidPath)
		record()
		res, code := runWaveMark(t, "--state", statePath, "--pkg", "B", "--status", "ready")
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
		return res
	}
	want := func(what string, got map[string]any, keys ...string) {
		t.Helper()
		if got["pkg"] != "B" || got["status"] != "verifying" || len(got) != 2+len(keys) {
			t.Errorf("%s: %v", what, got)
		}
		for _, k := range keys {
			if _, ok := got[k]; !ok {
				t.Errorf("%s: no %q in %v", what, k, got)
			}
		}
	}

	// No runner.pid: liveness unknown, the output is unchanged.
	want("no record", mark(func() {}))

	// A record whose process has ended: the runner is gone.
	gone := mark(func() { writeRecord(999999, 1) })
	want("gone", gone, "runner", "hint")
	if gone["runner"] != "gone" ||
		gone["hint"] != "no runner is running this state; start the same wave run command again to pick it up" {
		t.Errorf("gone: %v", gone)
	}

	// A live runner of this state (this process): nothing added.
	want("alive", mark(func() { writeRecord(os.Getpid(), testStart) }))

	// A failed mark keeps its exit code.
	writeRecord(999999, 1)
	if _, code := runWaveMark(t, "--state", statePath, "--pkg", "ZZ", "--status", "ready"); code != 2 {
		t.Errorf("unknown package: exit %d", code)
	}
}
