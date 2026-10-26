package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
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

// hookCtxTranscript writes a subagent transcript for session "s1" under a
// temporary CLAUDE_CONFIG_DIR and points the command at it.
func hookCtxTranscript(t *testing.T, lines string) string {
	t.Helper()
	hookCmdIsolate(t)
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	sub := filepath.Join(dir, "projects", "p", "s1", "subagents")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(sub, "agent-a1.jsonl")
	if err := os.WriteFile(file, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestHookCmdContextReportsLastSubagentResponse(t *testing.T) {
	file := hookCtxTranscript(t, `{"message":{"usage":{"input_tokens":1,"cache_read_input_tokens":2,"cache_creation_input_tokens":3}}}
{"message":{"usage":{"input_tokens":10,"cache_read_input_tokens":20,"cache_creation_input_tokens":30}}}
`)
	out, code := hookCmdRun(t, "", "hook", "context", "--session", "s1", "--agent", "a1")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var got struct {
		ContextTokens *int64  `json:"context_tokens"`
		Transcript    *string `json:"transcript"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if got.ContextTokens == nil || *got.ContextTokens != 60 {
		t.Errorf("context_tokens = %v; want 60 (last line only)", got.ContextTokens)
	}
	if got.Transcript == nil || *got.Transcript != file {
		t.Errorf("transcript = %v; want %q", got.Transcript, file)
	}
}

func TestHookCmdContextUnknownAgentIsNull(t *testing.T) {
	hookCtxTranscript(t, `{"message":{"usage":{"input_tokens":1}}}`)
	out, code := hookCmdRun(t, "", "hook", "context", "--session", "s1", "--agent", "nope")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if want := `{"context_tokens":null,"transcript":null}` + "\n"; out != want {
		t.Errorf("output %q; want %q", out, want)
	}
}

func TestHookCmdContextInvalidIDIsUsageError(t *testing.T) {
	hookCmdIsolate(t)
	if _, code := hookCmdRun(t, "", "hook", "context"); code != 2 {
		t.Errorf("missing --session: exit %d; want 2", code)
	}
	if _, code := hookCmdRun(t, "", "hook", "context", "--session", "a/b"); code != 2 {
		t.Errorf("--session with / : exit %d; want 2", code)
	}
}
