package isolate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

func TestRefreshCarriesEditsNewDeletedAndBinaryFiles(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, filepath.Join(repo, "doomed.txt"), "will be deleted\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "doomed")
	wt := create(t, repo, "alpha", "")
	writeFile(t, filepath.Join(wt, "README.md"), "hello\nALPHA\n")
	writeFile(t, filepath.Join(wt, "new.txt"), "brand new\n")
	blob := "\x00\x01binary\xff\xfe"
	writeFile(t, filepath.Join(wt, "blob.bin"), blob)
	if err := os.Remove(filepath.Join(wt, "doomed.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repo, "main-only.txt"), "main\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "advance")
	head := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))

	res, err := Refresh(repo, "alpha", "", "")
	if err != nil || res.Status != "ok" || res.Baseline != head || res.PatchFile == "" {
		t.Fatalf("refresh: %+v %v", res, err)
	}
	if got := readFile(t, filepath.Join(res.Path, "README.md")); got != "hello\nALPHA\n" {
		t.Errorf("edit lost: %q", got)
	}
	if got := readFile(t, filepath.Join(res.Path, "blob.bin")); got != blob {
		t.Errorf("binary changed: %q", got)
	}
	if exists(filepath.Join(res.Path, "doomed.txt")) {
		t.Error("deletion lost")
	}
	if !exists(filepath.Join(res.Path, "new.txt")) || !exists(filepath.Join(res.Path, "main-only.txt")) {
		t.Error("tree not recreated at the new base with the new file")
	}
	if !exists(res.PatchFile) {
		t.Error("patch file missing")
	}
}

func TestUnmergedConflictsCountsMarkersOnAnyLineEnding(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	writeFile(t, p, "<<<<<<< a\r\nx\r\n=======\r\n>>>>>>> b\r<<<<<<< c\rno <<<<<<< here\n")
	if n := countConflictMarkers(p); n != 2 {
		t.Errorf("markers = %d, want 2", n)
	}
	if n := countConflictMarkers(filepath.Join(dir, "missing")); n != 0 {
		t.Errorf("missing file counted %d", n)
	}
}

// TestAdoptedTaskResolutionPrefersTheSessionAndRefusesToGuess: two trees
// carrying the same task id (adopt itself refuses, so the second marker is
// forged) resolve by the caller's session, and are ambiguous otherwise.
func TestAdoptedTaskResolutionPrefersTheSessionAndRefusesToGuess(t *testing.T) {
	repo := newRepo(t)
	tmp := filepath.Dir(repo)
	one, two := filepath.Join(tmp, "ext1"), filepath.Join(tmp, "ext2")
	git(t, repo, "worktree", "add", "-q", "-b", "e1", one)
	git(t, repo, "worktree", "add", "-q", "-b", "e2", two)
	res, err := Adopt(repo, "ext", one, AdoptOptions{Session: sessA})
	if err != nil || res.Status != "adopted" {
		t.Fatalf("adopt: %+v %v", res, err)
	}
	if res, _ := Adopt(repo, "ext", two, AdoptOptions{Session: sessB}); res.Status != "error" {
		t.Fatalf("second tree adopted under the same task: %+v", res)
	}
	if err := writeAdoption(two, "ext", sessB); err != nil {
		t.Fatal(err)
	}
	for session, want := range map[string]string{sessA: one, sessB: two} {
		if path, _, ok := ResolveWorktree(repo, "ext", session); !ok || path != want {
			t.Errorf("session %s resolved %q, want %q", session[:8], path, want)
		}
	}
	if _, err := Unadopt(repo, "ext", "cccccccc-0000-4000-8000-000000000003"); err == nil ||
		!strings.Contains(err.Error(), "ambiguous adopted worktrees") {
		t.Errorf("ambiguity not reported: %v", err)
	}
	if res, err := Unadopt(repo, "ext", sessB); err != nil || res.Path != two {
		t.Errorf("unadopt by session: %+v %v", res, err)
	}
	if _, ok := ReadAdopted(two); ok {
		t.Error("marker left behind")
	}
	if !exists(filepath.Join(two, "README.md")) {
		t.Error("unadopt touched the files")
	}
}

// refreshIntegrationWithChild plants integration tree X under repo and work
// tree t1 under X, with an edit in t1 and a newer commit in X.
func refreshIntegrationWithChild(t *testing.T, repo string) (x, t1, xHead string) {
	t.Helper()
	x = plantTree(t, repo, "X", RoleIntegration, "")
	t1 = plantTree(t, x, "t1", "", x)
	writeFile(t, filepath.Join(t1, "README.md"), "hello\nT1\n")
	writeFile(t, filepath.Join(x, "new.txt"), "from X\n")
	git(t, x, "add", "-A")
	git(t, x, "commit", "-q", "-m", "advance X")
	xHead = strings.TrimSpace(git(t, x, "rev-parse", "HEAD"))
	return x, t1, xHead
}

func TestRefreshDefaultsToTheParentHeadAndKeepsParent(t *testing.T) {
	repo := newRepo(t)
	x, _, xHead := refreshIntegrationWithChild(t, repo)

	res, err := Refresh(repo, "t1", "", "")
	if err != nil || res.Status != "ok" || res.Baseline != xHead {
		t.Fatalf("refresh: %+v %v", res, err)
	}
	if got := readFile(t, filepath.Join(res.Path, "README.md")); got != "hello\nT1\n" {
		t.Errorf("edit lost: %q", got)
	}
	if got := ReadParent(res.Path); got != paths.RealPath(x) {
		t.Errorf("parent marker = %q, want %q", got, paths.RealPath(x))
	}
}

func TestRefreshExplicitOntoWinsOverParentHead(t *testing.T) {
	repo := newRepo(t)
	x, _, _ := refreshIntegrationWithChild(t, repo)
	mainSHA := strings.TrimSpace(git(t, repo, "rev-parse", "main"))

	res, err := Refresh(repo, "t1", "main", "")
	if err != nil || res.Status != "ok" || res.Baseline != mainSHA {
		t.Fatalf("refresh: %+v %v", res, err)
	}
	if got := ReadParent(res.Path); got != paths.RealPath(x) {
		t.Errorf("parent marker = %q, want %q", got, paths.RealPath(x))
	}
}

func TestRefreshRefusesAnAssembly(t *testing.T) {
	repo := newRepo(t)
	plantTree(t, repo, "A", RoleAssembly, "")

	res, err := Refresh(repo, "A", "", "")
	if err != nil || res.Status != "error" || res.Note != "refusing to refresh an assembly worktree" {
		t.Fatalf("refresh: %+v %v", res, err)
	}
}

func TestRefreshRefusesWhenParentIsGoneAndTouchesNothing(t *testing.T) {
	repo := newRepo(t)
	x, t1, _ := refreshIntegrationWithChild(t, repo)
	git(t, repo, "worktree", "remove", "--force", x)

	res, err := Refresh(repo, "t1", "", "")
	want := "parent tree " + paths.RealPath(x) + " is gone; nothing touched"
	if err != nil || res.Status != "error" || res.Note != want {
		t.Fatalf("refresh: %+v %v", res, err)
	}
	if got := readFile(t, filepath.Join(t1, "README.md")); got != "hello\nT1\n" {
		t.Errorf("edit lost: %q", got)
	}
	if exists(filepath.Join(Home(repo), patchesDirName)) {
		t.Error("patch file written before the parent check")
	}
}
