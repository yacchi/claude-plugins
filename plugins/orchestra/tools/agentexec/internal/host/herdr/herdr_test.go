package herdr

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/sandbox"
)

func TestEveryCallCarriesTheSessionAndTheServerEnvIsClean(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{"prompt_result": map[string]any{"status": "ok", "summary": "done"}}, true)
	x := e.executor("")
	s := e.mustStart(x, "a")
	out, err := host.PromptAndWait(context.Background(), x, s, []string{"/p/one.md"}, e.result, 60e9)
	if err != nil {
		t.Fatal(err)
	}
	_ = x.Close(s, true)
	x.Shutdown()
	if out["executor"] != "herdr" || out["status"] != "ok" || out["answer"] != "done" || out["resumed"] != false {
		t.Errorf("out = %v", out)
	}
	calls := e.calls()
	if len(calls) == 0 {
		t.Fatal("no calls")
	}
	for _, c := range calls {
		if !reflect.DeepEqual(c.Argv[:2], []string{"--session", session}) {
			t.Errorf("call without the session: %v", c.Argv)
		}
		// every herdr process, not only the server, runs in orchestra's home
		if c.Env["XDG_CONFIG_HOME"] != e.home || c.Env["CLAUDECODE"] != nil || c.Env["CLAUDE_CODE_SESSION_ID"] != nil {
			t.Errorf("call env %v", c.Env)
		}
	}
	if len(e.spawns) != 1 || !reflect.DeepEqual(e.spawns[0], []string{fakeBin, "--session", session, "server"}) {
		t.Errorf("spawns %v", e.spawns)
	}
	if !e.has("server stop") {
		t.Error("server not stopped")
	}
}

func TestServerEnvKeepsTheClaudeConfigDirAndDropsSessionMarkers(t *testing.T) {
	got := ServerEnv([]string{"CLAUDE_CONFIG_DIR=/c", "CLAUDE_CODE_SESSION_ID=x", "CLAUDECODE=1",
		"CLAUDE_CODE_CHILD_SESSION=1", "CLAUDE_PID=9", "CLAUDE_EFFORT=high", "HERDR_CONFIG_PATH=/u",
		"XDG_CONFIG_HOME=/u", "PATH=/usr/bin"})
	if !reflect.DeepEqual(got, []string{"CLAUDE_CONFIG_DIR=/c", "PATH=/usr/bin"}) {
		t.Error(got)
	}
}

func TestConfigTOMLIsRewrittenBeforeTheServerStarts(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{}, true)
	path := filepath.Join(e.home, "herdr", "config.toml")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, []byte("onboarding = true\n"), 0o644)
	var seen []string
	e.onSpawn = func() {
		b, _ := os.ReadFile(path)
		seen = append(seen, string(b))
	}
	e.mustStart(e.executor(""), "a")
	if len(seen) != 1 || !strings.Contains(seen[0], "onboarding = false") ||
		!strings.Contains(seen[0], "resume_agents_on_restore = false") {
		t.Errorf("%q", seen)
	}
}

func TestLongStatePathFallsBackToAPrivateTmpHome(t *testing.T) {
	tmp := t.TempDir()
	long := filepath.Join(tmp, strings.Repeat("x", 60), "hx")
	root := filepath.Join(tmp, "tmp")
	_ = os.MkdirAll(root, 0o755)
	home, err := ResolveHome(session, long, root, os.Getuid(), true)
	want := filepath.Join(root, "orchestra-"+strconv.Itoa(os.Getuid()), "hx")
	if err != nil || home != want {
		t.Fatalf("%s %v", home, err)
	}
	if st, _ := os.Stat(filepath.Dir(home)); st.Mode().Perm() != 0o700 {
		t.Error(st.Mode())
	}
	if h, _ := ResolveHome(session, "/h/hx", "", -1, true); h != "/h/hx" {
		t.Error(h)
	}
	// unsafe fallback parents are refused
	_ = os.Chmod(filepath.Dir(home), 0o775)
	if _, err := ResolveHome(session, long, root, os.Getuid(), true); err == nil || err.(*host.NeedError).Kind != host.KindError {
		t.Errorf("group-writable parent accepted: %v", err)
	}
	_ = os.Chmod(filepath.Dir(home), 0o700)
	if _, err := ResolveHome(session, long, root, os.Getuid()+1, true); err == nil {
		t.Error("someone else's parent accepted")
	}
}

func TestUnsandboxedServerIsReportedAndRequiredModeRefuses(t *testing.T) {
	t.Setenv(sandbox.ForceEnv, "none")
	e := newEnv(t)
	e.scenario(map[string]any{}, true)
	x := e.executor(`{"sandbox": {"mode": "auto"}}`)
	e.mustStart(x, "a")
	if !reflect.DeepEqual(e.spawns[0], []string{fakeBin, "--session", session, "server"}) || x.sandboxed {
		t.Errorf("%v sandboxed=%v", e.spawns, x.sandboxed)
	}
	if !strings.Contains(x.Stderr.(interface{ String() string }).String(), "agent-exec: herdr not sandboxed: no sandbox backend") {
		t.Errorf("stderr %s", x.Stderr)
	}
	e2 := newEnv(t)
	e2.scenario(map[string]any{}, true)
	_, err := e2.start(e2.executor(`{"sandbox": {"mode": "required"}}`), "a")
	if n := needOf(t, err); n.Kind != host.KindError || len(e2.spawns) != 0 {
		t.Errorf("%v spawns=%v", n, e2.spawns)
	}
}

func TestServerIsSandboxedWhenABackendIsAvailable(t *testing.T) {
	if p := sandbox.Probes(); !p.Seatbelt.OK {
		t.Skip("no seatbelt here")
	}
	e := newEnv(t)
	e.scenario(map[string]any{}, true)
	x := e.executor(`{"sandbox": {"mode": "auto", "network": {"mode": "off"}}}`)
	e.mustStart(x, "a")
	argv := e.spawns[0]
	if argv[0] != "/usr/bin/sandbox-exec" || !reflect.DeepEqual(argv[len(argv)-4:], []string{fakeBin, "--session", session, "server"}) {
		t.Fatalf("argv %v", argv)
	}
	if !strings.Contains(argAfter(argv, "-p"), "(deny file-write*") || !x.sandboxed {
		t.Error("not confined")
	}
	for _, a := range argv {
		if strings.HasPrefix(a, "W") && strings.Contains(a, "=") && strings.SplitN(a, "=", 2)[1] == e.repo {
			t.Errorf("the user's tree is writable: %s", a)
		}
	}
}

func TestShutdownKeepsTheServerWhileSessionsAwaitResume(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{}, true)
	x := e.executor("")
	e.mustStart(x, "a")
	x.Shutdown()
	if e.has("server stop") {
		t.Error("stopped with a session to resume")
	}
	e2 := newEnv(t)
	e2.scenario(map[string]any{}, true)
	x2 := e2.executor(`{"herdr": {"keep_sessions": false}}`)
	e2.mustStart(x2, "a")
	x2.Shutdown()
	if !e2.has("server stop") {
		t.Error("keep_sessions off must stop the server")
	}
}

func TestShutdownLeavesAServerItDidNotStart(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{"running": true}, true)
	x := e.executor("")
	e.mustStart(x, "a")
	x.Shutdown()
	if len(e.spawns) != 0 || e.has("server stop") {
		t.Errorf("spawns %v", e.spawns)
	}
}

func TestSessionReuseAndRegistry(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{"running": true, "prompt_result": map[string]any{"status": "escalate", "summary": "why"}}, true)
	x := e.executor("")
	s := e.mustStart(x, "a")
	reg := x.reg.Load(e.state)
	rec := reg["a"].(map[string]any)
	if rec["terminal"] != "p-a" || rec["herdr_session"] != session || rec["created"] != true || rec["cls"] != "standard" {
		t.Errorf("record %v", rec)
	}
	out, err := host.PromptAndWait(context.Background(), x, s, []string{"/p/one.md"}, e.result, 60e9)
	if err != nil || out["answer"] != "ESCALATE: why" || out["resumed"] != false {
		t.Fatalf("%v %v", out, err)
	}
	// a later run finds the live session and reuses its agent
	y := e.executor("")
	again, ok := y.Session("a", e.state)
	if !ok || again.Terminal != "p-a" || again.Worktree != s.Worktree {
		t.Fatalf("%+v %v", again, ok)
	}
	out, err = host.PromptAndWait(context.Background(), y, again, []string{"/p/two.md"}, e.result, 60e9)
	if err != nil || out["resumed"] != true || out["session_id"] != "p-a" {
		t.Fatalf("%v %v", out, err)
	}
	if e.count("agent start") != 1 {
		t.Errorf("agent started %d times", e.count("agent start"))
	}
	// an agent herdr no longer knows is dropped; a gone worktree drops all
	e.scenario(map[string]any{"running": true, "get_error": "agent_not_found"}, false)
	gone, ok := y.Session("a", e.state)
	if !ok || gone.Terminal != "" || x.reg.Load(e.state)["a"].(map[string]any)["terminal"] != nil {
		t.Errorf("%+v", gone)
	}
	_ = os.RemoveAll(s.Worktree)
	if _, ok := y.Session("a", e.state); ok || x.reg.Load(e.state)["a"] != nil {
		t.Error("a gone worktree must drop the entry")
	}
}

func TestStartFailureRemovesTheWorktree(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{"running": true, "not_ready": true, "screen": "login required"}, true)
	x := e.executor("")
	_, err := e.start(x, "a")
	n := needOf(t, err)
	if n.Kind != host.KindError || !strings.Contains(n.Detail, "login required") {
		t.Errorf("%v", n)
	}
	if _, ok := x.reg.Load(e.state)["a"]; ok {
		t.Error("registry kept the failed package")
	}
	if out := gitRun(t, e.repo, "worktree", "list"); strings.Contains(out, "pkg-a") {
		t.Errorf("worktree left: %s", out)
	}
}

func TestBadBaseRefIsAnErrorNeed(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{}, true)
	_, err := e.executor("").Start(context.Background(), "a", e.repo, "nope", "standard", "opus", "r", e.state, nil)
	if n := needOf(t, err); n.Detail != "could not resolve nope to a commit" {
		t.Error(n)
	}
}

func TestAvailable(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{}, true)
	if ok, why := e.executor("").Available(); !ok {
		t.Error(why)
	}
	if ok, why := e.executor(`{"herdr": {"enabled": false}}`).Available(); ok || why != "disabled by config (herdr.enabled: false)" {
		t.Error(why)
	}
	x := e.executor("")
	x.Binary = "/nonexistent/herdr"
	if ok, why := x.Available(); ok || !strings.HasPrefix(why, "could not run herdr") {
		t.Error(why)
	}
	if e.executor(`{"herdr": {"enabled": "yes"}}`).Enabled() != host.TriTrue {
		t.Error("enabled")
	}
}

func TestNamesAndPayloads(t *testing.T) {
	if AgentName("a") != "p-a" || AgentName("Foo Bar!") != "p-foo-bar" {
		t.Error(AgentName("Foo Bar!"))
	}
	long := AgentName(strings.Repeat("x", 40))
	if len(long) != 32 || !nameRE.MatchString(long) {
		t.Error(long)
	}
	if n := AgentName("日本"); n != "p-" { // as Python: nothing usable left, still a valid name
		t.Error(n)
	}
	if SessionName("", []string{"CLAUDE_CODE_SESSION_ID=ABCDEF12-x"}) != session {
		t.Error("session id")
	}
	if SessionName("Run-1/x", []string{}) != "orchestra-run1x" || SessionName("", []string{}) != "" {
		t.Error("run id fallback")
	}
	for out, want := range map[string]bool{
		`{"result": {"server": {"running": true}}}`:  true,
		`{"result": {"server": {"running": false}}}`: false,
		`{"status": "server running"}`:               true,
		"server is not running":                      false,
		"running":                                    true,
		"":                                           false,
	} {
		if ServerRunning(out) != want {
			t.Errorf("%q", out)
		}
	}
}
