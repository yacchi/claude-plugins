package isolate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	sessA = "aaaaaaaa-0000-4000-8000-000000000001"
	sessB = "bbbbbbbb-0000-4000-8000-000000000002"
)

// newRepo is a throwaway repository (realpath'd, as git reports it) with a
// README and a .gitignore covering the usual dependency dirs, under a
// hermetic git config and no ambient session. Heartbeats go to a temp dir.
func newRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "T")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "T")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	alive := filepath.Join(tmp, "alive")
	t.Setenv("ORCHESTRA_ALIVE_DIR", alive)
	SetAliveDir(alive)
	t.Cleanup(func() { SetAliveDir("") })
	t.Setenv(RequestRootEnv, filepath.Join(tmp, "requests"))
	repo := filepath.Join(tmp, "repo")
	writeFile(t, filepath.Join(repo, "README.md"), "hello\n")
	writeFile(t, filepath.Join(repo, ".gitignore"), "node_modules/\n.venv/\n.env\n")
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "init")
	return repo
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// create plants a worktree for task as session ("" = no session).
func create(t *testing.T, repo, task, session string) string {
	t.Helper()
	t.Setenv("CLAUDE_CODE_SESSION_ID", session)
	res, err := Create(repo, task, CreateOptions{Backend: BackendGit, Carry: true})
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	if err != nil || res.Status != "created" {
		t.Fatalf("create %s: %+v, %v", task, res, err)
	}
	return res.Path
}

func remove(t *testing.T, repo, task string, force bool, session string) RemoveResult {
	t.Helper()
	res, err := Remove(repo, task, force, session)
	if err != nil {
		t.Fatalf("remove %s: %v", task, err)
	}
	return res
}

func branches(t *testing.T, repo string) string {
	return strings.TrimSpace(git(t, repo, "branch", "--format=%(refname:short)"))
}
