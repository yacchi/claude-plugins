package dispatch

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
