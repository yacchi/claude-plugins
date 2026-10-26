package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/hookrun"
)

// hookCmdRun executes the agent-exec command line with stdin and returns
// stdout and the exit code the process would use.
func hookCmdRun(t *testing.T, stdin string, args ...string) (string, int) {
	t.Helper()
	root := newRoot()
	var stdout, stderr bytes.Buffer
	root.SetArgs(args)
	root.SetIn(strings.NewReader(stdin))
	root.SetOut(&stdout)
	root.SetErr(io.Discard)
	cmd, err := root.ExecuteC()
	return stdout.String(), report(cmd, err, &stderr)
}

// hookCmdIsolate points the state directory (where hookrun records stamps)
// at a temporary HOME.
func hookCmdIsolate(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", "")
}

func TestHookCmdSessionStartInjectsForOpus(t *testing.T) {
	hookCmdIsolate(t)
	in := `{"session_id":"s1","source":"startup","model":"claude-opus-5","cwd":"` + t.TempDir() + `"}`
	out, code := hookCmdRun(t, in, "hook", "session-start")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var line struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &line); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if !strings.HasPrefix(line.HookSpecificOutput.AdditionalContext, "<orchestra-router>") {
		t.Errorf("context does not start with <orchestra-router>: %q", line.HookSpecificOutput.AdditionalContext)
	}
}

func TestHookCmdSessionStartSilentForSonnet(t *testing.T) {
	hookCmdIsolate(t)
	in := `{"session_id":"s1","source":"startup","model":"claude-sonnet-5","cwd":"` + t.TempDir() + `"}`
	if out, code := hookCmdRun(t, in, "hook", "session-start"); code != 0 || out != "" {
		t.Errorf("exit %d, output %q; want exit 0 and no output", code, out)
	}
}

func TestHookCmdSessionStartExitsZeroOnBadInput(t *testing.T) {
	hookCmdIsolate(t)
	if out, code := hookCmdRun(t, "not json", "hook", "session-start"); code != 0 || out != "" {
		t.Errorf("exit %d, output %q; want exit 0 and no output", code, out)
	}
}

func TestHookCmdSeenRecordsEvent(t *testing.T) {
	hookCmdIsolate(t)
	if _, code := hookCmdRun(t, "", "hook", "seen", "--session", "s1", "--event", "module"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if _, ok := hookrun.Seen("s1")[hookrun.EventModule]; !ok {
		t.Errorf("Seen(s1) = %v; want %q recorded", hookrun.Seen("s1"), hookrun.EventModule)
	}
}

func TestHookCmdSeenMissingEventIsUsageError(t *testing.T) {
	hookCmdIsolate(t)
	if _, code := hookCmdRun(t, "", "hook", "seen", "--session", "s1"); code != 2 {
		t.Errorf("exit %d; want 2", code)
	}
}
