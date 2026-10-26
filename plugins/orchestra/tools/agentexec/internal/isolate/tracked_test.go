package isolate

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// trackedCommit writes each file (repo-relative path -> content), force-adds
// it so a directory the .gitignore covers is tracked too, and commits.
func trackedCommit(t *testing.T, repo string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		writeFile(t, filepath.Join(repo, p), content)
		git(t, repo, "add", "-f", "--", p)
	}
	git(t, repo, "commit", "-q", "-m", "tracked")
}

func TestTrackedExcludedIgnoresDirectoriesOnlyOnDisk(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, filepath.Join(repo, "node_modules/only-on-disk/i.js"), "x")
	trackedCommit(t, repo, map[string]string{"README.md": "changed\n"})
	f := ChangeFilter{Exclude: []string{"node_modules/"}}

	got, err := TrackedExcluded(repo, "HEAD", f)
	if err != nil || got != nil {
		t.Fatalf("on-disk only: got %+v, %v; want nil", got, err)
	}

	trackedCommit(t, repo, map[string]string{"node_modules/forced/i.js": "x"})
	got, err = TrackedExcluded(repo, "HEAD", f)
	want := []TrackedDir{{Dir: "node_modules/", Files: 1, Bytes: 1}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("force-added: got %+v, %v; want %+v", got, err, want)
	}
}

func TestTrackedExcludedAggregatesOutermostAndSortsByBytes(t *testing.T) {
	repo := newRepo(t)
	trackedCommit(t, repo, map[string]string{
		".pnpm-store/v11/files/ab/x":         "12345",
		".pnpm-store/y":                      "123",
		"packages/web/node_modules/dep/i.js": strings.Repeat("x", 100),
		"node_modules/a/i.js":                "12",
		"node_modules/a/node_modules/b/i.js": "1234567",
		"packages/a/.cache/x":                "1",
		"x/packages/a/.cache/y":              "1234",
		"src/node_modules":                   "a file, not a directory",
		"src/node_modules_util.ts":           "1",
	})
	f := ChangeFilter{Exclude: []string{".pnpm-store/", "node_modules/", "packages/a/.cache/"}}

	got, err := TrackedExcluded(repo, "HEAD", f)
	want := []TrackedDir{
		{Dir: "packages/web/node_modules/", Files: 1, Bytes: 100},
		{Dir: "node_modules/", Files: 2, Bytes: 9},
		{Dir: ".pnpm-store/", Files: 2, Bytes: 8},
		{Dir: "packages/a/.cache/", Files: 1, Bytes: 1},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, %v\nwant %+v", got, err, want)
	}
}

func TestTrackedExcludedEdgeCases(t *testing.T) {
	repo := newRepo(t)

	got, err := TrackedExcluded(repo, "HEAD", ChangeFilter{})
	if err != nil || got != nil {
		t.Fatalf("empty filter: got %+v, %v; want nil, nil", got, err)
	}

	f := ChangeFilter{Exclude: []string{"node_modules/"}}
	if _, err := TrackedExcluded(repo, "no-such-rev", f); err == nil {
		t.Fatal("bad rev: want an error")
	}
}

func TestTrackedRefusalNamesDirectoriesWithFixes(t *testing.T) {
	got := TrackedRefusal("3ecfe04e1f2a", []TrackedDir{{Dir: ".pnpm-store/", Files: 1, Bytes: 1 << 20}})
	want := "3ecfe04e tracks 1 files (1.0 MiB) under dependency or cache directories that isolation.exclude_paths keeps out of every change: .pnpm-store/ (1 files, 1.0 MiB). Every worktree orchestra creates would check them out. Take them out of the branch (git rm -r --cached .pnpm-store/, add it to .gitignore, commit; the same for each directory), or list a directory in isolation.keep_paths if the repository tracks it on purpose."
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestTrackedRefusalTruncatesAfterFiveDirectories(t *testing.T) {
	var dirs []TrackedDir
	for i := 1; i <= 7; i++ {
		dirs = append(dirs, TrackedDir{Dir: fmt.Sprintf("d%d/", i), Files: 1, Bytes: 1 << 20})
	}
	got := TrackedRefusal("3ecfe04e", dirs)
	if !strings.Contains(got, "d5/ (1 files, 1.0 MiB), and 2 more. Every worktree") {
		t.Fatalf("missing truncation tail: %s", got)
	}
	if strings.Contains(got, "d6/") {
		t.Fatalf("sixth directory must not be named: %s", got)
	}
	if !strings.Contains(got, "tracks 7 files (7.0 MiB)") {
		t.Fatalf("totals must cover every directory: %s", got)
	}
}
