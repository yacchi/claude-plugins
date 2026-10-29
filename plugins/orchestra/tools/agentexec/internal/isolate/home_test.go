package isolate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

func TestRepoKeyNames(t *testing.T) {
	cases := map[string]string{
		"/Users/u/src/app/.git":     "app-6c0c14cb",
		"/Users/u/other/app/.git":   "app-069dd895",
		"/srv/git/app.git":          "app-9367be9b",
		"/x/super/.git/modules/lib": "lib-0dfc3aea",
		"/x/My Repo!/.git":          "My-Repo-1d9ac2f8",
		"/.git":                     "repo-b8b6a5d3",
	}
	for common, want := range cases {
		if got := RepoKey(common); got != want {
			t.Errorf("RepoKey(%q) = %q; want %q", common, got, want)
		}
	}
}

func TestHomeIsKeyedByRepositoryNotCheckout(t *testing.T) {
	repo := newRepo(t)
	side := filepath.Join(filepath.Dir(repo), "side")
	git(t, repo, "worktree", "add", "-q", "-b", "side", side)

	if Home(side) != Home(repo) {
		t.Errorf("Home(linked) = %q; want Home(checkout) = %q", Home(side), Home(repo))
	}
	if !strings.HasPrefix(Home(repo), filepath.Join(paths.StateDir(), "worktrees")+string(os.PathSeparator)) {
		t.Errorf("Home(repo) = %q; want it under the state dir", Home(repo))
	}
	if strings.HasPrefix(Home(repo), filepath.Dir(repo)+string(os.PathSeparator)) {
		t.Errorf("Home(repo) = %q; want it not beside the checkout", Home(repo))
	}
}

func TestCreateDefaultsUnderHome(t *testing.T) {
	repo := newRepo(t)
	res, err := Create(repo, "t1", CreateOptions{Backend: BackendGit})
	if err != nil || res.Status != "created" {
		t.Fatalf("%+v %v", res, err)
	}
	if want := filepath.Join(Home(repo), "t1"); res.Path != want {
		t.Errorf("path = %q; want %q", res.Path, want)
	}
}

func TestCreateChildOfRequestIntegrationGoesToWorkspace(t *testing.T) {
	repo := newRepo(t)
	requestRootClock(t)
	res := mustEnsureIntegration(t, repo)

	child, err := Create(repo, "t1", CreateOptions{Backend: BackendGit, Parent: res.Integration, Onto: res.Head})
	if err != nil || child.Status != "created" {
		t.Fatalf("%+v %v", child, err)
	}
	if want := filepath.Join(integWorkspace(t), ".work", "repo", "t1"); child.Path != want {
		t.Errorf("path = %q; want %q", child.Path, want)
	}
	if got := ReadParent(child.Path); got != res.Integration {
		t.Errorf("parent = %q; want %q", got, res.Integration)
	}
}

func TestCreateFallsBackToHomeWithoutRequest(t *testing.T) {
	repo := newRepo(t)
	requestRootClock(t)
	res := mustEnsureIntegration(t, repo)
	if err := os.Remove(filepath.Join(integWorkspace(t), requestFile)); err != nil {
		t.Fatal(err)
	}

	child, err := Create(repo, "t2", CreateOptions{Backend: BackendGit, Parent: res.Integration, Onto: res.Head})
	if err != nil || child.Status != "created" {
		t.Fatalf("%+v %v", child, err)
	}
	if want := filepath.Join(Home(repo), "t2"); child.Path != want {
		t.Errorf("path = %q; want %q", child.Path, want)
	}
}

func TestCreateFallsBackToHomeForParentlessIntegration(t *testing.T) {
	repo := newRepo(t)
	head := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))
	parent, err := Create(repo, "old", CreateOptions{Backend: BackendGit, Role: RoleIntegration, Onto: head})
	if err != nil || parent.Status != "created" {
		t.Fatalf("%+v %v", parent, err)
	}

	child, err := Create(repo, "t3", CreateOptions{Backend: BackendGit, Parent: parent.Path, Onto: head})
	if err != nil || child.Status != "created" {
		t.Fatalf("%+v %v", child, err)
	}
	if want := filepath.Join(Home(repo), "t3"); child.Path != want {
		t.Errorf("path = %q; want %q", child.Path, want)
	}
}

func TestWorktreeBasesCoverHomeAndRequestRoot(t *testing.T) {
	repo := newRepo(t)
	want := []string{Home(repo), paths.RealPath(os.Getenv(RequestRootEnv))}
	if got := WorktreeBases(repo); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("WorktreeBases() = %q; want %q", got, want)
	}

	t.Setenv(RequestRootEnv, "relative")
	if got := WorktreeBases(repo); len(got) != 1 || got[0] != Home(repo) {
		t.Errorf("WorktreeBases() with a relative request root = %q; want only Home", got)
	}
}

// mustEnsureIntegration makes repo's assembly and an integration tree for
// feat, failing the test on anything but a ready result.
func mustEnsureIntegration(t *testing.T, repo string) EnsureResult {
	t.Helper()
	res, err := EnsureIntegration(repo, "feat", "")
	if err != nil || res.Status != "ready" {
		t.Fatalf("EnsureIntegration() = %+v, %v; want ready", res, err)
	}
	return res
}
