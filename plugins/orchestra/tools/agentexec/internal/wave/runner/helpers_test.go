package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/state"
)

// Tests run against real temp git repos; the executor is a fake that
// creates the task worktree the way `dispatch --isolate always --workdir
// <integration path> --task pkg-<id>` does and edits files in it. Gated
// self-verify and the default gate run a built agent-exec.

var agentExecBin struct {
	once sync.Once
	path string
	err  error
}

// agentExec builds cmd/agent-exec once per test binary.
func agentExec(t *testing.T) string {
	t.Helper()
	agentExecBin.once.Do(buildAgentExec)
	if agentExecBin.err != nil {
		t.Fatal(agentExecBin.err)
	}
	return agentExecBin.path
}

func buildAgentExec() {
	{
		dir, err := os.MkdirTemp("", "runner-agent-exec-")
		if err != nil {
			agentExecBin.err = err
			return
		}
		goBin, err := exec.LookPath("go")
		if err != nil {
			goBin = filepath.Join(runtime.GOROOT(), "bin", "go")
		}
		out := filepath.Join(dir, "agent-exec")
		cmd := exec.Command(goBin, "build", "-o", out, "./cmd/agent-exec")
		cmd.Dir = moduleDir
		if b, err := cmd.CombinedOutput(); err != nil {
			agentExecBin.err = fmt.Errorf("go build: %v\n%s", err, b)
			return
		}
		agentExecBin.path = out
	}
}

// moduleDir is the module root, resolved before any test changes the cwd.
var moduleDir string

func TestMain(m *testing.M) {
	moduleDir, _ = filepath.Abs("../../..")
	// Built before any test points HOME (and so the module cache) elsewhere.
	agentExecBin.once.Do(buildAgentExec)
	code := m.Run()
	if agentExecBin.path != "" {
		_ = os.RemoveAll(filepath.Dir(agentExecBin.path))
	}
	os.Exit(code)
}

// --- fake executor -----------------------------------------------------------

type spec struct {
	PromptFiles               []string
	Class, Workdir, Task, Run string
}

type call struct {
	Pkg, Token, WorkdirHead string
	Exhausted, PromptFiles  []string
	NoResume                bool
}

type round func(t *testing.T, s spec, exhausted []string) map[string]any

type fakeExecutor struct {
	t        *testing.T
	mu       sync.Mutex
	rounds   map[string][]round
	tokens   map[string]spec
	calls    []call
	lock     sync.Locker
	onStart  func(pid string, s spec)
	onReturn func(pid string)
}

func newFake(t *testing.T, rounds map[string][]round) *fakeExecutor {
	return &fakeExecutor{t: t, rounds: rounds, tokens: map[string]spec{}}
}

func (f *fakeExecutor) SetStartLock(l sync.Locker) { f.lock = l }

func (f *fakeExecutor) Prepare(promptFiles []string, cls, workdir, task, runID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	token := fmt.Sprintf("tok-%d", len(f.tokens))
	f.tokens[token] = spec{append([]string{}, promptFiles...), cls, workdir, task, runID}
	return token, nil
}

func (f *fakeExecutor) Dispatch(token, cls string, exhausted []string, noResume bool) map[string]any {
	f.mu.Lock()
	s := f.tokens[token]
	f.mu.Unlock()
	pid := strings.TrimPrefix(s.Task, "pkg-")
	if f.onStart != nil {
		f.onStart(pid, s)
	}
	if f.lock != nil {
		f.lock.Lock()
	}
	head, _ := gitRun(s.Workdir, "rev-parse", "HEAD")
	f.mu.Lock()
	f.calls = append(f.calls, call{Pkg: pid, Token: token, WorkdirHead: strings.TrimSpace(head),
		Exhausted: append([]string{}, exhausted...), PromptFiles: s.PromptFiles, NoResume: noResume})
	fns := f.rounds[pid]
	if len(fns) == 0 {
		f.mu.Unlock()
		if f.lock != nil {
			f.lock.Unlock()
		}
		panic("unexpected dispatch for " + pid)
	}
	fn := fns[0]
	f.rounds[pid] = fns[1:]
	f.mu.Unlock()
	out := fn(f.t, s, exhausted)
	if f.lock != nil {
		f.lock.Unlock()
	}
	if f.onReturn != nil {
		f.onReturn(pid)
	}
	return out
}

func (f *fakeExecutor) dispatched() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.Pkg
	}
	return out
}

func (f *fakeExecutor) callsOf(pid string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.Pkg == pid {
			out = append(out, c)
		}
	}
	return out
}

func createTree(t *testing.T, s spec) string {
	t.Helper()
	res, err := isolate.Create(s.Workdir, s.Task, isolate.CreateOptions{Backend: isolate.BackendGit})
	if err != nil || res.Path == "" {
		t.Errorf("create %s: %v %+v", s.Task, err, res)
	}
	return res.Path
}

type editOpt struct {
	answer, session, executor string
}

// edit creates/reuses the task tree and writes files into it.
func edit(files map[string]string, opts ...editOpt) round {
	o := editOpt{answer: "done", executor: "pi"}
	if len(opts) > 0 {
		o = opts[0]
		if o.answer == "" {
			o.answer = "done"
		}
		if o.executor == "" {
			o.executor = "pi"
		}
	}
	return func(t *testing.T, s spec, _ []string) map[string]any {
		path := createTree(t, s)
		for rel, text := range files {
			writeFile(t, filepath.Join(path, rel), text)
		}
		return map[string]any{"status": "ok", "answer": o.answer, "session_id": nullable(o.session),
			"executor": o.executor, "isolation": map[string]any{"isolate": true, "path": path, "workdir": path}}
	}
}

// result returns payload verbatim (after optional edits).
func result(payload map[string]any, files map[string]string) round {
	return func(t *testing.T, s spec, _ []string) map[string]any {
		out := map[string]any{}
		for k, v := range payload {
			out[k] = v
		}
		if files != nil {
			path := createTree(t, s)
			for rel, text := range files {
				writeFile(t, filepath.Join(path, rel), text)
			}
			out["isolation"] = map[string]any{"isolate": true, "path": path, "workdir": path}
		}
		return out
	}
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(path string) (string, bool) {
	b, err := os.ReadFile(path)
	return string(b), err == nil
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached in 30s")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// --- fixture ------------------------------------------------------------------------

const noBadCheck = "checks:\n  items:\n    - name: nobad\n      run: \"! grep -rqs BAD --include=*.txt .\"\n"

var baseLines = func() string {
	var b strings.Builder
	for n := 1; n <= 20; n++ {
		fmt.Fprintf(&b, "line %02d\n", n)
	}
	return b.String()
}()

type waveRepo struct {
	t                        *testing.T
	tmp, repo, home, waveDir string
	statePath, planPath      string
	verifyMode               string
	uiURL                    string
	uiErr                    error
	uiCalls                  int
	stderr                   strings.Builder
	stderrMu                 sync.Mutex
	clock                    func() float64
	sleep                    func(float64)
}

func (w *waveRepo) Write(p []byte) (int, error) {
	w.stderrMu.Lock()
	defer w.stderrMu.Unlock()
	return w.stderr.Write(p)
}

func newWaveRepo(t *testing.T) *waveRepo {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &waveRepo{t: t, tmp: tmp, repo: filepath.Join(tmp, "repo"), home: filepath.Join(tmp, "home"),
		waveDir: filepath.Join(tmp, "wave"), verifyMode: "gated", uiURL: "http://127.0.0.1:1/"}
	w.statePath = filepath.Join(w.waveDir, "state.json")
	w.planPath = filepath.Join(w.waveDir, "plan.json")
	gitconfig := filepath.Join(tmp, "gitconfig")
	writeFile(t, gitconfig, "[user]\n\tname = T\n\temail = t@example.com\n[init]\n\tdefaultBranch = main\n")
	for k, v := range map[string]string{
		"HOME": w.home, "GIT_CONFIG_GLOBAL": gitconfig, "GIT_CONFIG_SYSTEM": "/dev/null",
		"XDG_CONFIG_HOME": filepath.Join(w.home, ".config"), "XDG_STATE_HOME": filepath.Join(w.home, ".local", "state"),
		"XDG_CACHE_HOME": filepath.Join(w.home, ".cache"), "CLAUDE_CONFIG_DIR": filepath.Join(w.home, ".claude"),
		"ORCHESTRA_ALIVE_DIR":     filepath.Join(tmp, "alive"),
		"ORCHESTRA_WAVE_REGISTRY": filepath.Join(tmp, "registry", "waves.jsonl"),
		"ORCHESTRA_UI_STATE":      filepath.Join(tmp, "ui.json"),
		"CLAUDE_CODE_SESSION_ID":  "", "ORCHESTRA_WAVE_NO_UI": "", "ORCHESTRA_CONFIG_ROOT": "",
	} {
		t.Setenv(k, v)
	}
	writeFile(t, filepath.Join(w.repo, "shared.txt"), baseLines)
	writeFile(t, filepath.Join(w.home, ".claude", "orchestra.yaml"), "interactive_host: none\n")
	w.git(w.repo, "init", "-q")
	w.git(w.repo, "add", "-A")
	w.git(w.repo, "commit", "-q", "-m", "init")
	writeFile(t, filepath.Join(w.waveDir, "preamble.md"), "preamble\n")
	t.Chdir(w.repo)
	return w
}

func (w *waveRepo) git(dir string, args ...string) string {
	w.t.Helper()
	out, err := gitRun(dir, args...)
	if err != nil {
		w.t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(out)
}

func (w *waveRepo) checks(text string) {
	writeFile(w.t, filepath.Join(w.home, ".claude", "orchestra.yaml"), "interactive_host: none\n"+text)
}

type pkgDef struct {
	id, cls   string
	dependsOn []string
	owned     []string
}

func (w *waveRepo) plan(pkgs ...pkgDef) {
	var entries []map[string]any
	for _, p := range pkgs {
		specPath := filepath.Join(w.waveDir, "specs", p.id+".md")
		writeFile(w.t, specPath, "spec for "+p.id+"\n")
		cls := p.cls
		if cls == "" {
			cls = "light"
		}
		owned := p.owned
		if owned == nil {
			owned = []string{strings.ToLower(p.id) + ".txt"}
		}
		deps := p.dependsOn
		if deps == nil {
			deps = []string{}
		}
		entries = append(entries, map[string]any{"id": p.id, "spec": specPath, "cls": cls,
			"depends_on": deps, "files_owned": owned})
	}
	b, _ := json.Marshal(map[string]any{"preamble": []string{filepath.Join(w.waveDir, "preamble.md")}, "packages": entries})
	writeFile(w.t, w.planPath, string(b))
}

func (w *waveRepo) opts(mod ...func(*Options)) Options {
	o := Options{Plan: w.planPath, State: w.statePath, Into: "wave-int", Repo: w.repo, VerifyMode: w.verifyMode, FullEvery: 1}
	for _, m := range mod {
		m(&o)
	}
	return o
}

func (w *waveRepo) run(ex Executor, mod ...func(*Options)) (Report, int) {
	w.t.Helper()
	d := Deps{Executor: ex, AgentExec: []string{agentExecBinOrSkip(w.t)}, Stderr: w,
		StartUI: func() (string, error) { w.uiCalls++; return w.uiURL, w.uiErr },
		Clock:   w.clock, Sleep: w.sleep}
	return Run(context.Background(), w.opts(mod...), d)
}

// agentExecBinOrSkip builds the binary only for tests that reach it; the
// path is valid either way.
func agentExecBinOrSkip(t *testing.T) string { return agentExec(t) }

func (w *waveRepo) state() state.State {
	st, err := (&state.Store{Path: w.statePath}).Load()
	if err != nil {
		w.t.Fatal(err)
	}
	return st
}

func (w *waveRepo) status(pid string) string { return w.state().Packages[pid].Status }

func (w *waveRepo) intPath() string { return *w.state().Integration.Path }

func (w *waveRepo) intFile(rel string) (string, bool) {
	return readFile(filepath.Join(w.intPath(), rel))
}

type kindOf struct{ ID, Kind string }

func (w *waveRepo) needKinds() []kindOf {
	var out []kindOf
	for _, n := range w.state().Needs {
		id := ""
		if n.ID != nil {
			id = *n.ID
		}
		out = append(out, kindOf{id, n.Kind})
	}
	return out
}

type event struct {
	Event, Pkg, To string
	Detail         string
}

func (w *waveRepo) events(name string) []event {
	var out []event
	for _, ev := range state.ReadEvents(w.statePath, -1) {
		get := func(k string) string { v, _ := ev[k]; s, _ := v.(string); return s }
		if name == "" || get("event") == name {
			out = append(out, event{get("event"), get("pkg"), get("to"), get("detail")})
		}
	}
	return out
}

func (w *waveRepo) details(name, pkg string) []map[string]any {
	var out []map[string]any
	for _, e := range w.events(name) {
		if pkg != "*" && e.Pkg != pkg {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(e.Detail), &m) != nil {
			w.t.Fatalf("%s detail is not JSON: %q", name, e.Detail)
		}
		out = append(out, m)
	}
	return out
}

func eq[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func eqJSON(t *testing.T, what string, got, want any) {
	t.Helper()
	g, _ := fsutil.MarshalJSON(normalizeJSON(got))
	wnt, _ := fsutil.MarshalJSON(normalizeJSON(want))
	if string(g) != string(wnt) {
		t.Errorf("%s = %s, want %s", what, g, wnt)
	}
}

// normalizeJSON round-trips v through encoding/json so Go values compare
// by their JSON form.
func normalizeJSON(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	_ = json.Unmarshal(b, &out)
	return normalizeNumbers(out)
}

func ptr[T any](v T) *T { return &v }
