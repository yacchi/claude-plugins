package hookrun

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
)

const (
	sessionStartRouterOpen = "<orchestra-router>\nClassify EACH user request"
	sessionStartGateLine   = "applies ONLY when"
)

// sessionStartInput is a SessionStart hook input; an empty string field is
// left out, so "absent" and "" are both expressible.
func sessionStartInput(t *testing.T, session, source, model, cwd string) []byte {
	t.Helper()
	in := map[string]string{"session_id": session, "source": source, "model": model, "cwd": cwd}
	for k, v := range in {
		if v == "" {
			delete(in, k)
		}
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// sessionStartContextOf is the additionalContext the hook injects for raw, or
// "" when it prints nothing.
func sessionStartContextOf(t *testing.T, raw []byte) string {
	t.Helper()
	out := SessionStart(raw, time.Unix(1_800_000_000, 0))
	if out == nil {
		return ""
	}
	var v struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if v.HookSpecificOutput.HookEventName != "SessionStart" {
		t.Fatalf("hookEventName = %q, want SessionStart", v.HookSpecificOutput.HookEventName)
	}
	return v.HookSpecificOutput.AdditionalContext
}

// sessionStartRepo is a throwaway repository with one commit, under a hermetic
// git config and temp state; heartbeats go to a temp dir.
func sessionStartRepo(t *testing.T) string {
	t.Helper()
	stampsTempState(t)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "T")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "T")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	isolate.SetAliveDir(t.TempDir())
	t.Cleanup(func() { isolate.SetAliveDir("") })

	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"add", "-A"},
		{"commit", "-q", "-m", "init"},
	} {
		if _, err := gitx.Run(repo, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	return repo
}

// sessionStartTaskTree plants a task worktree in repo and returns its path.
func sessionStartTaskTree(t *testing.T, repo string) string {
	t.Helper()
	res, err := isolate.Create(repo, "t1", isolate.CreateOptions{Backend: isolate.BackendGit, Session: "hookrun-session"})
	if err != nil || res.Status != "created" {
		t.Fatalf("create task tree: %+v, %v", res, err)
	}
	path, err := filepath.EvalSymlinks(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSessionStartModelGate(t *testing.T) {
	stampsTempState(t)
	cwd := t.TempDir()
	cases := []struct {
		name, model string
		want        string
	}{
		{"opus injects the router without the self-gate", "claude-opus-5", sessionStartRouterOpen},
		{"fable injects the router without the self-gate", "claude-fable-5", sessionStartRouterOpen},
		{"sonnet injects nothing", "claude-sonnet-5", ""},
		{"absent model injects the self-gated router", "", sessionStartGateLine},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sessionStartContextOf(t, sessionStartInput(t, "gate", "startup", tc.model, cwd))
			switch {
			case tc.want == "":
				if got != "" {
					t.Fatalf("injected %q, want nothing", got)
				}
			case !strings.Contains(got, tc.want):
				t.Fatalf("context lacks %q:\n%s", tc.want, got)
			}
			if tc.want == sessionStartRouterOpen && strings.Contains(got, sessionStartGateLine) {
				t.Fatalf("instructor model got the self-gate paragraph:\n%s", got)
			}
		})
	}
}

func TestSessionStartSources(t *testing.T) {
	stampsTempState(t)
	cwd := t.TempDir()
	for _, source := range []string{"resume", "clear", "compact"} {
		if got := sessionStartContextOf(t, sessionStartInput(t, "src", source, "claude-opus-5", cwd)); !strings.HasPrefix(got, sessionStartRouterOpen) {
			t.Errorf("source %s: context %q, want the router", source, got)
		}
	}
	for _, source := range []string{"fork", "other", ""} {
		if got := sessionStartContextOf(t, sessionStartInput(t, "src", source, "claude-opus-5", cwd)); got != "" {
			t.Errorf("source %q injected %q, want nothing", source, got)
		}
	}
}

func TestSessionStartRouterHasNoLoophole(t *testing.T) {
	stampsTempState(t)
	got := sessionStartContextOf(t, sessionStartInput(t, "loop", "startup", "claude-opus-5", t.TempDir()))
	if !strings.Contains(got, "A hook enforces this") {
		t.Errorf("router lacks the hook-enforcement sentence:\n%s", got)
	}
	if strings.Contains(got, "Small ORCHESTRATED work") {
		t.Errorf("router still carries the opt-in loophole:\n%s", got)
	}
}

func TestSessionStartTaskTreeGetsWorkerNotice(t *testing.T) {
	repo := sessionStartRepo(t)
	tree := sessionStartTaskTree(t, repo)
	for _, model := range []string{"claude-opus-5", "claude-sonnet-5", ""} {
		got := sessionStartContextOf(t, sessionStartInput(t, "worker", "startup", model, tree))
		if !strings.HasPrefix(got, "<orchestra-worker>\n") || !strings.Contains(got, "task worktree "+tree+".") {
			t.Errorf("model %q: context %q, want the worker notice naming %s", model, got, tree)
		}
	}
}

func TestSessionStartIntegrationTreeGetsRouter(t *testing.T) {
	repo := sessionStartRepo(t)
	tree := sessionStartTaskTree(t, repo)
	if err := isolate.WriteRole(tree, isolate.RoleIntegration); err != nil {
		t.Fatal(err)
	}
	got := sessionStartContextOf(t, sessionStartInput(t, "integ", "startup", "claude-opus-5", tree))
	if !strings.HasPrefix(got, sessionStartRouterOpen) {
		t.Errorf("integration tree: context %q, want the router", got)
	}
}

func TestSessionStartBlankCwdFallsBackToWorkingDirectory(t *testing.T) {
	repo := sessionStartRepo(t)
	tree := sessionStartTaskTree(t, repo)
	t.Chdir(tree)
	got := sessionStartContextOf(t, sessionStartInput(t, "blank", "startup", "claude-opus-5", ""))
	if !strings.HasPrefix(got, "<orchestra-worker>\n") {
		t.Errorf("blank cwd in a task tree: context %q, want the worker notice", got)
	}
}

func TestSessionStartGarbageInputInjectsNothing(t *testing.T) {
	stampsTempState(t)
	for _, raw := range []string{"", "not json", "[]", "42", "null"} {
		if out := SessionStart([]byte(raw), time.Unix(1_800_000_000, 0)); out != nil {
			t.Errorf("input %q printed %s, want nothing", raw, out)
		}
	}
}

func TestSessionStartRecordsStampWithoutInjection(t *testing.T) {
	stampsTempState(t)
	now := time.Unix(1_800_000_000, 0)
	if out := SessionStart(sessionStartInput(t, "stamped", "startup", "claude-sonnet-5", t.TempDir()), now); out != nil {
		t.Fatalf("sonnet session printed %s, want nothing", out)
	}
	if got := Seen("stamped")[EventSessionStart]; got != now.Unix() {
		t.Errorf("session-start stamp = %d, want %d", got, now.Unix())
	}
}
