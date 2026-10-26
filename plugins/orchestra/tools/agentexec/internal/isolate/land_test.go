package isolate

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
)

// landRepo is newRepo whose identity comes from the repository's config
// (not the environment, which would override CommitAll's orchestra identity
// too), with a.txt and b.txt committed on main.
func landRepo(t *testing.T) string {
	t.Helper()
	repo := newRepo(t)
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	git(t, repo, "config", "user.name", "Land User")
	git(t, repo, "config", "user.email", "land@example.com")
	writeFile(t, filepath.Join(repo, "a.txt"), "one\ntwo\nthree\nfour\nfive\n")
	writeFile(t, filepath.Join(repo, "b.txt"), "b\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "seed")
	return repo
}

// landIntegration plants the "prog" integration tree on main's tip, as the
// wave runner does, with commits A (a.txt edit) and B (new c.txt) made by
// gitx.CommitAll.
func landIntegration(t *testing.T, repo string) string {
	t.Helper()
	head := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))
	res, err := Create(repo, "prog", CreateOptions{Backend: BackendGit, Onto: head})
	if err != nil || res.Status != "created" {
		t.Fatalf("create: %+v, %v", res, err)
	}
	if err := WriteRole(res.Path, RoleIntegration); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(res.Path, "a.txt"), "ONE\ntwo\nthree\nfour\nfive\n")
	landCommit(t, res.Path, "orchestra integrate pa")
	writeFile(t, filepath.Join(res.Path, "c.txt"), "c\n")
	landCommit(t, res.Path, "orchestra integrate pb")
	return res.Path
}

func landCommit(t *testing.T, dir, msg string) {
	t.Helper()
	if err := gitx.CommitAll(dir, msg); err != nil {
		t.Fatal(err)
	}
}

func land(t *testing.T, repo, msg string) LandResult {
	t.Helper()
	res, err := Land(repo, "prog", msg)
	if err != nil {
		t.Fatalf("land: %v", err)
	}
	return res
}

func TestLandSquashesAsTheUser(t *testing.T) {
	repo := landRepo(t)
	base := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))
	path := landIntegration(t, repo)
	tip := strings.TrimSpace(git(t, path, "rev-parse", "HEAD"))
	msg := "feat: add c\n\nbody\n"
	res := land(t, repo, msg)
	if res.Status != "landed" || res.Branch != "main" || res.Base != base ||
		!slices.Equal(res.Files, []string{"a.txt", "c.txt"}) {
		t.Fatalf("land: %+v", res)
	}
	head := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))
	if head != res.Commit {
		t.Fatalf("main at %s, landed %s", head, res.Commit)
	}
	if got := strings.TrimSpace(git(t, repo, "rev-list", "--count", base+"..HEAD")); got != "1" {
		t.Fatalf("%s commits landed", got)
	}
	if got := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD^")); got != base {
		t.Fatalf("parent %s, want %s", got, base)
	}
	if git(t, repo, "rev-parse", "HEAD^{tree}") != git(t, repo, "rev-parse", tip+"^{tree}") {
		t.Fatal("landed tree differs from the integration tip's")
	}
	if who := git(t, repo, "log", "-1", "--format=%an <%ae>|%cn <%ce>"); who != "Land User <land@example.com>|Land User <land@example.com>\n" {
		t.Fatalf("identity: %q", who)
	}
	if got := git(t, repo, "log", "-1", "--format=%B"); strings.TrimRight(got, "\n") != strings.TrimRight(msg, "\n") {
		t.Fatalf("message: %q", got)
	}
	if out := git(t, repo, "branch", "--contains", tip, "--format=%(refname:short)"); strings.Contains(out, "main") {
		t.Fatal("integration commits reachable from main")
	}
	if strings.TrimSpace(git(t, repo, "status", "--porcelain")) != "" {
		t.Fatal("checkout left dirty")
	}
}

func TestLandOnMovedTarget(t *testing.T) {
	repo := landRepo(t)
	path := landIntegration(t, repo)
	// a.txt too, apart from the integrated edit: the squash then matches
	// the integration tip's content on no branch.
	writeFile(t, filepath.Join(repo, "a.txt"), "one\ntwo\nthree\nfour\nFIVE\n")
	writeFile(t, filepath.Join(repo, "b.txt"), "b moved\n")
	git(t, repo, "commit", "-q", "-am", "move a and b")
	moved := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))
	res := land(t, repo, "feat: land")
	if res.Status != "landed" || res.Base != moved {
		t.Fatalf("land: %+v", res)
	}
	if got := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD^")); got != moved {
		t.Fatalf("parent %s, want %s", got, moved)
	}
	if readFile(t, filepath.Join(repo, "a.txt")) != "ONE\ntwo\nthree\nfour\nFIVE\n" ||
		readFile(t, filepath.Join(repo, "b.txt")) != "b moved\n" || readFile(t, filepath.Join(repo, "c.txt")) != "c\n" {
		t.Fatal("merged content wrong")
	}

	// The squash no longer matches the tip's content anywhere; the landed
	// marker still lets the tree go, until it gains another commit.
	writeFile(t, filepath.Join(path, "d.txt"), "d\n")
	landCommit(t, path, "orchestra integrate pd")
	if r := remove(t, repo, "prog", false, ""); r.Status != "dirty" || !exists(path) {
		t.Fatalf("unlanded commit after landing: %+v", r)
	}
	git(t, path, "reset", "-q", "--hard", "HEAD^")
	if r := remove(t, repo, "prog", false, ""); r.Status != "removed" || exists(path) {
		t.Fatalf("landed tree: %+v", r)
	}
}

func TestLandConflictLeavesCheckoutUntouched(t *testing.T) {
	repo := landRepo(t)
	landIntegration(t, repo)
	writeFile(t, filepath.Join(repo, "a.txt"), "uno\ntwo\nthree\nfour\nfive\n")
	git(t, repo, "commit", "-q", "-am", "edit a")
	before := git(t, repo, "rev-parse", "HEAD")
	res := land(t, repo, "feat: land")
	if res.Status != "conflict" || !slices.Equal(res.Files, []string{"a.txt"}) || res.Reason == "" {
		t.Fatalf("land: %+v", res)
	}
	if git(t, repo, "rev-parse", "HEAD") != before || strings.TrimSpace(git(t, repo, "status", "--porcelain")) != "" {
		t.Fatal("conflict changed the checkout")
	}
}

func TestLandRefusals(t *testing.T) {
	repo := landRepo(t)
	path := landIntegration(t, repo)
	head := git(t, repo, "rev-parse", "HEAD")
	if _, err := Land(repo, "prog", " \n"); err == nil {
		t.Fatal("empty message accepted")
	}
	if r := land(t, repo, "Orchestra integrate pa"); r.Status != "refused" {
		t.Fatalf("orchestra subject: %+v", r)
	}

	writeFile(t, filepath.Join(repo, "b.txt"), "local edit\n")
	if r := land(t, repo, "feat: x"); r.Status != "dirty" || !slices.Equal(r.Files, []string{"b.txt"}) {
		t.Fatalf("dirty checkout: %+v", r)
	}
	git(t, repo, "checkout", "-q", "--", "b.txt")

	writeFile(t, filepath.Join(path, "e.txt"), "e\n")
	if r := land(t, repo, "feat: x"); r.Status != "dirty" || !slices.Equal(r.Files, []string{"e.txt"}) {
		t.Fatalf("dirty integration tree: %+v", r)
	}
	_ = os.Remove(filepath.Join(path, "e.txt"))
	git(t, path, "reset", "-q")

	git(t, repo, "checkout", "-q", "--detach")
	if r := land(t, repo, "feat: x"); r.Status != "refused" || !strings.Contains(r.Reason, "no branch") {
		t.Fatalf("detached: %+v", r)
	}
	git(t, repo, "checkout", "-q", "main")

	writeFile(t, filepath.Join(path, "node_modules", "x"), "x\n")
	git(t, path, "add", "-f", "node_modules/x")
	landCommit(t, path, "orchestra integrate deps")
	if r := land(t, repo, "feat: x"); r.Status != "refused" || !slices.Equal(r.Files, []string{"node_modules/x"}) {
		t.Fatalf("excluded path: %+v", r)
	}
	if git(t, repo, "rev-parse", "HEAD") != head {
		t.Fatal("a refusal committed")
	}
}

func TestLandNothingCommittedIsEmpty(t *testing.T) {
	repo := landRepo(t)
	head := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))
	res, err := Create(repo, "prog", CreateOptions{Backend: BackendGit, Onto: head})
	if err != nil || res.Status != "created" {
		t.Fatalf("create: %+v, %v", res, err)
	}
	_ = WriteRole(res.Path, RoleIntegration)
	if r := land(t, repo, "feat: x"); r.Status != "empty" || r.Branch != "main" {
		t.Fatalf("empty: %+v", r)
	}
	if strings.TrimSpace(git(t, repo, "rev-parse", "HEAD")) != head {
		t.Fatal("empty land committed")
	}
}
