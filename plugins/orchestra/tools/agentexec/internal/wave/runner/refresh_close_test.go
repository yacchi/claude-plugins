package runner

import (
	"path/filepath"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
)

// refreshCloseCall is one Close the runner made on a session, with the
// baseline its tree had at that moment.
type refreshCloseCall struct {
	Pkg            string
	Baseline       string
	RemoveWorktree bool
}

// refreshCloseHost records every Close and, like the real Close, clears the
// session so the package no longer has a live one.
type refreshCloseHost struct {
	*fakeHost
	closes []refreshCloseCall
}

func (h *refreshCloseHost) Close(s *host.Session, remove bool) error {
	baseline, _ := isolate.ReadBaseline(s.Worktree)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closes = append(h.closes, refreshCloseCall{s.Pkg, baseline, remove})
	delete(h.sessions, s.Pkg)
	return nil
}

// refreshCloseLive returns a host named name with a live session for pkg on
// the tree at path.
func refreshCloseLive(name, pkg, path string) *refreshCloseHost {
	h := &refreshCloseHost{fakeHost: newFakeHost(name)}
	h.sessions[pkg] = &host.Session{Pkg: pkg, Name: name, Worktree: path, Terminal: "t-" + pkg}
	return h
}

// refreshCloseKeepTree is the closes that keep the tree, the only kind the
// refresh makes; the close after a package finishes removes it.
func refreshCloseKeepTree(h *refreshCloseHost) []refreshCloseCall {
	var out []refreshCloseCall
	for _, c := range h.closes {
		if !c.RemoveWorktree {
			out = append(out, c)
		}
	}
	return out
}

// refreshCloseStaleB makes B's task tree on an integration HEAD that has
// moved on, so the runner must refresh it before dispatching B. It returns
// the tree's path.
func refreshCloseStaleB(w *waveRepo, files map[string]string) string {
	head := w.git(w.repo, "rev-parse", "HEAD")
	created, err := isolate.Create(w.repo, "wave-int", isolate.CreateOptions{Backend: isolate.BackendGit, Onto: head})
	if err != nil {
		w.t.Fatal(err)
	}
	_ = isolate.WriteRole(created.Path, isolate.RoleIntegration)
	stale, err := isolate.Create(created.Path, "pkg-B", isolate.CreateOptions{Backend: isolate.BackendGit})
	if err != nil {
		w.t.Fatal(err)
	}
	for rel, text := range files {
		writeFile(w.t, filepath.Join(stale.Path, rel), text)
	}
	return stale.Path
}

// refreshCloseRun runs A, then B on the stale tree, with h as the host.
func refreshCloseRun(t *testing.T, w *waveRepo, h host.Host) {
	t.Helper()
	ex := newFake(t, map[string][]round{
		"A": {edit(map[string]string{"a.txt": "a\n"})},
		"B": {edit(map[string]string{"b2.txt": "b2\n"})},
	})
	_, code := w.runWithHost(ex, Deps{Host: h})
	eq(t, "exit", code, 0)
}

func TestRefreshCloseClosesHerdrSessionBeforeTheTreeMoves(t *testing.T) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A"}, pkgDef{id: "B", dependsOn: []string{"A"}})
	tree := refreshCloseStaleB(w, map[string]string{"b.txt": "b from before\n"})
	before, _ := isolate.ReadBaseline(tree)
	h := refreshCloseLive("herdr", "B", tree)
	refreshCloseRun(t, w, h)

	eqJSON(t, "keep-tree closes", refreshCloseKeepTree(h), []refreshCloseCall{{"B", before, false}})
	if len(w.details("refresh", "B")) == 0 {
		t.Error("no refresh event")
	}
}

func TestRefreshCloseSkipsHerdrWithoutLiveSession(t *testing.T) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A"}, pkgDef{id: "B", dependsOn: []string{"A"}})
	refreshCloseStaleB(w, map[string]string{"b.txt": "b from before\n"})
	h := &refreshCloseHost{fakeHost: newFakeHost("herdr")}
	refreshCloseRun(t, w, h)

	eqJSON(t, "keep-tree closes", refreshCloseKeepTree(h), []refreshCloseCall(nil))
	if len(w.details("refresh", "B")) == 0 {
		t.Error("no refresh event")
	}
}

func TestRefreshCloseLeavesOrcaAlone(t *testing.T) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A"}, pkgDef{id: "B", dependsOn: []string{"A"}})
	tree := refreshCloseStaleB(w, map[string]string{"b.txt": "b from before\n"})
	h := refreshCloseLive("orca", "B", tree)
	refreshCloseRun(t, w, h)

	eqJSON(t, "keep-tree closes", refreshCloseKeepTree(h), []refreshCloseCall(nil))
	if len(w.details("refresh", "B")) == 0 {
		t.Error("no refresh event")
	}
}
