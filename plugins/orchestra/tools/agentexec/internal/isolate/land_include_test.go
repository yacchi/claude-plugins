package isolate

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// includeU is the user's uncommitted edit to a.txt (line 1) in an include test.
const includeU = "ONE\ntwo\nthree\nfour\nfive\n"

// includeDirtyReason is the land refusal for an included assembly whose
// assembly work and the checkout's uncommitted work touch the same file.
const includeDirtyReason = "the checkout has uncommitted changes to files this land changes; the assembly was built on your uncommitted work, so commit it (or set it aside) and land again"

// includeBuild edits a.txt in the checkout without committing (the user's
// uncommitted work), then creates the assembly with the given mode.
func includeBuild(t *testing.T, repo, mode string) EnsureResult {
	t.Helper()
	writeFile(t, filepath.Join(repo, "a.txt"), includeU)
	res, err := EnsureAssembly(repo, mode)
	if err != nil || res.Status != "ready" {
		t.Fatalf("EnsureAssembly(%q) = %+v, %v; want ready", mode, res, err)
	}
	return res
}

func TestIncludeLandTakesOnlyAssemblyWork(t *testing.T) {
	repo := landRepo(t)
	head := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))
	res := includeBuild(t, repo, UncommittedInclude)
	writeFile(t, filepath.Join(res.Assembly, "c.txt"), "c\n")
	landCommit(t, res.Assembly, "orchestra integrate pc")

	got, err := Land(repo, res.Task, "feat: add c")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "landed" || !slices.Equal(got.Files, []string{"c.txt"}) || !got.Removed {
		t.Fatalf("land: %+v", got)
	}
	if got.Base != head {
		t.Errorf("base = %s; want the checkout HEAD %s", got.Base, head)
	}
	if parent := strings.TrimSpace(git(t, repo, "rev-parse", got.Commit+"^")); parent != head {
		t.Errorf("landed parent = %s; want %s", parent, head)
	}
	if a := git(t, repo, "show", got.Commit+":a.txt"); a != "one\ntwo\nthree\nfour\nfive\n" {
		t.Errorf("landed a.txt = %q; want the HEAD version, without the uncommitted edit", a)
	}
	if a := readFile(t, filepath.Join(repo, "a.txt")); a != includeU {
		t.Errorf("checkout a.txt = %q; want the uncommitted edit kept", a)
	}
	if st := strings.TrimSpace(git(t, repo, "status", "--porcelain", "--untracked-files=no")); st != "M a.txt" {
		t.Errorf("status = %q; want only the uncommitted a.txt", st)
	}
}

func TestIncludeLandRefusesOverlappingCheckoutWork(t *testing.T) {
	repo := landRepo(t)
	head := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))
	res := includeBuild(t, repo, UncommittedInclude)
	writeFile(t, filepath.Join(res.Assembly, "c.txt"), "c\n")
	writeFile(t, filepath.Join(res.Assembly, "a.txt"), "ONE\ntwo\nthree\nfour\nFIVE\n")
	landCommit(t, res.Assembly, "orchestra integrate pc")

	got, err := Land(repo, res.Task, "feat: add c")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "dirty" || !slices.Equal(got.Files, []string{"a.txt"}) || got.Reason != includeDirtyReason {
		t.Fatalf("land: %+v; want dirty on a.txt", got)
	}
	if now := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD")); now != head {
		t.Errorf("checkout HEAD = %s; want %s unchanged", now, head)
	}
	if a := readFile(t, filepath.Join(repo, "a.txt")); a != includeU {
		t.Errorf("checkout a.txt = %q; want it unchanged", a)
	}
}

func TestIncludeLandAfterUserCommitsUncommittedWork(t *testing.T) {
	repo := landRepo(t)
	res := includeBuild(t, repo, UncommittedInclude)
	writeFile(t, filepath.Join(res.Assembly, "c.txt"), "c\n")
	landCommit(t, res.Assembly, "orchestra integrate pc")
	git(t, repo, "commit", "-qam", "wip")
	moved := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))
	writeFile(t, filepath.Join(res.Assembly, "a.txt"), "ONE\ntwo\nthree\nfour\nFIVE\n")
	landCommit(t, res.Assembly, "orchestra integrate pe")

	got, err := Land(repo, res.Task, "feat: add c and fifth line")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "landed" || !slices.Equal(got.Files, []string{"a.txt", "c.txt"}) {
		t.Fatalf("land: %+v; want landed on a.txt and c.txt", got)
	}
	if got.Base != moved {
		t.Errorf("base = %s; want the user's commit %s", got.Base, moved)
	}
	if a := git(t, repo, "show", "HEAD:a.txt"); a != "ONE\ntwo\nthree\nfour\nFIVE\n" {
		t.Errorf("landed a.txt = %q; want the user's line 1 and the assembly's line 5", a)
	}
}

func TestIgnoredUncommittedStillBlocksLand(t *testing.T) {
	repo := landRepo(t)
	res := includeBuild(t, repo, UncommittedIgnore)
	if readMarker(res.Assembly, includedFile) != "" {
		t.Fatal("ignore mode wrote the included marker")
	}
	writeFile(t, filepath.Join(res.Assembly, "c.txt"), "c\n")
	landCommit(t, res.Assembly, "orchestra integrate pc")

	got, err := Land(repo, res.Task, "feat: add c")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "dirty" || !slices.Equal(got.Files, []string{"a.txt"}) ||
		got.Reason != "the checkout has uncommitted changes to tracked files; commit or set them aside first" {
		t.Fatalf("land: %+v; want the dirty refusal on a.txt", got)
	}
}
