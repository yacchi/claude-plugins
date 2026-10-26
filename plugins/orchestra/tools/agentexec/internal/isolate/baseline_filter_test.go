package isolate

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A staged store in the user's checkout is neither carried into the new tree
// nor committed into its baseline, while the user's ordinary edit still is.
func TestBaselineSkipsStagedStore(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "edited\n")
	writeFile(t, filepath.Join(repo, ".pnpm-store/v11/files/ab/x"), "x\n")
	git(t, repo, "add", ".pnpm-store")

	path := create(t, repo, "t1", "")
	files := strings.Split(strings.TrimSpace(git(t, path, "show", "--name-only", "--format=", "HEAD")), "\n")
	if !slices.Equal(files, []string{"README.md"}) {
		t.Errorf("baseline files = %q, want [README.md]", files)
	}
	if exists(filepath.Join(path, ".pnpm-store")) {
		t.Errorf(".pnpm-store was carried into the tree")
	}
	if got := readFile(t, filepath.Join(path, "README.md")); got != "edited\n" {
		t.Errorf("README.md = %q, want the user's edit", got)
	}
}

// A store committed on an earlier branch arrives at its HEAD content, not
// the user's unstaged edit to it, and stays out of the baseline.
func TestBaselineSkipsStoreCommittedInHead(t *testing.T) {
	repo := newRepo(t)
	const store = ".pnpm-store/v11/files/cd/y"
	writeFile(t, filepath.Join(repo, store), "head\n")
	git(t, repo, "add", ".pnpm-store")
	git(t, repo, "commit", "-q", "-m", "polluted")
	writeFile(t, filepath.Join(repo, store), "user\n")

	path := create(t, repo, "t2", "")
	if got := readFile(t, filepath.Join(path, store)); got != "head\n" {
		t.Errorf("%s = %q, want the HEAD content", store, got)
	}
	if strings.Contains(git(t, path, "show", "--name-only", "--format=", "HEAD"), ".pnpm-store") {
		t.Errorf("baseline commit lists %s", store)
	}
}
