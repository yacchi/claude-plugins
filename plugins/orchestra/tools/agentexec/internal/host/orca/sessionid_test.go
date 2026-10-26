package orca

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/transcript"
)

var sessionIDFlag = regexp.MustCompile(`--session-id ([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)

func TestSessionIDHostedClaudeGetsAKnownIDRecordedForItsRun(t *testing.T) {
	e := newEnv(t, map[string]any{})
	x := e.executor("")
	s, err := e.start(x)
	if err != nil {
		t.Fatal(err)
	}
	cmd := opt(e.callsOf("terminal create")[0], "--command")
	m := sessionIDFlag.FindStringSubmatch(cmd)
	if m == nil {
		t.Fatalf("no --session-id <uuid> in %q", cmd)
	}
	if got := s.Str("claude_session_id"); got != m[1] {
		t.Errorf("claude_session_id = %q; command carries %q", got, m[1])
	}
	if got := transcript.HostedSessions("run-1"); !slices.Equal(got, []string{m[1]}) {
		t.Errorf("HostedSessions(run-1) = %v; want [%s]", got, m[1])
	}
}

func TestSessionIDReopenGetsAFreshIDRecordedAfterTheFirst(t *testing.T) {
	e := newEnv(t, map[string]any{"prompts": map[string]any{"a": []any{ok("one", nil, 0)}}})
	x := e.executor("")
	s, err := e.start(x)
	if err != nil {
		t.Fatal(err)
	}
	first := s.Str("claude_session_id")

	y := e.executor("")
	_, _ = y.call(context.Background(), callTimeout, "terminal", "close", "--terminal", "term-1", "--tab")
	gone, found := y.Session("A", e.state)
	if !found || gone.Terminal != "" {
		t.Fatalf("%+v", gone)
	}
	if _, err := host.PromptAndWait(context.Background(), y, gone, []string{"/p/one.md"}, e.result, 60e9); err != nil {
		t.Fatal(err)
	}
	creates := e.callsOf("terminal create")
	if len(creates) != 2 {
		t.Fatalf("terminal create ran %d times; want 2", len(creates))
	}
	m := sessionIDFlag.FindStringSubmatch(opt(creates[1], "--command"))
	if m == nil || m[1] == first {
		t.Fatalf("reopen command %v; want a new --session-id (first %q)", creates[1], first)
	}
	if got := transcript.HostedSessions("run-1"); !slices.Equal(got, []string{first, m[1]}) {
		t.Errorf("HostedSessions(run-1) = %v; want [%s %s]", got, first, m[1])
	}
}

func TestSessionIDTemplateWithItsOwnIDIsLeftAlone(t *testing.T) {
	e := newEnv(t, map[string]any{})
	x := e.executor(`{"orca": {"command": "claude --model {model} --permission-mode {permission_mode} --resume 11111111-1111-4111-8111-111111111111"}}`)
	s, err := e.start(x)
	if err != nil {
		t.Fatal(err)
	}
	cmd := opt(e.callsOf("terminal create")[0], "--command")
	if strings.Contains(cmd, "--session-id") {
		t.Errorf("template with --resume got a session id: %q", cmd)
	}
	if got := s.Str("claude_session_id"); got != "" {
		t.Errorf("claude_session_id = %q; want unset", got)
	}
	if got := transcript.HostedSessions("run-1"); got != nil {
		t.Errorf("HostedSessions(run-1) = %v; want nil", got)
	}
}
