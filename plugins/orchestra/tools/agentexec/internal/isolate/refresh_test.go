package isolate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
