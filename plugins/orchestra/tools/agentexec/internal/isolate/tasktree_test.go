package isolate

import (
	"path/filepath"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

func TestTaskTreeRows(t *testing.T) {
	repo := newRepo(t)
	task := create(t, repo, "t1", "")
	top := paths.RealPath(task)

	t.Run("task worktree", func(t *testing.T) {
		got, ok := TaskTree(task)
		if !ok || paths.RealPath(got) != top {
			t.Fatalf("TaskTree(%s) = %q, %v", task, got, ok)
		}
	})

	t.Run("subdirectory and not-yet-existing path", func(t *testing.T) {
		sub := filepath.Join(task, "pkg", "missing")
		for _, dir := range []string{filepath.Join(task, "README.md"), sub} {
			got, ok := TaskTree(dir)
			if !ok || paths.RealPath(got) != top {
				t.Fatalf("TaskTree(%s) = %q, %v", dir, got, ok)
			}
		}
	})

	t.Run("adopted worktree", func(t *testing.T) {
		plain := filepath.Join(t.TempDir(), "adopted")
		git(t, repo, "worktree", "add", "-q", "-b", "adopted", plain)
		if _, ok := TaskTree(plain); ok {
			t.Fatal("a tree with no marker is a task tree")
		}
		if err := WriteAdopted(plain, "t2"); err != nil {
			t.Fatal(err)
		}
		got, ok := TaskTree(plain)
		if !ok || paths.RealPath(got) != paths.RealPath(plain) {
			t.Fatalf("adopted TaskTree = %q, %v", got, ok)
		}
	})

	t.Run("integration role", func(t *testing.T) {
		if err := WriteRole(task, RoleIntegration); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = WriteRole(task, "") })
		if got, ok := TaskTree(task); ok {
			t.Fatalf("integration tree is a task tree: %q", got)
		}
	})

	t.Run("assembly role", func(t *testing.T) {
		if err := WriteRole(task, RoleAssembly); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = WriteRole(task, "") })
		if got, ok := TaskTree(task); ok {
			t.Fatalf("assembly is a task tree: %q", got)
		}
	})

	t.Run("plain worktree without marker", func(t *testing.T) {
		plain := filepath.Join(t.TempDir(), "plain")
		git(t, repo, "worktree", "add", "-q", "-b", "plain", plain)
		if got, ok := TaskTree(plain); ok {
			t.Fatalf("unmarked worktree is a task tree: %q", got)
		}
	})

	t.Run("main checkout with stray marker", func(t *testing.T) {
		writeFile(t, filepath.Join(repo, ".git", "orchestra-baseline"), "abc\n")
		t.Cleanup(func() { _ = removeMarkers(repo, baselineFile) })
		if got, ok := TaskTree(repo); ok {
			t.Fatalf("main checkout is a task tree: %q", got)
		}
	})

	t.Run("outside any repository", func(t *testing.T) {
		if got, ok := TaskTree(t.TempDir()); ok {
			t.Fatalf("directory outside a repository is a task tree: %q", got)
		}
	})
}
