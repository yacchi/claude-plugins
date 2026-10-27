package runner

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// initRequestRoot is the request workspace root newWaveRepo's XDG_STATE_HOME
// puts under the test's temp home.
func initRequestRoot(w *waveRepo) string {
	return paths.RealPath(filepath.Join(w.home, ".local", "state", "claude-orchestra", "requests"))
}

// The integration tree is the request's, cut from its assembly: its parent is
// an assembly under the request workspace, not the user's checkout.
func TestInitRequestIntegrationLivesInRequestAssembly(t *testing.T) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A", owned: []string{"a.txt"}})
	ex := newFake(t, map[string][]round{"A": {edit(map[string]string{"a.txt": "a\n"})}})
	rep, code := w.run(ex)
	if rep.Status == "error" {
		t.Fatalf("run errored: %+v", rep)
	}
	if code != 0 {
		t.Fatalf("exit code %d", code)
	}

	integ := paths.RealPath(w.intPath())
	wantSuffix := filepath.Join(".integration", "repo", "wave-int")
	if !strings.HasSuffix(integ, wantSuffix) || !strings.HasPrefix(integ, initRequestRoot(w)+string(filepath.Separator)) {
		t.Fatalf("integration path %s; want under %s ending in %s", integ, initRequestRoot(w), wantSuffix)
	}
	asm := isolate.ReadParent(integ)
	if asm == "" {
		t.Fatalf("integration tree %s has no parent marker", integ)
	}
	if got := isolate.ReadRole(asm); got != isolate.RoleAssembly {
		t.Errorf("parent %s role %q; want %q", asm, got, isolate.RoleAssembly)
	}
	if !strings.HasPrefix(paths.RealPath(asm), initRequestRoot(w)+string(filepath.Separator)) {
		t.Errorf("assembly %s is outside the request root %s", asm, initRequestRoot(w))
	}
}

// A checkout with uncommitted work is refused before anything is dispatched,
// with the edited path and both choices in the reason.
func TestInitRequestUncommittedRefusesStart(t *testing.T) {
	w := newWaveRepo(t)
	writeFile(t, filepath.Join(w.repo, "shared.txt"), "edited\n")
	w.plan(pkgDef{id: "A", owned: []string{"a.txt"}})
	ex := newFake(t, nil)
	rep, code := w.run(ex)
	eq(t, "status", rep.Status, "error")
	eq(t, "kind", rep.Kind, "uncommitted")
	if rep.Reason == nil || !strings.Contains(*rep.Reason, "shared.txt") || !strings.Contains(*rep.Reason, "--uncommitted include") {
		t.Errorf("reason %v", rep.Reason)
	}
	eq(t, "exit code", code, 3)
	eq(t, "worktrees", strings.Count(w.git(w.repo, "worktree", "list", "--porcelain"), "worktree "), 1)
	eq(t, "dispatched", len(ex.dispatched()), 0)
}

// The request's choice decides whether the checkout's edit reaches the
// integration tree.
func TestInitRequestUncommittedChoice(t *testing.T) {
	for _, tc := range []struct {
		name, choice, want string
	}{
		{"ignore starts from HEAD", isolate.UncommittedIgnore, baseLines},
		{"include builds on the edit", isolate.UncommittedInclude, "edited\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWaveRepo(t)
			writeFile(t, filepath.Join(w.repo, "shared.txt"), "edited\n")
			w.plan(pkgDef{id: "A", owned: []string{"a.txt"}})
			ex := newFake(t, map[string][]round{"A": {edit(map[string]string{"a.txt": "a\n"})}})
			rep, _ := w.run(ex, func(o *Options) { o.Uncommitted = tc.choice })
			if rep.Status == "error" {
				t.Fatalf("run errored: %+v", rep)
			}
			got, ok := w.intFile("shared.txt")
			if !ok || got != tc.want {
				t.Errorf("integration shared.txt = %q; want %q", got, tc.want)
			}
		})
	}
}
