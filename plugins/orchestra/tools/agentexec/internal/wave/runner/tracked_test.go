package runner

import (
	"path/filepath"
	"strings"
	"testing"
)

// A start commit that tracks a dependency store would check it out into
// every package worktree, so the run refuses before creating anything.
func TestTrackedDependencyStoreRefusesStart(t *testing.T) {
	w := newWaveRepo(t)
	writeFile(t, filepath.Join(w.repo, "node_modules", "dep", "i.js"), "x\n")
	w.git(w.repo, "add", "-f", "node_modules")
	w.git(w.repo, "commit", "-q", "-m", "track deps")
	w.plan(pkgDef{id: "A", owned: []string{"a.txt"}})
	ex := newFake(t, nil)
	rep, code := w.run(ex)
	eq(t, "status", rep.Status, "error")
	eq(t, "kind", rep.Kind, "environment")
	if rep.Reason == nil || !strings.Contains(*rep.Reason, "node_modules/") || !strings.Contains(*rep.Reason, "isolation.keep_paths") {
		t.Errorf("reason %v", rep.Reason)
	}
	if code == 0 {
		t.Errorf("exit code 0 for a refused start")
	}
	eq(t, "worktrees", strings.Count(w.git(w.repo, "worktree", "list", "--porcelain"), "worktree "), 1)
	eq(t, "dispatched", len(ex.dispatched()), 0)
}

// The same directory present only on disk never reaches a worktree, so it
// does not block the start.
func TestUntrackedDependencyStoreDoesNotBlockStart(t *testing.T) {
	w := newWaveRepo(t)
	writeFile(t, filepath.Join(w.repo, "node_modules", "dep", "i.js"), "x\n")
	w.plan(pkgDef{id: "A", owned: []string{"a.txt"}})
	ex := newFake(t, map[string][]round{"A": {edit(map[string]string{"a.txt": "a\n"})}})
	rep, _ := w.run(ex)
	if rep.Status == "error" {
		t.Fatalf("untracked store blocked the start: %+v", rep)
	}
	if !isDir(w.intPath()) {
		t.Errorf("integration worktree %s was not created", w.intPath())
	}
}
