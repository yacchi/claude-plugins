package gitx

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestConfigEnvNumbersPairsAndExtendsAnExistingCount(t *testing.T) {
	base := []string{"PATH=/bin", "GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=a.b", "GIT_CONFIG_VALUE_0=1"}
	orig := slices.Clone(base)
	env := ConfigEnv([][2]string{{"gtr.copy.include", ".env"}, {"gtr.copy.include", "*.local"}}, base)
	if !slices.Equal(base, orig) {
		t.Fatalf("base was mutated: %v", base)
	}
	want := []string{
		"GIT_CONFIG_KEY_2=gtr.copy.include", "GIT_CONFIG_VALUE_2=.env",
		"GIT_CONFIG_KEY_3=gtr.copy.include", "GIT_CONFIG_VALUE_3=*.local",
		"GIT_CONFIG_COUNT=4", "GIT_CONFIG_KEY_0=a.b", "PATH=/bin",
	}
	for _, kv := range want {
		if !slices.Contains(env, kv) {
			t.Errorf("missing %s in %v", kv, env)
		}
	}
	if n := countKey(env, "GIT_CONFIG_COUNT"); n != 1 {
		t.Errorf("GIT_CONFIG_COUNT appears %d times", n)
	}
}

func TestConfigEnvWithoutPairsInjectsNothing(t *testing.T) {
	env := ConfigEnv(nil, []string{"A=1"})
	if !slices.Equal(env, []string{"A=1"}) {
		t.Fatalf("got %v", env)
	}
}

func TestConfigEnvIgnoresABadExistingCount(t *testing.T) {
	env := ConfigEnv([][2]string{{"k.v", "x"}}, []string{"GIT_CONFIG_COUNT=-3"})
	if !slices.Contains(env, "GIT_CONFIG_KEY_0=k.v") || !slices.Contains(env, "GIT_CONFIG_COUNT=1") {
		t.Fatalf("got %v", env)
	}
}

func countKey(env []string, key string) int {
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			n++
		}
	}
	return n
}

func newRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "T")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "T")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "init", "-q", "-b", "main")
	write(t, filepath.Join(dir, "a.txt"), "a\n")
	write(t, filepath.Join(dir, ".gitignore"), "node_modules/\n")
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestConfigEnvIsVisibleToGitButWritesNoFile(t *testing.T) {
	repo := newRepo(t)
	env := ConfigEnv([][2]string{{"orchestra.probe", "yes"}}, nil)
	out, err := RunEnv(repo, env, "config", "--get", "orchestra.probe")
	if err != nil || strings.TrimSpace(out) != "yes" {
		t.Fatalf("got %q, %v", out, err)
	}
	cfg, _ := os.ReadFile(filepath.Join(repo, ".git", "config"))
	if strings.Contains(string(cfg), "probe") {
		t.Fatal(".git/config was written")
	}
}

func TestRunReportsTheExitCodeAndStderr(t *testing.T) {
	repo := newRepo(t)
	_, err := Run(repo, "rev-parse", "--verify", "no-such-ref")
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code == 0 || ee.Stderr == "" {
		t.Fatalf("got %#v", err)
	}
}

func TestIsDirtyCountsUntrackedButNotIgnored(t *testing.T) {
	repo := newRepo(t)
	if IsDirty(repo) {
		t.Fatal("clean tree reported dirty")
	}
	write(t, filepath.Join(repo, "node_modules", "x.js"), "x")
	if IsDirty(repo) {
		t.Fatal("an ignored file alone made the tree dirty")
	}
	write(t, filepath.Join(repo, "new.txt"), "n")
	if !IsDirty(repo) {
		t.Fatal("untracked file not reported")
	}
	if IsDirty(t.TempDir()) {
		t.Fatal("a non-repository is dirty")
	}
}

func TestWorktreeListAndGitDir(t *testing.T) {
	repo := newRepo(t)
	wt := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-wt")
	mustGit(t, repo, "worktree", "add", "-q", "-b", "orchestra/x", wt)
	mustGit(t, repo, "worktree", "add", "-q", "--detach", wt+"2")
	list, err := WorktreeList(repo)
	if err != nil || len(list) != 3 {
		t.Fatalf("got %v, %v", list, err)
	}
	if list[0].Path != repo || list[0].Branch != "main" {
		t.Errorf("main entry %+v", list[0])
	}
	if list[1].Branch != "orchestra/x" || list[1].Head == "" {
		t.Errorf("linked entry %+v", list[1])
	}
	if !list[2].Detached || list[2].Branch != "" {
		t.Errorf("detached entry %+v", list[2])
	}
	dir, err := GitDir(wt)
	if err != nil || !strings.Contains(dir, filepath.Join(".git", "worktrees")) {
		t.Fatalf("GitDir = %q, %v", dir, err)
	}
	if root, ok := RepoRoot(filepath.Join(wt)); !ok || root != wt {
		t.Fatalf("RepoRoot = %q %v", root, ok)
	}
}

func TestApplyPatch3WayLeavesConflictMarkers(t *testing.T) {
	repo := newRepo(t)
	write(t, filepath.Join(repo, "a.txt"), "theirs\n")
	patch, err := Run(repo, "diff", "--binary")
	if err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "checkout", "--", "a.txt")
	write(t, filepath.Join(repo, "a.txt"), "ours\n")
	mustGit(t, repo, "commit", "-qam", "ours")
	err = ApplyPatch3Way(repo, []byte(patch))
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("want a conflict exit, got %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(repo, "a.txt"))
	if !strings.Contains(string(b), "<<<<<<<") {
		t.Fatalf("no conflict markers: %q", b)
	}
}
