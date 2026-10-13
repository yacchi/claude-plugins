package capture

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/sandbox"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/watchdog"
)

func TestSpawnLargeStdinStdoutStderrDoesNotDeadlock(t *testing.T) {
	payload := strings.Repeat(strings.Repeat("x", 99)+"\n", 10000) // ~1 MB
	res, err := spawn(context.Background(), spawnOptions{
		Argv: []string{"sh", "-c", "tee /dev/stderr"}, Input: payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || res.Stdout != payload || res.Stderr != payload {
		t.Fatalf("exit %d, stdout %d bytes, stderr %d bytes", res.ExitCode, len(res.Stdout), len(res.Stderr))
	}
}

func TestSpawnSplitsRecordsOnNewlineOnly(t *testing.T) {
	var lines []string
	res, err := spawn(context.Background(), spawnOptions{
		Argv:   []string{"printf", `a\342\200\250b\r\nc\n\377tail`},
		OnLine: func(l string) { lines = append(lines, l) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(lines, []string{"a b", "c", "�tail"}) {
		t.Fatalf("lines %q", lines)
	}
	if res.Stdout != "a b\r\nc\n�tail" {
		t.Fatalf("stdout %q", res.Stdout)
	}
}

func TestSpawnVeryLongLine(t *testing.T) {
	var longest int
	res, err := spawn(context.Background(), spawnOptions{
		Argv:   []string{"sh", "-c", "head -c 3000000 /dev/zero | tr '\\0' y; echo"},
		OnLine: func(l string) { longest = max(longest, len(l)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if longest != 3000000 || len(res.Stdout) != 3000001 {
		t.Fatalf("longest record %d, stdout %d", longest, len(res.Stdout))
	}
}

func TestSpawnSignalExitIsNegative(t *testing.T) {
	res, err := spawn(context.Background(), spawnOptions{Argv: []string{"sh", "-c", "kill -TERM $$"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != -int(syscall.SIGTERM) {
		t.Fatalf("exit %d", res.ExitCode)
	}
}

func TestSpawnWorkdirAndEnv(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	res, err := spawn(context.Background(), spawnOptions{
		Argv: []string{"sh", "-c", "pwd; echo $P5_X"}, Cwd: dir, Env: []string{"P5_X=1", "PATH=" + os.Getenv("PATH")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != dir+"\n1\n" {
		t.Fatalf("stdout %q", res.Stdout)
	}
}

func TestSpawnContextCancelKillsGroup(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "pid")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := spawn(ctx, spawnOptions{Argv: []string{"sh", "-c", "sleep 30 & echo $! > " + marker + "; wait"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %v", d)
	}
	assertDead(t, marker)
}

func assertDead(t *testing.T, marker string) {
	t.Helper()
	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	for i := 0; i < 100; i++ {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("grandchild survived")
}

func guard(idle, toolIdle, wall time.Duration) *watchdog.Watchdog {
	return watchdog.New(watchdog.Config{Enabled: true, Idle: idle, ToolIdle: toolIdle, RepeatLimit: 3,
		Wall: map[string]time.Duration{"standard": wall}}, "standard")
}

func TestWatchdogWallKillsGrandchildren(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "pid")
	res, err := spawn(context.Background(), spawnOptions{
		Argv:     []string{"sh", "-c", "sleep 30 & echo $! > " + marker + "; wait"},
		Watchdog: guard(10*time.Second, 10*time.Second, 500*time.Millisecond),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Runaway == nil || res.Runaway.Reason != "wall" || res.ExitCode == 0 {
		t.Fatalf("runaway %+v exit %d", res.Runaway, res.ExitCode)
	}
	assertDead(t, marker)
}

func TestWatchdogToolIdleUsesLongerBudget(t *testing.T) {
	res, err := spawn(context.Background(), spawnOptions{
		Argv:     []string{"sh", "-c", `echo '{"type": "tool_execution_start", "toolCallId": "c", "toolName": "bash", "args": {}}'; sleep 0.15`},
		Watchdog: guard(50*time.Millisecond, 800*time.Millisecond, 2*time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Runaway != nil || res.ExitCode != 0 {
		t.Fatalf("runaway %+v exit %d", res.Runaway, res.ExitCode)
	}
}

func TestWatchdogToolIdleFiresWithinHalfASecond(t *testing.T) {
	res, err := spawn(context.Background(), spawnOptions{
		Argv:     []string{"sh", "-c", `echo '{"type": "tool_execution_start", "toolCallId": "c", "toolName": "bash", "args": {}}'; sleep 10`},
		Watchdog: guard(200*time.Millisecond, 600*time.Millisecond, 30*time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	r := res.Runaway
	if r == nil || r.Reason != "tool-idle" || r.ElapsedS < 0.6 || r.ElapsedS >= 1.1 {
		t.Fatalf("runaway %+v", r)
	}
}

func TestRequiredModeWithoutBackendNeverSpawns(t *testing.T) {
	isolatedHome(t)
	t.Setenv(sandbox.ForceEnv, "none")
	cfg := defaultsCfg()
	cfg.Values.Object("sandbox").Set("mode", "required")
	tree := t.TempDir()
	spec := sandbox.Prepare("pi", tree, cfg, "m", "")
	if !spec.Refuse {
		t.Fatalf("spec not refused: %+v", spec)
	}
	restore := swapSeams(func(context.Context, Request, sandbox.Spec, string, string) (*jsonx.Object, *denialInput, error) {
		t.Fatal("spawned")
		return nil, nil, nil
	}, prepareSpec)
	defer restore()
	got, _, err := RunWithGrants(context.Background(), Request{Executor: "pi", Workdir: tree,
		SandboxTree: tree, Sandbox: spec, Cfg: cfg})
	if err != nil {
		t.Fatal(err)
	}
	limits, _ := jsonx.Marshal(spec.Limits)
	want := `{"status": "unavailable", "answer": null, "session_id": null, "reason": "sandbox", "exit_code": null, "usage": null, "sandbox": {"backend": "none", "enforced": false, "reason": ` +
		strconv.Quote(spec.Reason) + `, "limits": ` + string(limits) + `, "writable": []}}`
	if raw, _ := jsonx.Marshal(got); string(raw) != want {
		t.Fatalf("got  %s\nwant %s", raw, want)
	}
}

func TestMissingBinaryIsNotFound(t *testing.T) {
	isolatedHome(t)
	t.Setenv("PATH", t.TempDir())
	cfg := defaultsCfg()
	cfg.Values.Object("sandbox").Set("mode", "off")
	_, _, err := RunWithGrants(context.Background(), Request{Executor: "pi", Prompt: "x",
		Sandbox: sandbox.Prepare("pi", t.TempDir(), cfg, "m", ""), Cfg: cfg})
	if !errors.Is(err, ErrExecutorNotFound) {
		t.Fatalf("err %v", err)
	}
}

func TestUnregisteredExecutorDoesNotFallBack(t *testing.T) {
	_, _, err := RunWithGrants(context.Background(), Request{Executor: "nonesuch", Cfg: defaultsCfg()})
	if !errors.Is(err, ErrUnknownExecutor) {
		t.Fatalf("err %v", err)
	}
}

// The child runs inside the workdir, with PI_SKIP_VERSION_CHECK and the
// managed agent dir in its env and the prompt on stdin.
func TestPiChildEnvCwdAndStdin(t *testing.T) {
	home := isolatedHome(t)
	bin := t.TempDir()
	script := "#!/bin/sh\nprompt=$(cat)\n" +
		`printf '{"type":"session","id":"s"}\n{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"%s|%s|%s|%s"}],"stopReason":"stop"}}\n' ` +
		`"$(pwd)" "$PI_SKIP_VERSION_CHECK" "$PI_CODING_AGENT_DIR" "$prompt"` + "\n"
	if err := os.WriteFile(filepath.Join(bin, "pi"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := defaultsCfg()
	cfg.Values.Object("sandbox").Set("mode", "off")
	work, _ := filepath.EvalSymlinks(t.TempDir())
	for _, dir := range []string{work, filepath.Join(work, "missing")} {
		got, _, err := RunWithGrants(context.Background(), Request{Executor: "pi", Workdir: dir,
			Prompt: "the prompt", Sandbox: sandbox.Prepare("pi", work, cfg, "m", ""), Cfg: cfg})
		if err != nil {
			t.Fatal(err)
		}
		answer, _ := got.Get("answer")
		parts := strings.Split(answer.(string), "|")
		cwd, _ := os.Getwd()
		wantCwd := work
		if dir != work {
			wantCwd, _ = filepath.EvalSymlinks(cwd)
		}
		agent := filepath.Join(home, ".local", "state", "claude-orchestra", "pi-agent")
		if parts[0] != wantCwd || parts[1] != "1" || parts[2] != agent || parts[3] != "the prompt" {
			t.Fatalf("child saw %q", parts)
		}
		if st, err := os.Stat(agent); err != nil || st.Mode().Perm() != 0o700 {
			t.Fatalf("agent dir %v %v", st, err)
		}
	}
}

// Prepare's policy sees the executor env registered by this package: the
// managed pi agent dir is writable.
func TestExecutorEnvIsRegistered(t *testing.T) {
	home := isolatedHome(t)
	cfg := defaultsCfg()
	spec := sandbox.Prepare("pi", t.TempDir(), cfg, "m", "")
	if spec.Policy == nil {
		t.Skip("no sandbox backend on this host")
	}
	agent := filepath.Join(home, ".local", "state", "claude-orchestra", "pi-agent")
	if !slices.Contains(spec.Policy.Writable, agent) {
		t.Fatalf("writable %v lacks %s", spec.Policy.Writable, agent)
	}
	if ExecutorEnv("other", cfg) == nil || len(ExecutorEnv("other", cfg)) != 0 {
		t.Fatal("non-pi executor got env")
	}
}

func TestMergeUsage(t *testing.T) {
	a := jsonx.NewObject().Set("tokens", jsonx.NewObject().Set("input_tokens", int64(1))).
		Set("cost_micro_usd", int64(2))
	b := jsonx.NewObject().Set("tokens", jsonx.NewObject().Set("input_tokens", int64(3)).
		Set("output_tokens", int64(4))).Set("cost_micro_usd", int64(5))
	got, _ := jsonx.Marshal(mergeUsage(a, b))
	if string(got) != `{"tokens": {"input_tokens": 4, "output_tokens": 4}, "cost_micro_usd": 7}` {
		t.Fatalf("merged %s", got)
	}
	if mergeUsage(nil, b).(*jsonx.Object) == b {
		t.Fatal("not copied")
	}
	if mergeUsage(a, nil) != any(a) {
		t.Fatal("a not kept")
	}
	if mergeUsage(nil, nil) != nil {
		t.Fatal("nil")
	}
}

func TestStderrTail(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 30; i++ {
		b.WriteString("line " + strconv.Itoa(i) + "  \n\n[SandboxDebug] x\n")
	}
	got := stderrTail(b.String())
	lines := strings.Split(got, "\n")
	if len(lines) != 20 || lines[0] != "line 10" || lines[19] != "line 29" {
		t.Fatalf("tail %q", got)
	}
	if got := stderrTail(strings.Repeat("é", 3000)); len([]rune(got)) != 2000 {
		t.Fatalf("chars %d", len([]rune(got)))
	}
	if stderrTail(" \n[SandboxDebug] only\n") != "" {
		t.Fatal("blank tail")
	}
}

func writeFiles(t *testing.T, contents ...string) []string {
	t.Helper()
	dir := t.TempDir()
	var out []string
	for i, c := range contents {
		p := filepath.Join(dir, strconv.Itoa(i))
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func TestReadPromptFiles(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want string
	}{
		{[]string{"one\n\n"}, "one\n\n"},
		{[]string{"one", "two"}, "one\n\ntwo"},
		{[]string{"one", "two\n\n\n", "three"}, "one\n\ntwo\n\nthree"},
		{[]string{`日本語 "quotes" and \\slashes`}, `日本語 "quotes" and \\slashes`},
		{[]string{"one", "", "three"}, "one\n\nthree"},
		{[]string{"", ""}, ""},
		{[]string{"a\r\nb\rc"}, "a\nb\nc"},
	} {
		got, err := ReadPromptFiles(writeFiles(t, c.in...))
		if err != nil || got != c.want {
			t.Errorf("%q: got %q, %v", c.in, got, err)
		}
	}
	missing := filepath.Join(t.TempDir(), "agent-exec-missing-prompt")
	if _, err := ReadPromptFiles([]string{missing}); err == nil || !strings.Contains(err.Error(), missing) {
		t.Errorf("missing: %v", err)
	}
	dir := t.TempDir()
	if _, err := ReadPromptFiles([]string{dir}); err == nil || !strings.Contains(err.Error(), dir) {
		t.Errorf("directory: %v", err)
	}
	bad := writeFiles(t, "\xff\xfe")
	if _, err := ReadPromptFiles(bad); err == nil {
		t.Error("invalid UTF-8 accepted")
	}
}
