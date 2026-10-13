package orca

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
)

var fakeOrca string

// TestMain builds the fake once; invoked through an `orca` link it speaks
// the orca CLI.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fakeorca-bin-")
	if err != nil {
		panic(err)
	}
	bin := filepath.Join(dir, "fakeherdr")
	build := exec.Command("go", "build", "-o", bin, "../../testutil/fakeherdr")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		panic(err)
	}
	fakeOrca = filepath.Join(dir, "orca")
	if err := os.Symlink(bin, fakeOrca); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type env struct {
	t                             *testing.T
	tmp, repo, state, fake, result string
	mu                            sync.Mutex
	now                           time.Time
	events                        []map[string]any
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func newEnv(t *testing.T, scenario map[string]any) *env {
	tmp, _ := filepath.EvalSymlinks(t.TempDir())
	e := &env{t: t, tmp: tmp, repo: filepath.Join(tmp, "repo"), state: filepath.Join(tmp, "state"),
		fake: filepath.Join(tmp, "fake"), now: time.Unix(1000, 0)}
	for _, d := range []string{e.repo, e.state, e.fake} {
		_ = os.MkdirAll(d, 0o755)
	}
	e.result = filepath.Join(e.state, "orca", "A.result.json")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "abcdef12-0000")
	t.Setenv("HOME", filepath.Join(tmp, "home"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "xstate"))
	t.Setenv("ORCHESTRA_ALIVE_DIR", filepath.Join(tmp, "alive"))
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("FAKEHERDR_DIR", e.fake)
	t.Setenv("AGENT_EXEC_BOOTSTRAP", "")
	gitRun(t, e.repo, "init", "-q", "-b", "main", ".")
	gitRun(t, e.repo, "config", "user.email", "t@example.com")
	gitRun(t, e.repo, "config", "user.name", "T")
	_ = os.WriteFile(filepath.Join(e.repo, "a.txt"), []byte("one\n"), 0o644)
	gitRun(t, e.repo, "add", "-A")
	gitRun(t, e.repo, "commit", "-q", "-m", "init")
	e.scenario(scenario)
	return e
}

func (e *env) scenario(sc map[string]any) {
	b, _ := json.Marshal(sc)
	_ = os.WriteFile(filepath.Join(e.fake, "scenario.json"), b, 0o644)
}

func (e *env) executor(cfg string) *Executor {
	if cfg == "" {
		cfg = "{}"
	}
	v, err := jsonx.Decode([]byte(cfg))
	if err != nil {
		e.t.Fatal(err)
	}
	x := New(&config.Resolved{Values: jsonx.DeepMerge(config.Defaults(), v.(*jsonx.Object))})
	x.Binary = fakeOrca
	x.Stderr = &bytes.Buffer{}
	x.Clock = host.Clock{
		Now: func() time.Time {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.now
		},
		Sleep: func(ctx context.Context, d time.Duration) error {
			e.mu.Lock()
			e.now = e.now.Add(max(d, time.Second))
			e.mu.Unlock()
			return ctx.Err()
		},
	}
	x.SetEventFunc(func(pkg string, d map[string]any) {
		d["pkg"] = pkg
		e.events = append(e.events, d)
	})
	return x
}

func (e *env) calls() [][]string {
	f, err := os.Open(filepath.Join(e.fake, "calls.jsonl"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out [][]string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var c struct{ Argv []string }
		if json.Unmarshal(sc.Bytes(), &c) == nil {
			out = append(out, c.Argv)
		}
	}
	return out
}

func (e *env) callsOf(verb string) [][]string {
	var out [][]string
	for _, c := range e.calls() {
		if len(c) >= 2 && c[0]+" "+c[1] == verb {
			out = append(out, c)
		}
	}
	return out
}

func opt(argv []string, flag string) string {
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

func (e *env) start(x *Executor) (*host.Session, error) {
	return x.Start(context.Background(), "A", e.repo, "main", "standard", "opus", "run-1", e.state, []string{e.state})
}

func needOf(t *testing.T, err error) *host.NeedError {
	t.Helper()
	n, ok := err.(*host.NeedError)
	if !ok {
		t.Fatalf("expected a need, got %T %v", err, err)
	}
	return n
}

func ok(summary string, files map[string]any, afterWaits int) map[string]any {
	return map[string]any{"files": files, "result": map[string]any{"status": "ok", "summary": summary}, "after_waits": afterWaits}
}

func TestClaudePackageRunsInOrcaAndIsAdopted(t *testing.T) {
	e := newEnv(t, map[string]any{"prompts": map[string]any{"a": []any{ok("done", map[string]any{"b.txt": "two\n"}, 1)}}})
	x := e.executor("")
	s, err := e.start(x)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(s.Worktree, "wave-run1-a") || s.Terminal != "term-1" {
		t.Fatalf("%+v", s)
	}
	if wt, _, ok := isolate.ResolveWorktree(e.repo, "pkg-A", ""); !ok || wt != s.Worktree {
		t.Errorf("not adopted: %s", wt)
	}
	out, err := host.PromptAndWait(context.Background(), x, s, []string{"/p/one.md"}, e.result, 60e9)
	if err != nil {
		t.Fatal(err)
	}
	if out["executor"] != "orca" || out["answer"] != "done" || out["session_id"] != "term-1" || out["resumed"] != false {
		t.Errorf("%v", out)
	}
	if b, _ := os.ReadFile(filepath.Join(s.Worktree, "b.txt")); string(b) != "two\n" {
		t.Error("files not written in the worktree")
	}
	rec := x.reg.Load(e.state)["A"].(map[string]any)
	if ids, _ := rec["request_ids"].([]any); len(ids) != 1 || rec["terminal"] != "term-1" {
		t.Errorf("record %v", rec)
	}
	create := e.callsOf("worktree create")[0]
	if opt(create, "--setup") != "skip" || opt(create, "--base-branch") != "main" || opt(create, "--name") != "wave-run1-a" {
		t.Error(create)
	}
	_ = x.Close(s, true)
	if _, ok := x.reg.Load(e.state)["A"]; ok {
		t.Error("registry kept the closed package")
	}
	if _, _, ok := isolate.ResolveWorktree(e.repo, "pkg-A", ""); ok {
		t.Error("still adopted")
	}
	if len(e.callsOf("worktree rm")) != 1 || len(e.callsOf("terminal close")) != 1 {
		t.Error("not removed through orca")
	}
}

func TestAddDirsAndPermissionModeAreRendered(t *testing.T) {
	e := newEnv(t, map[string]any{})
	x := e.executor(`{"orca": {"permission_mode": "acceptEdits", "user_config": "inherit"}}`)
	if _, err := e.start(x); err != nil {
		t.Fatal(err)
	}
	cmd := opt(e.callsOf("terminal create")[0], "--command")
	if cmd != "claude --model opus --permission-mode acceptEdits --add-dir "+e.state {
		t.Errorf("%q", cmd)
	}
}

func TestInvalidSetupAndPermissionModesAreRejected(t *testing.T) {
	e := newEnv(t, map[string]any{})
	_, err := e.start(e.executor(`{"orca": {"setup": "always"}}`))
	if n := needOf(t, err); n.Detail != "invalid orca.setup: always" || len(e.callsOf("worktree create")) != 0 {
		t.Error(n)
	}
	for cfg, want := range map[string]string{
		`{"orca": {"permission_mode": "yolo"}}`:                                   "invalid orca.permission_mode: yolo",
		`{"orca": {"permission_mode": "bypassPermissions"}}`:                      "invalid orca.permission_mode: bypassPermissions",
		`{"orca": {"command": "claude --dangerously-skip-permissions --model x"}}`: "orca permission bypass is not allowed",
	} {
		_, err := e.start(e.executor(cfg))
		if n := needOf(t, err); n.Detail != want || n.Kind != host.KindError {
			t.Error(cfg, n)
		}
	}
	if len(e.callsOf("terminal create")) != 0 {
		t.Error("a terminal opened")
	}
	if out := gitRun(t, e.repo, "worktree", "list"); strings.Contains(out, "wave-") {
		t.Errorf("worktree left: %s", out)
	}
}

func TestTrustDialog(t *testing.T) {
	e := newEnv(t, map[string]any{"trust": true})
	_, err := e.start(e.executor(""))
	if n := needOf(t, err); n.Kind != host.KindTrust || len(e.callsOf("terminal send")) != 0 {
		t.Error(n)
	}
	e2 := newEnv(t, map[string]any{"trust": true})
	s, err := e2.start(e2.executor(`{"orca": {"auto_trust": true}}`))
	if err != nil {
		t.Fatal(err)
	}
	sends := e2.callsOf("terminal send")
	if len(sends) != 2 || opt(sends[0], "--text") != down || !contains(sends[1], "--enter") || contains(sends[0], "--enter") {
		t.Errorf("sends %q", sends)
	}
	if s.Terminal == "" || e2.events[0]["action"] != "trust" {
		t.Error(e2.events)
	}
}

func TestStartupTimeoutIsStalled(t *testing.T) {
	e := newEnv(t, map[string]any{"never_ready": true})
	_, err := e.start(e.executor(`{"orca": {"startup_timeout": 5}}`))
	if n := needOf(t, err); n.Kind != host.KindStalled || !strings.Contains(n.Detail, "no Claude prompt after 5s") {
		t.Error(n)
	}
}

func TestWaits(t *testing.T) {
	// no result: timeout; premature idle keeps waiting; a choice dialog stalls
	e := newEnv(t, map[string]any{"prompts": map[string]any{"a": []any{map[string]any{}}}})
	x := e.executor("")
	s, _ := e.start(x)
	_, err := host.PromptAndWait(context.Background(), x, s, []string{"/p/one.md"}, e.result, 10e9)
	if n := needOf(t, err); n.Kind != host.KindTimeout || !strings.HasPrefix(n.Detail, "no result after 10s:") {
		t.Error(n)
	}
	e = newEnv(t, map[string]any{"prompts": map[string]any{"a": []any{ok("late", nil, 3)}}})
	x = e.executor("")
	s, _ = e.start(x)
	if out, err := host.PromptAndWait(context.Background(), x, s, []string{"/p/one.md"}, e.result, 60e9); err != nil || out["answer"] != "late" {
		t.Error(out, err)
	}
	e = newEnv(t, map[string]any{"prompts": map[string]any{"a": []any{map[string]any{"dialog": true}}}})
	x = e.executor("")
	s, _ = e.start(x)
	_, err = host.PromptAndWait(context.Background(), x, s, []string{"/p/one.md"}, e.result, 60e9)
	n := needOf(t, err)
	if n.Kind != host.KindStalled || !strings.HasPrefix(n.Detail, "dialog:\nDo you want to make this edit to a.txt?\n❯ 1. Yes\n  2. No\nterminal: term-1") {
		t.Errorf("%q", n.Detail)
	}
	// escalate via the result file
	e = newEnv(t, map[string]any{"prompts": map[string]any{"a": []any{map[string]any{"result": map[string]any{"status": "escalate", "summary": "no"}}}}})
	x = e.executor("")
	s, _ = e.start(x)
	if out, _ := host.PromptAndWait(context.Background(), x, s, []string{"/p/one.md"}, e.result, 60e9); out["answer"] != "ESCALATE: no" {
		t.Error(out)
	}
}

func TestResumeReusesALiveTerminalAndReplacesADeadOne(t *testing.T) {
	e := newEnv(t, map[string]any{"prompts": map[string]any{"a": []any{ok("one", nil, 0), ok("two", nil, 0), ok("three", nil, 0)}}})
	x := e.executor("")
	s, _ := e.start(x)
	if _, err := host.PromptAndWait(context.Background(), x, s, []string{"/p/one.md"}, e.result, 60e9); err != nil {
		t.Fatal(err)
	}
	y := e.executor("")
	again, found := y.Session("A", e.state)
	if !found || again.Terminal != "term-1" {
		t.Fatalf("%+v", again)
	}
	out, err := host.PromptAndWait(context.Background(), y, again, []string{"/p/two.md"}, e.result, 60e9)
	if err != nil || out["resumed"] != true || out["answer"] != "two" || len(e.callsOf("terminal create")) != 1 {
		t.Fatalf("%v %v", out, err)
	}
	// the terminal is gone: Session drops it and the next prompt opens one
	_, _ = y.call(context.Background(), callTimeout, "terminal", "close", "--terminal", "term-1", "--tab")
	gone, found := y.Session("A", e.state)
	if !found || gone.Terminal != "" {
		t.Fatalf("%+v", gone)
	}
	out, err = host.PromptAndWait(context.Background(), y, gone, []string{"/p/three.md"}, e.result, 60e9)
	if err != nil || out["resumed"] != false || out["session_id"] != "term-2" {
		t.Errorf("%v %v", out, err)
	}
}

func TestATerminalWhoseClaudeExitedIsNotReused(t *testing.T) {
	e := newEnv(t, map[string]any{"prompts": map[string]any{"a": []any{ok("one", nil, 0), ok("two", nil, 0)}}})
	x := e.executor("")
	s, _ := e.start(x)
	_, _ = host.PromptAndWait(context.Background(), x, s, []string{"/p/one.md"}, e.result, 60e9)
	// Claude exited: the terminal shows a bare shell
	state := map[string]any{}
	b, _ := os.ReadFile(filepath.Join(e.fake, "state.json"))
	_ = json.Unmarshal(b, &state)
	state["terminals"].(map[string]any)["term-1"].(map[string]any)["exited"] = true
	b, _ = json.Marshal(state)
	_ = os.WriteFile(filepath.Join(e.fake, "state.json"), b, 0o644)
	out, err := host.PromptAndWait(context.Background(), x, s, []string{"/p/two.md"}, e.result, 60e9)
	if err != nil || out["resumed"] != false || out["session_id"] != "term-2" || len(e.callsOf("terminal close")) != 1 {
		t.Errorf("%v %v", out, err)
	}
}

func TestKeepSessionsOffClosesAfterEachPrompt(t *testing.T) {
	e := newEnv(t, map[string]any{"prompts": map[string]any{"a": []any{ok("one", nil, 0)}}})
	x := e.executor(`{"orca": {"keep_sessions": false}}`)
	s, _ := e.start(x)
	if _, err := host.PromptAndWait(context.Background(), x, s, []string{"/p/one.md"}, e.result, 60e9); err != nil {
		t.Fatal(err)
	}
	if s.Terminal != "" || len(e.callsOf("terminal close")) != 1 {
		t.Error("terminal not closed")
	}
}

func TestAvailability(t *testing.T) {
	e := newEnv(t, map[string]any{})
	if ok, why := e.executor("").Available(); !ok {
		t.Error(why)
	}
	if ok, why := e.executor(`{"orca": {"enabled": false}}`).Available(); ok || why != "disabled by config (orca.enabled: false)" {
		t.Error(why)
	}
	x := e.executor("")
	x.Binary = "no-such-orca-binary"
	if ok, why := x.Available(); ok || why != "no-such-orca-binary is not on PATH" {
		t.Error(why)
	}
	e.scenario(map[string]any{"status_ok": false})
	if ok, why := e.executor("").Available(); ok || why != "orca status not ok: orca status failed: runtime not running" {
		t.Error(why)
	}
	if e.executor(`{"orca": {"models": {"light": "haiku"}}}`).ModelFor("light") != "haiku" || e.executor("").ModelFor("x") != "opus" {
		t.Error("models")
	}
	if !reflect.DeepEqual(e.executor("").Settings().AddDirs, []string(nil)) && len(e.executor("").Settings().AddDirs) != 0 {
		t.Error("add_dirs")
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
