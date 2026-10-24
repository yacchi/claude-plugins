package runner

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/state"
)

// gitShim puts a git in front of PATH that runs action (shell, "$a" is the
// argument) for an argument equal to message, and the real git otherwise.
func gitShim(t *testing.T, message, action string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "git")
	writeFile(t, path, "#!/bin/sh\nn=$#\nfor a in \"$@\"; do\n"+
		"  if [ \"$a\" = '"+message+"' ]; then "+action+"; fi\n"+
		"  set -- \"$@\" \"$a\"\ndone\nshift $n\nexec '"+real+"' \"$@\"\n")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// integrateNeedOfB runs A then B, B's integrate commit tampered with by
// shim, and checks B ends on an `integrate` need with its tree kept.
func integrateNeedOfB(t *testing.T, action, excerpt string) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A"}, pkgDef{id: "B"})
	ex := newFake(t, map[string][]round{
		"A": {edit(map[string]string{"a.txt": "a\n"})}, "B": {edit(map[string]string{"b.txt": "b\n"})},
	})
	ex.onReturn = finishAfter(t, w, "A", "B")
	gitShim(t, "orchestra integrate pkg-B", action)
	rep, code := w.run(ex)
	eq(t, "exit", code, 1)
	eqJSON(t, "integrated", rep.Integrated, []string{"A"})
	eqJSON(t, "needs", w.needKinds(), []kindOf{{"B", "integrate"}})
	var detail struct{ Excerpt, Tree string }
	if err := json.Unmarshal([]byte(w.state().Needs[0].Detail), &detail); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail.Excerpt, excerpt) {
		t.Errorf("excerpt %q, want %q in it", detail.Excerpt, excerpt)
	}
	if text, ok := readFile(filepath.Join(detail.Tree, "b.txt")); !ok || text != "b\n" {
		t.Errorf("B's tree %q not kept with its work", detail.Tree)
	}
	gitDir := w.git(detail.Tree, "rev-parse", "--absolute-git-dir")
	if _, err := os.Stat(filepath.Join(gitDir, "orchestra-collected")); err == nil {
		t.Error("B's tree is marked collected")
	}
	if p := w.state().Packages["B"]; p.Commit != nil {
		t.Errorf("B has commit %s", *p.Commit)
	}
}

func TestFailedIntegrateCommitBecomesANeed(t *testing.T) {
	integrateNeedOfB(t, "echo 'error: forced commit failure' >&2; exit 1", "forced commit failure")
}

func TestAppliedWithoutIntegrateCommitBecomesANeed(t *testing.T) {
	// The commit lands under another subject: integrate says applied, but
	// no `orchestra integrate pkg-B` commit exists.
	integrateNeedOfB(t, `a="other subject"`, "no `orchestra integrate` commit landed")
}

func TestCleanIntegrationSavesARescuePatchFirst(t *testing.T) {
	w := newWaveRepo(t)
	writeFile(t, filepath.Join(w.repo, "staged.txt"), "staged work\n")
	w.git(w.repo, "add", "staged.txt")
	writeFile(t, filepath.Join(w.repo, "untracked.txt"), "untracked work\n")
	store := &state.Store{Path: w.statePath, Clock: func() float64 { return 0 }}
	if _, err := store.Init(w.planPath, nil, "integrate"); err != nil {
		t.Fatal(err)
	}
	r := &runner{intPath: w.repo, stateDir: w.waveDir, store: store}
	eq(t, "files", r.cleanIntegration("gate"), 2)
	if st := w.git(w.repo, "status", "--porcelain"); st != "" {
		t.Errorf("tree not reset: %q", st)
	}
	dirty := w.details("integration-dirty", "*")
	if len(dirty) != 1 {
		t.Fatalf("integration-dirty events %v", dirty)
	}
	patch, _ := dirty[0]["patch"].(string)
	if filepath.Dir(patch) != filepath.Join(w.waveDir, "rescue") {
		t.Fatalf("patch path %q", patch)
	}
	w.git(w.repo, "apply", "--index", patch)
	for name, want := range map[string]string{"staged.txt": "staged work\n", "untracked.txt": "untracked work\n"} {
		if got, _ := readFile(filepath.Join(w.repo, name)); got != want {
			t.Errorf("%s after applying the rescue patch: %q", name, got)
		}
	}
}
