package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// checkout is a repo with a tracked .claude/orchestra.yaml and an untracked
// .claude/orchestra.local.yaml, plus a linked worktree outside it.
func checkout(t *testing.T) (repo, linked string) {
	t.Helper()
	t.Setenv("ORCHESTRA_CONFIG_ROOT", "")
	tmp, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("HOME", filepath.Join(tmp, "home"))
	repo = filepath.Join(tmp, "repo")
	write(t, filepath.Join(repo, ".claude", "orchestra.yaml"), "tiers: {light: tracked}\n")
	write(t, filepath.Join(repo, "sub", "keep.txt"), "x\n")
	git(t, repo, "init", "-q", "-b", "main", ".")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "init")
	// per-machine, never committed
	write(t, filepath.Join(repo, ".claude", "orchestra.local.yaml"), "orca: {enabled: false}\n")
	linked = filepath.Join(tmp, "state", "requests", "r1", ".integration", "repo", "prog")
	git(t, repo, "worktree", "add", "-q", "-b", "prog", linked)
	return repo, linked
}

func markWorktree(t *testing.T, repo, linked, marker string) {
	t.Helper()
	out, err := exec.Command("git", "-C", linked, "rev-parse", "--absolute-git-dir").Output()
	if err != nil {
		t.Fatal(err)
	}
	dir := string(out[:len(out)-1])
	write(t, filepath.Join(dir, marker), "x\n")
}

// An integration or package worktree has no .claude/orchestra.local.yaml of
// its own: the project layers are the user's checkout's.
func TestProjectBaseOfAnOrchestraWorktreeIsTheOriginCheckout(t *testing.T) {
	repo, linked := checkout(t)
	for _, marker := range ManagedWorktreeMarkers {
		t.Run(marker, func(t *testing.T) {
			markWorktree(t, repo, linked, marker)
			t.Cleanup(func() { _ = os.Remove(filepath.Join(gitDirOf(t, linked), marker)) })
			want := paths.RealPath(repo)
			if got := paths.RealPath(ProjectBaseAt(linked)); got != want {
				t.Errorf("worktree root: base %s, want %s", got, want)
			}
			// a subdirectory maps to the same place of the checkout
			if got := paths.RealPath(ProjectBaseAt(filepath.Join(linked, "sub"))); got != want {
				t.Errorf("worktree subdir: base %s, want %s", got, want)
			}
		})
	}
}

func gitDirOf(t *testing.T, wt string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", wt, "rev-parse", "--absolute-git-dir").Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out[:len(out)-1])
}

func TestResolveInAnOrchestraWorktreeReadsTheCheckoutsLocalLayer(t *testing.T) {
	repo, linked := checkout(t)
	markWorktree(t, repo, linked, "orchestra-baseline")
	t.Chdir(linked)
	cfg, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if paths.RealPath(cfg.Root) != paths.RealPath(repo) {
		t.Errorf("root %s, want %s", cfg.Root, repo)
	}
	scopes := map[string]string{}
	for _, l := range cfg.Layers {
		scopes[l.Scope] = paths.RealPath(l.Path)
	}
	if scopes["local"] != paths.RealPath(filepath.Join(repo, ".claude", "orchestra.local.yaml")) ||
		scopes["project"] != paths.RealPath(filepath.Join(repo, ".claude", "orchestra.yaml")) {
		t.Errorf("layers %v", cfg.Layers)
	}
	if orca := ObjectAt(cfg.Values, "orca"); orca == nil || orca["enabled"] != false {
		t.Errorf("orca.enabled from the local layer is lost: %v", orca)
	}
}

// A worktree the user made has layers of its own; the walk stays in it.
func TestProjectBaseOfTheUsersOwnWorktreeStaysThere(t *testing.T) {
	_, linked := checkout(t)
	write(t, filepath.Join(linked, ".claude", "orchestra.local.yaml"), "tiers: {light: own}\n")
	if got, want := paths.RealPath(ProjectBaseAt(linked)), paths.RealPath(linked); got != want {
		t.Errorf("base %s, want %s", got, want)
	}
}

func TestExplicitConfigRootStillWinsInAnOrchestraWorktree(t *testing.T) {
	repo, linked := checkout(t)
	markWorktree(t, repo, linked, "orchestra-baseline")
	other := filepath.Join(t.TempDir(), "other")
	_ = os.MkdirAll(other, 0o755)
	t.Setenv("ORCHESTRA_CONFIG_ROOT", other)
	if got := ProjectBaseAt(linked); got != other {
		t.Errorf("base %s, want %s", got, other)
	}
}
