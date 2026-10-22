package runner

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
)

// fakeHost is an interactive host whose sessions are task worktrees and
// whose "Claude" is a function writing files and a result.
type fakeHost struct {
	name      string
	enabled   host.Tri
	available bool
	reason    string

	mu       sync.Mutex
	sessions map[string]*host.Session
	started  []string
	prompts  []string
	closed   []string
	down     bool
	work     func(pid, tree string)
	wait     func(pid string) (map[string]any, error)
	stateDir string
}

func newFakeHost(name string) *fakeHost {
	return &fakeHost{name: name, available: true, sessions: map[string]*host.Session{},
		wait: func(string) (map[string]any, error) { return map[string]any{"status": "done", "summary": "ok"}, nil }}
}

func (h *fakeHost) Name() string              { return h.name }
func (h *fakeHost) Enabled() host.Tri         { return h.enabled }
func (h *fakeHost) Available() (bool, string) { return h.available, h.reason }
func (h *fakeHost) ModelFor(string) string    { return "opus" }
func (h *fakeHost) Settings() host.Settings {
	return host.Settings{TaskTimeout: time.Minute, KeepSessions: true}
}
func (h *fakeHost) SetStateDir(d string)        { h.stateDir = d }
func (h *fakeHost) SetEventFunc(host.EventFunc) {}
func (h *fakeHost) Shutdown()                   { h.down = true }

func (h *fakeHost) Session(pkg, _ string) (*host.Session, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sessions[pkg]
	return s, ok
}

func (h *fakeHost) Start(_ context.Context, pid, repo, baseRef, _, _, _, _ string, _ []string) (*host.Session, error) {
	res, err := isolate.Create(repo, "pkg-"+pid, isolate.CreateOptions{Backend: isolate.BackendGit, Onto: baseRef})
	if err != nil {
		return nil, err
	}
	s := &host.Session{Pkg: pid, Name: h.name, Worktree: res.Path, Terminal: "t-" + pid}
	h.mu.Lock()
	h.sessions[pid] = s
	h.started = append(h.started, pid)
	h.mu.Unlock()
	return s, nil
}

func (h *fakeHost) Prompt(_ context.Context, s *host.Session, _ []string, _ string, _ time.Duration) error {
	h.mu.Lock()
	h.prompts = append(h.prompts, s.Pkg)
	h.mu.Unlock()
	if h.work != nil {
		h.work(s.Pkg, s.Worktree)
	}
	return nil
}

func (h *fakeHost) WaitResult(_ context.Context, s *host.Session, _ string, _ time.Duration) (map[string]any, error) {
	return h.wait(s.Pkg)
}

func (h *fakeHost) Close(s *host.Session, remove bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = append(h.closed, s.Pkg)
	if remove {
		delete(h.sessions, s.Pkg)
	}
	return nil
}

func (w *waveRepo) runWithHost(ex Executor, d Deps, mod ...func(*Options)) (Report, int) {
	w.t.Helper()
	d.Executor, d.AgentExec, d.Stderr = ex, []string{agentExec(w.t)}, w
	return Run(context.Background(), w.opts(mod...), d)
}

var delegated = map[string]any{"status": "delegate", "executor": "claude", "model": "sonnet"}

func TestDelegatedPackageRunsInTheInteractiveHost(t *testing.T) {
	w := newWaveRepo(t)
	w.verifyMode = "final"
	w.plan(pkgDef{id: "A"})
	h := newFakeHost("herdr")
	h.work = func(pid, tree string) { writeFile(t, filepath.Join(tree, "a.txt"), "by claude\n") }
	ex := newFake(t, map[string][]round{"A": {result(delegated, map[string]string{})}})
	rep, code := w.runWithHost(ex, Deps{Host: h})
	eq(t, "exit", code, 0)
	eqJSON(t, "integrated", rep.Integrated, []string{"A"})
	if got, _ := w.intFile("a.txt"); got != "by claude\n" {
		t.Errorf("a.txt %q", got)
	}
	p := w.state().Packages["A"]
	eq(t, "executor", *p.Executor, "herdr")
	eq(t, "session", p.Session, any(true))
	eqJSON(t, "closed", h.closed, []string{"A"})
	if !h.down {
		t.Error("the host was not shut down")
	}
	end := w.details("dispatch-end", "A")
	if len(end) != 1 || end[0]["status"] != "ok" || end[0]["executor"] != "herdr" {
		t.Errorf("dispatch-end %v", end)
	}
}

func TestDialogStallIsANeedThenAwaitResumesTheSameSession(t *testing.T) {
	w := newWaveRepo(t)
	w.verifyMode = "final"
	w.plan(pkgDef{id: "A"})
	h := newFakeHost("herdr")
	h.work = func(pid, tree string) { writeFile(t, filepath.Join(tree, "a.txt"), "after dialog\n") }
	stalled := true
	h.wait = func(pid string) (map[string]any, error) {
		if stalled {
			return nil, &host.NeedError{Kind: host.KindStalled, Detail: "dialog:\nAllow write?\nterminal: t-A",
				Data: map[string]any{"network_denials": []any{map[string]any{"host": "x.example.com", "port": 443}}}}
		}
		return map[string]any{"status": "done", "summary": "finished"}, nil
	}
	ex := newFake(t, map[string][]round{"A": {result(delegated, map[string]string{})}})
	_, code := w.runWithHost(ex, Deps{Host: h})
	eq(t, "exit", code, 1)
	eqJSON(t, "needs", w.needKinds(), []kindOf{{"A", "delegate"}})
	var d map[string]any
	_ = json.Unmarshal([]byte(w.state().Needs[0].Detail), &d)
	detail, _ := d["detail"].(string)
	if d["orca"] != "stalled" || !strings.Contains(detail, "approve it in Herdr (terminal t-A)") ||
		!strings.Contains(detail, "--await A") || d["network_denials"] == nil {
		t.Errorf("need detail %v", d)
	}

	// The registry still holds the session: --await hands A back.
	tree := h.sessions["A"].Worktree
	b, _ := json.Marshal(map[string]any{"A": map[string]any{"pkg": "A", "terminal": "t-A", "worktree": tree}})
	writeFile(t, filepath.Join(w.waveDir, "herdr-sessions.json"), string(b))
	res, mcode := Mark(MarkOptions{State: w.statePath, Await: "A"})
	eq(t, "await", mcode, 0)
	eqJSON(t, "await result", res, map[string]any{"pkg": "A", "status": "implementing", "detail": "await-orca"})
	stalled = false
	rep, code := w.runWithHost(newFake(t, nil), Deps{Host: h})
	eq(t, "rerun exit", code, 0)
	eqJSON(t, "integrated", rep.Integrated, []string{"A"})
	eq(t, "one start", len(h.started), 1)
}

func TestInteractiveHostSelection(t *testing.T) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A"})
	herdrHost, orcaHost := newFakeHost("herdr"), newFakeHost("orca")
	// interactive_host: none (the fixture's config) keeps delegate a need.
	ex := newFake(t, map[string][]round{"A": {result(delegated, map[string]string{})}})
	_, code := w.runWithHost(ex, Deps{Herdr: herdrHost, Orca: orcaHost})
	eq(t, "exit", code, 1)
	eq(t, "no host started", len(herdrHost.started)+len(orcaHost.started), 0)
	eq(t, "width", w.details("max-in-flight", "")[0]["source"], any("auto"))

	// auto prefers herdr, and herdr widens max_in_flight: auto.
	w2 := newWaveRepo(t)
	w2.verifyMode = "final"
	w2.plan(pkgDef{id: "A"})
	writeFile(t, filepath.Join(w2.home, ".claude", "orchestra.yaml"), "interactive_host: auto\n")
	herdrHost.work = func(pid, tree string) { writeFile(t, filepath.Join(tree, "a.txt"), "a\n") }
	ex = newFake(t, map[string][]round{"A": {result(delegated, map[string]string{})}})
	_, code = w2.runWithHost(ex, Deps{Herdr: herdrHost, Orca: orcaHost})
	eq(t, "exit", code, 0)
	eq(t, "herdr started", len(herdrHost.started), 1)
	eq(t, "auto width", w2.details("max-in-flight", "")[0]["value"], any(float64(autoMaxInFlight())))

	// An `enabled: true` host that is unreachable stops the run up front.
	w3 := newWaveRepo(t)
	w3.plan(pkgDef{id: "A"})
	writeFile(t, filepath.Join(w3.home, ".claude", "orchestra.yaml"), "interactive_host: auto\n")
	down := newFakeHost("orca")
	down.enabled, down.available, down.reason = host.TriTrue, false, "no orca binary"
	off := newFakeHost("herdr")
	off.available = false
	rep, code := w3.runWithHost(newFake(t, nil), Deps{Herdr: off, Orca: down})
	eq(t, "exit", code, 5)
	eq(t, "reason", *rep.Reason, "orca unavailable: no orca binary")
}

func TestMaxInFlightResolution(t *testing.T) {
	for _, c := range []struct {
		name   string
		option int
		config string
		want   int
		source string
	}{
		{"option wins", 3, "wave:\n  max_in_flight: 5\n", 3, "option"},
		{"config", 0, "wave:\n  max_in_flight: 5\n", 5, "config"},
		{"config auto", 0, "wave:\n  max_in_flight: auto\n", 4, "auto"},
		{"default", 0, "", 4, "auto"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWaveRepo(t)
			w.checks(c.config)
			w.plan(pkgDef{id: "A"})
			w.run(newFake(t, map[string][]round{"A": {edit(map[string]string{"a.txt": "a\n"})}}),
				func(o *Options) { o.MaxInFlight = c.option })
			d := w.details("max-in-flight", "")[0]
			eq(t, "value", d["value"], any(float64(c.want)))
			eq(t, "source", d["source"], any(c.source))
		})
	}
}

func TestMaxInFlightAutoByHost(t *testing.T) {
	for _, c := range []struct {
		host string // "" = no interactive host
		want int
	}{
		{"herdr", autoMaxInFlight()},
		{"orca", autoMaxInFlight()},
		{"other", defaultMaxInFlight},
		{"", defaultMaxInFlight},
	} {
		t.Run("host="+c.host, func(t *testing.T) {
			w := newWaveRepo(t)
			w.plan(pkgDef{id: "A"})
			ex := newFake(t, map[string][]round{"A": {edit(map[string]string{"a.txt": "a\n"})}})
			if c.host == "" {
				w.run(ex)
			} else {
				w.runWithHost(ex, Deps{Host: newFakeHost(c.host)})
			}
			d := w.details("max-in-flight", "")[0]
			eq(t, "value", d["value"], any(float64(c.want)))
			eq(t, "source", d["source"], any("auto"))
		})
	}
}
