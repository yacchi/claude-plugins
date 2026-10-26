package isolate

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
)

var testFilter = ChangeFilter{Exclude: []string{".pnpm-store/", "node_modules/", "packages/a/.cache/"}}

func TestChangeFilterExcluded(t *testing.T) {
	for path, want := range map[string]bool{
		".pnpm-store/v11/files/ab/x": true,
		"pkg/node_modules/a/b.js":    true,
		"node_modules":               true,
		".pnpm-store":                true,
		"pkg/node_modules":           true,
		"packages/a/.cache":          true,
		"packages/a/.cache/x":        true,
		"src/node_modules_util.ts":   false,
		"other/packages/a/.cache/x":  false,
		"packages/a":                 false,
	} {
		if got := testFilter.Excluded(path); got != want {
			t.Errorf("Excluded(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestChangeFilterPathspec(t *testing.T) {
	want := []string{"--", ".",
		":(exclude,glob)**/.pnpm-store", ":(exclude,glob)**/.pnpm-store/**",
		":(exclude,glob)**/node_modules", ":(exclude,glob)**/node_modules/**",
		":(exclude,glob)packages/a/.cache", ":(exclude,glob)packages/a/.cache/**"}
	if got := testFilter.Pathspec(); !slices.Equal(got, want) {
		t.Errorf("Pathspec = %q", got)
	}
	if got := (ChangeFilter{}).Pathspec(); !slices.Equal(got, []string{"--", "."}) {
		t.Errorf("empty Pathspec = %q", got)
	}
	if got := (ChangeFilter{}).ExcludedPathspec(); got != nil {
		t.Errorf("empty ExcludedPathspec = %q", got)
	}
}

// Real git: the glob crosses directories and matches at the root as well
// as deeper, and nothing excluded reaches the files, the count or the patch.
func TestDiffLeavesExcludedDirectoriesOut(t *testing.T) {
	repo := newRepo(t)
	path := create(t, repo, "t1", "")
	writeFile(t, filepath.Join(path, "src/a.ts"), "a\n")
	writeFile(t, filepath.Join(path, ".pnpm-store/v11/files/1"), "1\n")
	writeFile(t, filepath.Join(path, ".pnpm-store/v11/files/2"), "2\n")
	writeFile(t, filepath.Join(path, "pkg/deep/__pycache__/x/y.pyc"), "y\n")
	f := ChangeFilter{Exclude: []string{".pnpm-store/", "__pycache__/"}}
	res, err := DiffWith(repo, "t1", "", true, f)
	if err != nil || res.Status != "ok" {
		t.Fatalf("diff: %+v %v", res, err)
	}
	if !slices.Equal(res.Files, []string{"src/a.ts"}) || res.Added != 1 {
		t.Errorf("files %v added %d", res.Files, res.Added)
	}
	if res.Excluded != 3 || !slices.Equal(res.ExcludedDirs, []string{".pnpm-store/", "__pycache__/"}) {
		t.Errorf("excluded %d dirs %v", res.Excluded, res.ExcludedDirs)
	}
	if strings.Contains(res.Patch, ".pnpm-store") || strings.Contains(res.Patch, "__pycache__") {
		t.Errorf("patch mentions an excluded dir:\n%s", res.Patch)
	}
}

// A top-level symlink (node_modules -> elsewhere) and a file named like an
// entry are excluded too, not only the files below a directory.
func TestDiffLeavesExcludedSymlinkAndFileOut(t *testing.T) {
	repo := newRepo(t)
	path := create(t, repo, "t1", "")
	writeFile(t, filepath.Join(path, "src/a.ts"), "a\n")
	writeFile(t, filepath.Join(path, ".pnpm-store"), "x\n")
	if err := os.Symlink(t.TempDir(), filepath.Join(path, "node_modules")); err != nil {
		t.Fatal(err)
	}
	f := ChangeFilter{Exclude: []string{".pnpm-store/", "node_modules/"}}
	res, err := DiffWith(repo, "t1", "", true, f)
	if err != nil || res.Status != "ok" {
		t.Fatalf("diff: %+v %v", res, err)
	}
	if !slices.Equal(res.Files, []string{"src/a.ts"}) {
		t.Errorf("files %v", res.Files)
	}
	if !slices.Equal(res.ExcludedDirs, []string{".pnpm-store/", "node_modules/"}) {
		t.Errorf("excluded dirs %v", res.ExcludedDirs)
	}
	if _, err := gitx.Run(path, append([]string{"add", "-A"}, f.Pathspec()...)...); err != nil {
		t.Fatal(err)
	}
	staged, err := gitx.Run(path, "diff", "--cached", "--name-only")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(staged) != "src/a.ts" {
		t.Errorf("staged %q", staged)
	}
}

// A tree whose only leftovers are excluded dirs is clean, or sweep would
// keep it forever.
func TestUncommittedIgnoresExcludedLeftovers(t *testing.T) {
	repo := newRepo(t)
	path := create(t, repo, "t1", "")
	writeFile(t, filepath.Join(path, ".pnpm-store/v11/files/1"), "1\n")
	files, err := uncommittedWith(path, ChangeFilter{Exclude: []string{".pnpm-store/"}})
	if err != nil || len(files) != 0 {
		t.Errorf("uncommitted = %v, %v", files, err)
	}
}
