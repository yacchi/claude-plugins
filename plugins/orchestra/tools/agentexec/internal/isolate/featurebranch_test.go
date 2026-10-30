package isolate

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestFeatureSlug(t *testing.T) {
	cases := []struct {
		names []string
		want  string
	}{
		{[]string{"orchestra-auto-land"}, "orchestra-auto-land"},
		{[]string{"", "Fix Login_Page v2!"}, "fix-login-page-v2"},
		{[]string{"自動land", "wave-int"}, "land"},
		{[]string{"ab", "--", ""}, "run"},
		{[]string{strings.Repeat("abc-", 15)}, "abc-abc-abc-abc-abc-abc-abc-abc-abc-abc"},
	}
	for _, c := range cases {
		if got := FeatureSlug(c.names...); got != c.want {
			t.Errorf("FeatureSlug(%q) = %q, want %q", c.names, got, c.want)
		}
	}
}

func TestIsDefaultBranch(t *testing.T) {
	repo := landRepo(t)
	for branch, want := range map[string]bool{"main": true, "master": true, "topic": false, "trunk": false, "dev": false, "": false} {
		if got := IsDefaultBranch(repo, branch); got != want {
			t.Errorf("IsDefaultBranch(%q) = %v, want %v", branch, got, want)
		}
	}
	git(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/trunk")
	if !IsDefaultBranch(repo, "trunk") {
		t.Error("origin/HEAD target trunk is not the default branch")
	}
	git(t, repo, "config", "init.defaultBranch", "dev")
	if !IsDefaultBranch(repo, "dev") {
		t.Error("init.defaultBranch dev is not the default branch")
	}
}

func featureLand(repo string) func() (LandResult, error) {
	return func() (LandResult, error) { return Land(repo, "prog", "feat: x") }
}

func headBranch(t *testing.T, repo string) string {
	t.Helper()
	return strings.TrimSpace(git(t, repo, "symbolic-ref", "--short", "HEAD"))
}

func featureBranches(t *testing.T, repo string) string {
	t.Helper()
	return strings.TrimSpace(git(t, repo, "for-each-ref", "--format=%(refname:short)", "refs/heads/feature"))
}

func TestOnFeatureBranchFromMain(t *testing.T) {
	repo := landRepo(t)
	landAssembly(t, repo)
	seed := strings.TrimSpace(git(t, repo, "rev-parse", "main"))
	res, created, err := OnFeatureBranch(repo, "auto-land", featureLand(repo))
	if err != nil || res.Status != "landed" || res.Branch != "feature/auto-land" || !created {
		t.Fatalf("got %+v created=%v err=%v", res, created, err)
	}
	if b := headBranch(t, repo); b != "feature/auto-land" {
		t.Fatalf("HEAD is %s", b)
	}
	if m := strings.TrimSpace(git(t, repo, "rev-parse", "main")); m != seed {
		t.Fatalf("main moved: %s -> %s", seed, m)
	}
}

func TestOnFeatureBranchSkipsTakenNames(t *testing.T) {
	repo := landRepo(t)
	landAssembly(t, repo)
	git(t, repo, "branch", "feature/auto-land")
	git(t, repo, "update-ref", "refs/remotes/origin/feature/auto-land-2", "HEAD")
	res, created, err := OnFeatureBranch(repo, "auto-land", featureLand(repo))
	if err != nil || res.Status != "landed" || res.Branch != "feature/auto-land-3" || !created {
		t.Fatalf("got %+v created=%v err=%v", res, created, err)
	}
}

func TestOnFeatureBranchOffDefault(t *testing.T) {
	repo := landRepo(t)
	git(t, repo, "checkout", "-q", "-b", "topic")
	landAssembly(t, repo)
	res, created, err := OnFeatureBranch(repo, "auto-land", featureLand(repo))
	if err != nil || res.Status != "landed" || res.Branch != "topic" || created {
		t.Fatalf("got %+v created=%v err=%v", res, created, err)
	}
	if fb := featureBranches(t, repo); fb != "" {
		t.Fatalf("feature branches created: %s", fb)
	}
}

func TestOnFeatureBranchRollsBack(t *testing.T) {
	repo := landRepo(t)
	landAssembly(t, repo)
	edit := "one\ntwo\nthree\nfour\nFIVE\n"
	writeFile(t, filepath.Join(repo, "a.txt"), edit)
	res, created, err := OnFeatureBranch(repo, "auto-land", featureLand(repo))
	if err != nil || res.Status != "dirty" || created {
		t.Fatalf("got %+v created=%v err=%v", res, created, err)
	}
	if b := headBranch(t, repo); b != "main" {
		t.Fatalf("HEAD is %s", b)
	}
	if fb := featureBranches(t, repo); fb != "" {
		t.Fatalf("feature branches left: %s", fb)
	}
	if got := readFile(t, filepath.Join(repo, "a.txt")); got != edit {
		t.Fatalf("a.txt lost the edit: %q", got)
	}
}
