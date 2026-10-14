package herdr

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/srt"
)

const session = "orchestra-abcdef12"

var fakeBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fakeherdr-bin-")
	if err != nil {
		panic(err)
	}
	fakeBin = filepath.Join(dir, "herdr")
	build := exec.Command("go", "build", "-o", fakeBin, "../../testutil/fakeherdr")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

const (
	trustScreen      = "Accessing workspace\nIs this a project you created or one you trust?\n> 1. No, exit\n  2. Yes, I trust this folder"
	permissionScreen = "Bash command\nDo you want to proceed?\n> 1. Yes\n  2. No"
)

// env is one test's world: a git repo, a state dir, a herdr home, the
// fake's scenario dir, and a fake clock that advances on every sleep.
type env struct {
	t                               *testing.T
	tmp, repo, state, home, fakeDir string
	result                          string
	mu                              sync.Mutex
	now                             time.Time
	spawns                          [][]string
	spawnEnvs                       [][]string
	onSpawn                         func()
	events                          []map[string]any
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

func newEnv(t *testing.T) *env {
	tmp, _ := filepath.EvalSymlinks(t.TempDir())
	e := &env{t: t, tmp: tmp, repo: filepath.Join(tmp, "repo"), state: filepath.Join(tmp, "state"),
		home: filepath.Join(tmp, "hx"), fakeDir: filepath.Join(tmp, "fake"), now: time.Unix(1000, 0)}
	for _, d := range []string{e.repo, e.state, e.fakeDir} {
		_ = os.MkdirAll(d, 0o755)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "abcdef12-0000")
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "xstate"))
	t.Setenv("HOME", filepath.Join(tmp, "home"))
	t.Setenv("ORCHESTRA_ALIVE_DIR", filepath.Join(tmp, "alive"))
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("FAKEHERDR_DIR", e.fakeDir)
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("AGENT_EXEC_BOOTSTRAP", pluginBootstrap(t))
	gitRun(t, e.repo, "init", "-q", "-b", "main", ".")
	gitRun(t, e.repo, "config", "user.email", "t@example.com")
	gitRun(t, e.repo, "config", "user.name", "T")
	_ = os.WriteFile(filepath.Join(e.repo, "README.md"), []byte("hello\n"), 0o644)
	gitRun(t, e.repo, "add", "-A")
	gitRun(t, e.repo, "commit", "-q", "-m", "init")
	e.result = filepath.Join(e.state, "a.result.json")
	t.Cleanup(func() {
		c := exec.Command("git", "worktree", "prune")
		c.Dir = e.repo
		_ = c.Run()
	})
	return e
}

// pluginBootstrap is tools/agent-exec of this checkout (the plugin root is
// its grandparent).
func pluginBootstrap(t *testing.T) string {
	wd, _ := os.Getwd()
	p := filepath.Join(wd, "..", "..", "..", "..", "agent-exec")
	abs, _ := filepath.Abs(p)
	return abs
}

func (e *env) pluginRoot() string { return filepath.Dir(filepath.Dir(pluginBootstrap(e.t))) }

// scenario (re)writes the fake's scenario; state starts fresh with
// reset.
func (e *env) scenario(sc map[string]any, reset bool) {
	b, _ := json.Marshal(sc)
	_ = os.WriteFile(filepath.Join(e.fakeDir, "scenario.json"), b, 0o644)
	if reset {
		_ = os.Remove(filepath.Join(e.fakeDir, "state.json"))
		_ = os.Remove(filepath.Join(e.fakeDir, "calls.jsonl"))
	}
}

func cfgOf(t *testing.T, js string) *config.Resolved {
	t.Helper()
	if js == "" {
		js = "{}"
	}
	v, err := fsutil.DecodeJSON([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	over := v.(map[string]any)
	if config.ObjectAt(over, "sandbox") == nil {
		over["sandbox"] = map[string]any{"mode": "off"}
	}
	return &config.Resolved{Values: config.DeepMerge(config.Defaults(), over)}
}

// executor is a herdr host on the fake; cfg is a JSON overlay of the
// config (sandbox.mode defaults to off).
func (e *env) executor(cfg string) *Executor {
	x := New(cfgOf(e.t, cfg))
	x.Binary = fakeBin
	x.Home = e.home
	x.Stderr = &bytes.Buffer{}
	x.SrtStatus = func() srt.StatusInfo { return srt.StatusInfo{Reason: "srt not available"} }
	x.Poll = time.Second
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
	x.OnSpawn = func(argv, envv []string) {
		e.spawns = append(e.spawns, argv)
		e.spawnEnvs = append(e.spawnEnvs, envv)
		if e.onSpawn != nil {
			e.onSpawn()
		}
	}
	x.SetEventFunc(func(pkg string, d map[string]any) {
		d["pkg"] = pkg
		e.mu.Lock()
		e.events = append(e.events, d)
		e.mu.Unlock()
	})
	return x
}

type loggedCall struct {
	Argv []string       `json:"argv"`
	Env  map[string]any `json:"env"`
}

func (e *env) calls() []loggedCall {
	f, err := os.Open(filepath.Join(e.fakeDir, "calls.jsonl"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []loggedCall
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var c loggedCall
		if json.Unmarshal(sc.Bytes(), &c) == nil {
			out = append(out, c)
		}
	}
	return out
}

// verb is argv[2:4] of a herdr call (after --session S).
func verb(c loggedCall) string {
	if len(c.Argv) < 4 {
		if len(c.Argv) == 3 {
			return c.Argv[2]
		}
		return ""
	}
	return c.Argv[2] + " " + c.Argv[3]
}

func (e *env) count(v string) int {
	n := 0
	for _, c := range e.calls() {
		if verb(c) == v {
			n++
		}
	}
	return n
}

func (e *env) has(v string) bool { return e.count(v) > 0 }

func (e *env) callsOf(v string) []loggedCall {
	var out []loggedCall
	for _, c := range e.calls() {
		if verb(c) == v {
			out = append(out, c)
		}
	}
	return out
}

func (e *env) verbIndex(v string) int {
	for i, c := range e.calls() {
		if verb(c) == v {
			return i
		}
	}
	return -1
}

func (e *env) writeResult(status string) {
	b, _ := json.Marshal(map[string]any{"status": status, "summary": "done"})
	_ = os.WriteFile(e.result, b, 0o644)
}

func (e *env) start(x *Executor, pkg string) (*host.Session, error) {
	return x.Start(context.Background(), pkg, e.repo, "main", "standard", "opus", "run-1", e.state, nil)
}

func (e *env) mustStart(x *Executor, pkg string) *host.Session {
	e.t.Helper()
	s, err := e.start(x, pkg)
	if err != nil {
		e.t.Fatalf("start %s: %v\nstderr: %s", pkg, err, x.Stderr)
	}
	return s
}

func (e *env) manualSession() *host.Session {
	return &host.Session{Pkg: "a", Name: "herdr", Worktree: e.repo, Terminal: "p-a", Extra: map[string]any{
		"workspace": "w1", "pane": "w1:p1", "state_dir": e.state, "repo": e.repo}}
}

func needOf(t *testing.T, err error) *host.NeedError {
	t.Helper()
	if err == nil {
		t.Fatal("expected a need, got nil")
	}
	n, ok := err.(*host.NeedError)
	if !ok {
		t.Fatalf("expected *host.NeedError, got %T %v", err, err)
	}
	return n
}

func containsAll(s string, parts ...string) error {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return fmt.Errorf("%q not in %q", p, s)
		}
	}
	return nil
}

func argAfter(argv []string, flag string) string {
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

// jsonMarshal is compact JSON of a report.
func jsonMarshal(v any) ([]byte, error) {
	b, err := fsutil.MarshalJSON(v)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	err = json.Compact(&out, b)
	return out.Bytes(), err
}

func hasArg(argv []string, a string) bool {
	for _, x := range argv {
		if x == a {
			return true
		}
	}
	return false
}
