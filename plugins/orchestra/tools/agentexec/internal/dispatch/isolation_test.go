package dispatch

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// trackedDispatchRepo is a throwaway repository with one commit, under an
// isolated HOME so the worktrees it creates land in the test's temp dir.
func trackedDispatchRepo(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("ORCHESTRA_ALIVE_DIR", t.TempDir())
	mustWrite(t, filepath.Join(home, ".gitconfig"),
		"[user]\n\tname = T\n\temail = t@example.com\n")
	repo := filepath.Join(t.TempDir(), "repo")
	mustWrite(t, filepath.Join(repo, "README.md"), "hello\n")
	mustWrite(t, filepath.Join(repo, ".gitignore"), "node_modules/\n")
	trackedDispatchGit(t, repo, "init", "-q", "-b", "main")
	trackedDispatchGit(t, repo, "add", "-A")
	trackedDispatchGit(t, repo, "commit", "-q", "-m", "init")
	return repo
}

// trackedDispatchGit runs git in dir and fails the test on error.
func trackedDispatchGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// trackedDispatchWorktrees counts the worktrees git lists for repo.
func trackedDispatchWorktrees(t *testing.T, repo string) int {
	t.Helper()
	out := trackedDispatchGit(t, repo, "worktree", "list", "--porcelain")
	return strings.Count(out, "worktree ")
}

func TestTrackedDispatchRefusesStartCommitWithTrackedStore(t *testing.T) {
	repo := trackedDispatchRepo(t)
	mustWrite(t, filepath.Join(repo, "node_modules", "x.js"), "x\n")
	trackedDispatchGit(t, repo, "add", "-f", "node_modules/x.js")
	trackedDispatchGit(t, repo, "commit", "-q", "-m", "track store")
	prompt := filepath.Join(t.TempDir(), "prompt.md")
	mustWrite(t, prompt, "do it\n")
	t.Setenv(DryRunEnv, "1")

	out := Execute(context.Background(), testConfig(t), Request{
		Executor: "pi", Isolate: IsolateAlways, Task: "t1", Workdir: repo,
		PromptFiles: []string{prompt},
	})

	if out.Exit != 1 {
		t.Fatalf("exit %d, want 1 (diag %q)", out.Exit, out.Diag)
	}
	for _, want := range []string{"dispatch: HEAD tracks 1 files", "node_modules/", "isolation.keep_paths"} {
		if !strings.Contains(out.Diag, want) {
			t.Errorf("diag %q lacks %q", out.Diag, want)
		}
	}
	if n := trackedDispatchWorktrees(t, repo); n != 1 {
		t.Fatalf("worktrees %d, want only the repository itself", n)
	}
}

func TestTrackedDispatchIsolatesWhenStoreIsUntracked(t *testing.T) {
	repo := trackedDispatchRepo(t)
	mustWrite(t, filepath.Join(repo, "node_modules", "x.js"), "x\n")
	prompt := filepath.Join(t.TempDir(), "prompt.md")
	mustWrite(t, prompt, "do it\n")
	t.Setenv(DryRunEnv, "1")

	out := Execute(context.Background(), testConfig(t), Request{
		Executor: "pi", Isolate: IsolateAlways, Task: "t1", Workdir: repo,
		PromptFiles: []string{prompt},
	})

	if out.Exit != 0 || out.Diag != "" {
		t.Fatalf("exit %d diag %q, want a clean dispatch", out.Exit, out.Diag)
	}
	if n := trackedDispatchWorktrees(t, repo); n != 2 {
		t.Fatalf("worktrees %d, want the repository plus one task tree", n)
	}
}

// parentDispatchSetup is a tracked repository with an integration tree cut
// from it; the tree holds a sub directory for the subdirectory workdir case.
func parentDispatchSetup(t *testing.T) (repo, integration string) {
	t.Helper()
	repo = trackedDispatchRepo(t)
	mustWrite(t, filepath.Join(repo, "sub", "a.txt"), "a\n")
	trackedDispatchGit(t, repo, "add", "-A")
	trackedDispatchGit(t, repo, "commit", "-q", "-m", "add sub")
	created, err := isolate.Create(repo, "int", isolate.CreateOptions{
		Backend: isolate.BackendGit, Role: isolate.RoleIntegration,
	})
	if err != nil || created.Status != "created" {
		t.Fatalf("integration tree: %+v, %v", created, err)
	}
	return repo, created.Path
}

func TestParentDispatchRecordsIntegrationTreeAsParent(t *testing.T) {
	repo, integration := parentDispatchSetup(t)
	prompt := filepath.Join(t.TempDir(), "prompt.md")
	mustWrite(t, prompt, "do it\n")
	t.Setenv(DryRunEnv, "1")

	cases := []struct {
		name, task, workdir, wantParent string
	}{
		{"user checkout has no parent", "t-user", repo, ""},
		{"integration root is the parent", "t-int", integration, paths.RealPath(integration)},
		{"integration subdirectory uses its tree root", "t-sub", filepath.Join(integration, "sub"), paths.RealPath(integration)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := Execute(context.Background(), testConfig(t), Request{
				Executor: "pi", Isolate: IsolateAlways, Task: c.task, Workdir: c.workdir,
				PromptFiles: []string{prompt},
			})
			if out.Exit != 0 || out.Diag != "" {
				t.Fatalf("exit %d diag %q, want a clean dispatch", out.Exit, out.Diag)
			}
			path, _, ok := isolate.ResolveWorktree(repo, c.task, "")
			if !ok {
				t.Fatalf("no task tree for %s", c.task)
			}
			if got := isolate.ReadParent(path); got != c.wantParent {
				t.Errorf("parent %q, want %q", got, c.wantParent)
			}
		})
	}
}

func TestParentDispatchRefusesTreeUnderAnotherIntegrationTree(t *testing.T) {
	repo, integration := parentDispatchSetup(t)
	t.Setenv("ORCHESTRA_REQUEST_ROOT", t.TempDir())
	other, err := isolate.Create(repo, "int2", isolate.CreateOptions{
		Backend: isolate.BackendGit, Role: isolate.RoleIntegration,
	})
	if err != nil || other.Status != "created" {
		t.Fatalf("second integration tree: %+v, %v", other, err)
	}
	prompt := filepath.Join(t.TempDir(), "prompt.md")
	mustWrite(t, prompt, "do it\n")
	t.Setenv(DryRunEnv, "1")
	dispatch := func(workdir string) Outcome {
		return Execute(context.Background(), testConfig(t), Request{
			Executor: "pi", Isolate: IsolateAlways, Task: "t-pkg", Workdir: workdir,
			PromptFiles: []string{prompt},
		})
	}

	if out := dispatch(integration); out.Exit != 0 || out.Diag != "" {
		t.Fatalf("first dispatch: exit %d diag %q", out.Exit, out.Diag)
	}
	path, _, ok := isolate.ResolveWorktree(repo, "t-pkg", "")
	if !ok {
		t.Fatal("no task tree after the first dispatch")
	}
	trees := trackedDispatchWorktrees(t, repo)

	// Same parent: the tree is reused.
	if out := dispatch(integration); out.Exit != 0 || out.Diag != "" {
		t.Fatalf("same-parent re-dispatch: exit %d diag %q", out.Exit, out.Diag)
	}

	out := dispatch(other.Path)
	if out.Exit != 1 {
		t.Fatalf("exit %d, want 1 (diag %q)", out.Exit, out.Diag)
	}
	for _, want := range []string{"task t-pkg already has a tree at", "not under " + paths.RealPath(other.Path), "isolate remove --task t-pkg"} {
		if !strings.Contains(out.Diag, want) {
			t.Errorf("diag %q lacks %q", out.Diag, want)
		}
	}
	if got := isolate.ReadParent(path); got != paths.RealPath(integration) {
		t.Errorf("existing tree's parent %q, want %q", got, paths.RealPath(integration))
	}
	if n := trackedDispatchWorktrees(t, repo); n != trees {
		t.Errorf("worktrees %d, want %d (no second tree)", n, trees)
	}
}
