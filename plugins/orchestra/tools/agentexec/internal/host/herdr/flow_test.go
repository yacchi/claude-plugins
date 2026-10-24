package herdr

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/sandbox"
)

// --- warm reader --------------------------------------------------------------

func (e *env) warmExecutor(cfg string, counts map[string]int) *Executor {
	x := e.executor(cfg)
	if counts == nil {
		counts = map[string]int{"opus": 2}
	}
	x.ConfigureWarm([]string{"README.md"}, counts)
	return x
}

func (e *env) warmStart(x *Executor, pkg, model string) *host.Session {
	e.t.Helper()
	s, err := x.Start(context.Background(), pkg, e.repo, "main", "standard", model, "run-1", e.state, []string{e.state})
	if err != nil {
		e.t.Fatalf("start %s: %v", pkg, err)
	}
	return s
}

func (e *env) starts() [][]string {
	var out [][]string
	for _, c := range e.callsOf("agent start") {
		out = append(out, c.Argv)
	}
	return out
}

func (e *env) forks() [][]string {
	var out [][]string
	for _, s := range e.starts() {
		if hasArg(s, "--fork-session") {
			out = append(out, s)
		}
	}
	return out
}

func (e *env) warmStarts() [][]string {
	var out [][]string
	for _, s := range e.starts() {
		if hasArg(s, "--session-id") {
			out = append(out, s)
		}
	}
	return out
}

func TestWarmReaderHasItsOwnWorktreeAndPackagesForkFromIt(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{"running": true}, true)
	x := e.warmExecutor("", nil)
	first := e.warmStart(x, "a", "opus")
	second := e.warmStart(x, "b", "opus")
	var cwds []string
	for _, c := range e.callsOf("workspace create") {
		cwds = append(cwds, argAfter(c.Argv, "--cwd"))
	}
	if len(cwds) != 3 || !strings.Contains(cwds[0], "warm-opus") || cwds[0] == first.Worktree || cwds[0] == second.Worktree {
		t.Fatalf("cwds %v", cwds)
	}
	warm := e.starts()[0]
	sid := argAfter(warm, "--session-id")
	if sid == "" || hasArg(warm, "--fork-session") || len(e.forks()) != 2 {
		t.Fatalf("starts %v", e.starts())
	}
	for _, f := range e.forks() {
		if argAfter(f, "--resume") != sid {
			t.Error(f)
		}
	}
	if first.Str("warm_session") != sid || e.count("agent prompt") != 1 {
		t.Errorf("warm_session %q prompts %d", first.Str("warm_session"), e.count("agent prompt"))
	}
	rec := x.warmRecords(e.state)["opus"].(map[string]any)
	if rec["session_id"] != sid || !reflect.DeepEqual(rec["files"], []any{"README.md"}) {
		t.Errorf("record %v", rec)
	}
}

func TestNoForkWhenWarmDoesNotApply(t *testing.T) {
	for _, c := range []struct {
		label, cfg string
		counts     map[string]int
		model      string
	}{
		{"disabled", `{"herdr": {"warm": {"enabled": false}}}`, nil, "opus"},
		{"below min_packages", "", map[string]int{"opus": 1}, "opus"},
		{"other model", "", map[string]int{"opus": 2}, "sonnet"},
	} {
		t.Run(c.label, func(t *testing.T) {
			e := newEnv(t)
			e.scenario(map[string]any{"running": true}, true)
			x := e.warmExecutor(c.cfg, c.counts)
			e.warmStart(x, "a", c.model)
			if len(e.forks()) != 0 || e.count("agent prompt") != 0 || hasArg(e.starts()[0], "--session-id") {
				t.Errorf("starts %v", e.starts())
			}
		})
	}
}

func TestExpiredTTLAndChangedWarmFileRebuildButOtherChangesDoNot(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{"running": true}, true)
	x := e.warmExecutor("", nil)
	e.warmStart(x, "a", "opus")
	_ = os.WriteFile(filepath.Join(e.repo, "other.txt"), []byte("x\n"), 0o644)
	gitRun(t, e.repo, "add", "-A")
	gitRun(t, e.repo, "commit", "-q", "-m", "other")
	e.warmStart(x, "b", "opus")
	if e.count("agent prompt") != 1 || len(e.forks()) != 2 {
		t.Fatalf("prompts %d forks %d", e.count("agent prompt"), len(e.forks()))
	}
	_ = os.WriteFile(filepath.Join(e.repo, "README.md"), []byte("changed\n"), 0o644)
	gitRun(t, e.repo, "add", "-A")
	gitRun(t, e.repo, "commit", "-q", "-m", "readme")
	e.warmStart(x, "c", "opus")
	warm := e.warmStarts()
	if e.count("agent prompt") != 2 || len(warm) != 2 || argAfter(warm[0], "--session-id") == argAfter(warm[1], "--session-id") {
		t.Fatalf("prompts %d warm %v", e.count("agent prompt"), warm)
	}
	e.mu.Lock()
	e.now = e.now.Add(100000 * time.Second)
	e.mu.Unlock()
	e.warmStart(x, "d", "opus")
	if e.count("agent prompt") != 3 {
		t.Errorf("ttl: prompts %d", e.count("agent prompt"))
	}
}

func TestWarmFailureFallsBackToAFreshStart(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{"running": true, "fail_warm": true}, true)
	x := e.warmExecutor("", nil)
	first := e.warmStart(x, "a", "opus")
	second := e.warmStart(x, "b", "opus")
	if len(e.forks()) != 0 || first.Str("warm_session") != "" || second.Pkg != "b" {
		t.Errorf("forks %v", e.forks())
	}
	n := 0
	for _, ev := range e.events {
		if ev["action"] == "warm-failed" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("warm-failed reported %d times (want once, not per package)", n)
	}
}

func TestForkThatDirtiesTheWarmTreeIsANeedAndForcesARebuild(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{"running": true}, true)
	x := e.warmExecutor("", nil)
	s := e.warmStart(x, "a", "opus")
	e.scenario(map[string]any{"running": true,
		"prompt_write":  map[string]any{filepath.Join(s.Str("warm_worktree"), "oops.txt"): "x\n"},
		"prompt_result": map[string]any{"status": "ok", "summary": "done"}}, false)
	_, err := host.PromptAndWait(context.Background(), x, s, []string{"/p/one.md"}, e.result, 60e9)
	if n := needOf(t, err); n.Kind != host.KindError || !strings.Contains(n.Detail, "oops.txt") {
		t.Fatal(n)
	}
	e.scenario(map[string]any{"running": true}, false)
	e.warmStart(x, "b", "opus")
	if len(e.warmStarts()) != 2 {
		t.Errorf("warm not rebuilt: %v", e.warmStarts())
	}
}

func TestForkedPromptNamesBothTreesOnce(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{"running": true, "prompt_result": map[string]any{"status": "ok", "summary": "done"}}, true)
	x := e.warmExecutor("", nil)
	s := e.warmStart(x, "a", "opus")
	for _, f := range []string{"/p/one.md", "/p/two.md"} {
		if _, err := host.PromptAndWait(context.Background(), x, s, []string{f}, e.result, 60e9); err != nil {
			t.Fatal(err)
		}
	}
	prompts := e.callsOf("agent prompt")
	first, second := prompts[1].Argv[5], prompts[2].Argv[5]
	if !strings.Contains(first, "Your working tree is "+s.Worktree+".") || !strings.Contains(first, s.Str("warm_worktree")) ||
		strings.Contains(second, "Your working tree") {
		t.Errorf("first %q\nsecond %q", first, second)
	}
}

func TestWarmRecordIsNotAPackageAndDoesNotKeepTheServer(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{}, true)
	x := e.warmExecutor("", nil)
	e.warmStart(x, "a", "opus")
	if _, ok := x.Session(WarmKey, e.state); ok {
		t.Error("_warm is not a package")
	}
	s, _ := x.Session("a", e.state)
	_ = x.Close(s, true)
	x.Shutdown()
	if !e.has("server stop") {
		t.Error("warm records kept the server")
	}
	if _, ok := x.reg.Load(e.state)[WarmKey]; ok {
		t.Error("warm records survived shutdown")
	}
	if out := gitRun(t, e.repo, "worktree", "list"); strings.Contains(out, "warm-opus") {
		t.Errorf("warm worktree left: %s", out)
	}
}

func TestBypassIsRefusedOnTheForkPath(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{"running": true}, true)
	x := e.executor(`{"herdr": {"permission_mode": "bypassPermissions"}}`)
	_, err := x.nativeArgs(&host.Session{Extra: map[string]any{"model": "opus", "warm_session": "sid"}}, true)
	if n := needOf(t, err); !strings.Contains(n.Detail, "bypass") {
		t.Error(n)
	}
	x = e.warmExecutor(`{"herdr": {"args": "--dangerously-skip-permissions"}}`, nil)
	if _, err := e.start(x, "a"); err == nil {
		t.Fatal("bypass accepted")
	}
	if len(e.starts()) != 0 {
		t.Errorf("an agent started: %v", e.starts())
	}
}

// --- prompts, trust, waits ---------------------------------------------------------

func TestAStalledPromptIsResentOnceAndASecondStallIsANeed(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{"running": true, "stalls": 1, "prompt_result": map[string]any{"status": "ok"}}, true)
	x := e.executor("")
	out, err := host.PromptAndWait(context.Background(), x, e.manualSession(), []string{"/p/one.md"}, e.result, 60e9)
	prompts := e.callsOf("agent prompt")
	if err != nil || out["status"] != "ok" || len(prompts) != 2 || !reflect.DeepEqual(prompts[0].Argv, prompts[1].Argv) {
		t.Fatalf("%v %v %d", out, err, len(prompts))
	}
	e.scenario(map[string]any{"running": true, "stalls": 5}, true)
	err = x.Prompt(context.Background(), e.manualSession(), []string{"/p/one.md"}, e.result, 60e9)
	if n := needOf(t, err); n.Kind != host.KindStalled || e.count("agent prompt") != 2 {
		t.Errorf("%v prompts %d", n, e.count("agent prompt"))
	}
}

func (e *env) trustStart(cfg string, precreate, sandboxed bool) (*Executor, error) {
	if precreate {
		sha := strings.TrimSpace(gitRun(e.t, e.repo, "rev-parse", "HEAD"))
		gitRun(e.t, e.repo, "worktree", "add", "-q", "-b", "orchestra/abcdef12/pkg-a",
			filepath.Join(e.repo, ".claude", "worktrees", "pkg-a"), sha)
	}
	x := e.executor(cfg)
	x.sandboxed = sandboxed
	if sandboxed {
		// stand-in for a server started under Seatbelt/bwrap
		x.serverReady, x.serverStarted = true, true
	}
	_, err := e.start(x, "a")
	return x, err
}

func TestTrustDialogIsReportedUnlessAutoTrustAndTheExecutorCreatedTheTree(t *testing.T) {
	for _, c := range []struct {
		label, cfg           string
		precreate, sandboxed bool
	}{
		{"auto_trust off", `{"herdr": {"auto_trust": false}}`, false, true},
		{"tree not created here", `{"herdr": {"auto_trust": true}}`, true, true},
		{"server not sandboxed", `{"herdr": {"auto_trust": true}}`, false, false},
	} {
		t.Run(c.label, func(t *testing.T) {
			e := newEnv(t)
			e.scenario(map[string]any{"not_ready": true, "screen": trustScreen}, true)
			_, err := e.trustStart(c.cfg, c.precreate, c.sandboxed)
			if n := needOf(t, err); n.Kind != host.KindTrust || e.has("agent send-keys") {
				t.Errorf("%v", n)
			}
		})
	}
}

func TestAutoTrustMovesDownBeforeEnterThenWaitsForTheInputBox(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{"not_ready": true, "screen": trustScreen, "after_keys": []any{trustScreen, "", "❯ "}}, true)
	if _, err := e.trustStart("", false, true); err != nil {
		t.Fatal(err)
	}
	keys := e.callsOf("agent send-keys")
	if len(keys) != 1 || !reflect.DeepEqual(keys[0].Argv[2:], []string{"agent", "send-keys", "p-a", "down", "enter"}) {
		t.Fatalf("%v", keys)
	}
	if e.verbIndex("agent send-keys") > e.verbIndex("agent wait") {
		t.Error("wait before keys")
	}
	reads := 0
	for _, c := range e.calls()[e.verbIndex("agent send-keys"):] {
		if verb(c) == "agent read" {
			reads++
		}
	}
	if reads != 3 {
		t.Errorf("reads after keys = %d", reads)
	}
}

func TestInputBoxThatNeverAppearsIsStalled(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{"not_ready": true, "screen": trustScreen, "after_keys": []any{trustScreen}}, true)
	_, err := e.trustStart(`{"herdr": {"startup_timeout": 5}}`, false, true)
	if n := needOf(t, err); n.Kind != host.KindStalled {
		t.Error(n)
	}
}

func TestIdleAndDoneAreNotCompletion(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{"running": true, "statuses": []any{"idle", "done", "idle"}}, true)
	_, err := e.executor("").WaitResult(context.Background(), e.manualSession(), e.result, 10e9)
	if n := needOf(t, err); n.Kind != host.KindTimeout || e.count("agent get") <= 2 {
		t.Errorf("%v gets %d", n, e.count("agent get"))
	}
}

func TestAnIdleAgentWithoutALoginFailsAtOnce(t *testing.T) {
	e := newEnv(t)
	screen := "❯ implement A0\n  ⎿  Not logged in · Please run /login\n✻ Worked for 0s"
	e.scenario(map[string]any{"running": true, "statuses": []any{"working", "idle"}, "screen": screen}, true)
	_, err := e.executor("").WaitResult(context.Background(), e.manualSession(), e.result, 3600e9)
	n := needOf(t, err)
	if err := containsAll(n.Detail, "no usable login", "Not logged in"); n.Kind != host.KindError || err != nil ||
		e.count("agent get") > 2 {
		t.Errorf("%v %v gets %d", n, err, e.count("agent get"))
	}
}

func TestBlockedIsAStallWithTheDialog(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{"running": true, "statuses": []any{"working", "blocked"}, "screen": permissionScreen}, true)
	_, err := e.executor("").WaitResult(context.Background(), e.manualSession(), e.result, 60e9)
	n := needOf(t, err)
	if err := containsAll(n.Detail, "dialog:", "Do you want to proceed?", "terminal: p-a"); n.Kind != host.KindStalled || err != nil {
		t.Error(n, err)
	}
}

func TestVanishedAgentIsAStall(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{"running": true, "get_error": "agent_not_found"}, true)
	_, err := e.executor("").WaitResult(context.Background(), e.manualSession(), e.result, 60e9)
	if n := needOf(t, err); n.Kind != host.KindStalled || !strings.Contains(n.Detail, "went away") {
		t.Error(n)
	}
}

func TestNoPromptIsSentToABlockedAgent(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{"running": true, "statuses": []any{"blocked"}, "screen": permissionScreen}, true)
	err := e.executor("").Prompt(context.Background(), e.manualSession(), []string{"/p/one.md"}, e.result, 60e9)
	if n := needOf(t, err); n.Kind != host.KindStalled || e.has("agent prompt") {
		t.Error(n)
	}
}

// --- native args ------------------------------------------------------------------

func TestBypassIsRefused(t *testing.T) {
	e := newEnv(t)
	for _, cfg := range []string{`{"herdr": {"permission_mode": "bypassPermissions"}}`,
		`{"herdr": {"args": "--model {model} --dangerously-skip-permissions"}}`,
		`{"herdr": {"args": "--permission-mode bypassPermissions"}}`} {
		_, err := e.executor(cfg).nativeArgs(&host.Session{Extra: map[string]any{"model": "opus"}}, false)
		if n := needOf(t, err); !strings.Contains(n.Detail, "bypass") {
			t.Error(cfg, n)
		}
	}
	_, err := e.executor(`{"herdr": {"permission_mode": "yolo"}}`).nativeArgs(&host.Session{}, false)
	if n := needOf(t, err); n.Detail != "invalid herdr.permission_mode: yolo" {
		t.Error(n)
	}
}

func TestNativeArgsAndIsolationFlags(t *testing.T) {
	e := newEnv(t)
	s := &host.Session{Extra: map[string]any{"model": "opus", "add_dirs": []any{"/x/state"}}}
	args, _ := e.executor("").nativeArgs(s, false)
	want := []string{"--model", "opus", "--permission-mode", "auto", "--add-dir", "/x/state",
		"--strict-mcp-config", "--setting-sources", "project,local", "--plugin-dir", e.pluginRoot(),
		"--settings", `{"autoMemoryEnabled":false,"disableClaudeAiConnectors":true,"disableRemoteControl":true}`, "--no-chrome"}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("%v", args)
	}
	if _, err := os.Stat(filepath.Join(e.pluginRoot(), ".claude-plugin", "plugin.json")); err != nil {
		t.Error(err)
	}
	args, _ = e.executor(`{"herdr": {"user_config": "inherit"}}`).nativeArgs(s, false)
	if !reflect.DeepEqual(args, append(want[:6:6], "--settings", `{"disableRemoteControl":true}`)) {
		t.Errorf("inherit %v", args)
	}
	args, _ = e.executor(`{"herdr": {"args": "--model {model} --strict-mcp-config --setting-sources=user --plugin-dir /mine"}}`).
		nativeArgs(&host.Session{}, false)
	if strings.Count(strings.Join(args, " "), "--strict-mcp-config") != 1 || hasArg(args, "--setting-sources") ||
		strings.Count(strings.Join(args, " "), "--plugin-dir") != 1 {
		t.Errorf("dup %v", args)
	}
}

// The worker's --settings reaches the pane intact: the start line, split the
// way the pane's shell splits it, gives back the same argv, and the one
// --settings value is the JSON that turns Remote Control off.
func TestStartLineRoundTripsTheSettings(t *testing.T) {
	e := newEnv(t)
	for _, cfg := range []string{"", `{"herdr": {"user_config": "inherit"}}`} {
		native, err := e.executor(cfg).nativeArgs(&host.Session{}, false)
		if err != nil {
			t.Fatal(err)
		}
		line := startLine(native)
		split, err := host.ShellSplit(line)
		if err != nil || !reflect.DeepEqual(split, append([]string{"claude"}, native...)) {
			t.Fatalf("%q: %v %v", line, split, err)
		}
		var settings []string
		for i, a := range native {
			if a == "--settings" {
				settings = append(settings, native[i+1])
			}
		}
		var m map[string]any
		if len(settings) != 1 || json.Unmarshal([]byte(settings[0]), &m) != nil || m["disableRemoteControl"] != true {
			t.Errorf("%q: settings %v", cfg, settings)
		}
	}
}

func TestWarmReaderForkAndReopenCarryTheSameFlags(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{"running": true}, true)
	x := e.warmExecutor("", nil)
	e.warmStart(x, "a", "opus")
	e.warmStart(x, "b", "opus")
	flags := []string{"--strict-mcp-config", "--setting-sources", "project,local", "--plugin-dir", e.pluginRoot(),
		"--settings", `{"autoMemoryEnabled":false,"disableClaudeAiConnectors":true,"disableRemoteControl":true}`, "--no-chrome"}
	starts := e.starts()
	if len(starts) != 3 {
		t.Fatal(starts)
	}
	for _, s := range starts {
		if !reflect.DeepEqual(s[len(s)-len(flags):], flags) {
			t.Error(s)
		}
	}
}

func TestHostReportWithoutSrt(t *testing.T) {
	e := newEnv(t)
	t.Setenv(sandbox.ForceEnv, "none")
	_ = e
	r := HostReport(cfgOf(t, `{"sandbox": {"mode": "auto"}}`))
	b, _ := jsonMarshal(r)
	if err := containsAll(string(b), `"sandboxed":false`, `"restricted":false`, `network not restricted: host not sandboxed`,
		`"srt":{"path":null,"reason":"srt forced unavailable by AGENT_EXEC_SANDBOX_BACKEND=none","version":null}`); err != nil {
		t.Errorf("%v\n%s", err, b)
	}
}
