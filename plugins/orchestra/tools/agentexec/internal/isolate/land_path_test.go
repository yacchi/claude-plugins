package isolate

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var landPathFilter = ChangeFilter{Exclude: []string{".pnpm-store/", "node_modules/"}}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return strings.TrimSpace(git(t, dir, args...))
}

// agentTree is an Agent-tool worktree forked from main on worktree-agent-x:
// b.txt committed, c.txt and an ignored-by-config .pnpm-store file left
// uncommitted.
func agentTree(t *testing.T, repo string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "agent")
	git(t, repo, "worktree", "add", "-q", "-b", "worktree-agent-x", dir)
	writeFile(t, filepath.Join(dir, "b.txt"), "b2\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "agent: b")
	writeFile(t, filepath.Join(dir, "c.txt"), "c\n")
	writeFile(t, filepath.Join(dir, ".pnpm-store", "v11", "x"), "x\n")
	return dir
}

func TestLandPathTakesCommittedAndUncommittedWork(t *testing.T) {
	repo := landRepo(t)
	dir := agentTree(t, repo)
	mainBefore := gitOut(t, repo, "rev-parse", "HEAD")
	agentHead := gitOut(t, dir, "rev-parse", "HEAD")
	agentIndex := gitOut(t, dir, "diff", "--cached")

	res, err := landPathWith(repo, dir, "", "feat: add b and c", landPathFilter)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "landed" || !slices.Equal(res.Files, []string{"b.txt", "c.txt"}) {
		t.Fatalf("landed: %+v", res)
	}
	if got := gitOut(t, repo, "log", "-1", "--format=%an|%ae|%s"); got != "Land User|land@example.com|feat: add b and c" {
		t.Fatalf("commit identity or message: %q", got)
	}
	if got := gitOut(t, repo, "rev-parse", "HEAD^"); got != mainBefore {
		t.Fatalf("landed commit is not on main's tip: parent %s, want %s", got, mainBefore)
	}
	if got := gitOut(t, repo, "diff", "--name-only", "HEAD^", "HEAD"); got != "b.txt\nc.txt" {
		t.Fatalf("landed files: %q", got)
	}
	if got := gitOut(t, repo, "log", "--format=%an%s", mainBefore+"..HEAD"); strings.Contains(strings.ToLower(got), "orchestra") {
		t.Fatalf("orchestra trace on main: %q", got)
	}
	if got := gitOut(t, repo, "rev-parse", "worktree-agent-x"); got != agentHead {
		t.Fatalf("agent branch moved: %s, want %s", got, agentHead)
	}
	if got := gitOut(t, dir, "rev-parse", "HEAD"); got != agentHead {
		t.Fatalf("agent HEAD moved: %s, want %s", got, agentHead)
	}
	if got := gitOut(t, dir, "diff", "--cached"); got != agentIndex {
		t.Fatalf("agent index changed: %q", got)
	}
}

func TestLandPathOnMovedCheckout(t *testing.T) {
	repo := landRepo(t)
	dir := agentTree(t, repo)
	writeFile(t, filepath.Join(repo, "d.txt"), "d\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "chore: add d")
	moved := gitOut(t, repo, "rev-parse", "HEAD")

	res, err := landPathWith(repo, dir, "", "feat: add b and c", landPathFilter)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "landed" || !slices.Equal(res.Files, []string{"b.txt", "c.txt"}) {
		t.Fatalf("landed on moved checkout: %+v", res)
	}
	if got := gitOut(t, repo, "rev-parse", "HEAD^"); got != moved {
		t.Fatalf("landed commit not on the moved tip: parent %s, want %s", got, moved)
	}
	if got := gitOut(t, repo, "show", "HEAD:d.txt"); got != "d" {
		t.Fatalf("the user's commit lost: d.txt %q", got)
	}
}

func TestLandPathRefusesWhatIsNotALandableTree(t *testing.T) {
	repo := landRepo(t)

	if r, err := landPathWith(repo, repo, "", "feat: x", landPathFilter); err != nil || r.Status != "absent" ||
		!strings.Contains(r.Reason, "is the checkout itself") {
		t.Fatalf("checkout itself: %+v, %v", r, err)
	}
	if r, err := landPathWith(repo, t.TempDir(), "", "feat: x", landPathFilter); err != nil || r.Status != "absent" ||
		!strings.Contains(r.Reason, "not a linked worktree") {
		t.Fatalf("plain directory: %+v, %v", r, err)
	}
	tree := create(t, repo, "t1", "")
	if r, err := landPathWith(repo, tree, "", "feat: x", landPathFilter); err != nil || r.Status != "refused" ||
		!strings.Contains(r.Reason, "land it with --task") {
		t.Fatalf("orchestra's tree: %+v, %v", r, err)
	}

	// From a linked checkout, the main worktree is not a linked one.
	git(t, repo, "checkout", "-q", "--detach")
	checkout := filepath.Join(t.TempDir(), "checkout")
	git(t, repo, "worktree", "add", "-q", checkout, "main")
	if r, err := landPathWith(checkout, repo, "", "feat: x", landPathFilter); err != nil || r.Status != "absent" ||
		!strings.Contains(r.Reason, "not a linked worktree") {
		t.Fatalf("main worktree: %+v, %v", r, err)
	}
}

func TestLandMaxAddedFilesRefusesOnBothSources(t *testing.T) {
	repo := landRepo(t)
	dir := agentTree(t, repo)
	writeFile(t, filepath.Join(dir, "d.txt"), "d\n")
	writeFile(t, filepath.Join(dir, "e.txt"), "e\n")
	capped := ChangeFilter{Exclude: landPathFilter.Exclude, MaxAdded: 2}
	if r, err := landPathWith(repo, dir, "", "feat: x", capped); err != nil || r.Status != "refused" ||
		r.Reason != "adds 3 new files (isolation.max_added_files 2)" || len(r.Files) != 0 {
		t.Fatalf("agent tree over the cap: %+v, %v", r, err)
	}

	integration := landIntegration(t, repo)
	writeFile(t, filepath.Join(integration, "d.txt"), "d\n")
	writeFile(t, filepath.Join(integration, "e.txt"), "e\n")
	landCommit(t, integration, "orchestra integrate pc")
	if r, err := landWith(repo, "prog", "feat: x", capped); err != nil || r.Status != "refused" ||
		r.Reason != "adds 3 new files (isolation.max_added_files 2)" {
		t.Fatalf("integration tree over the cap: %+v, %v", r, err)
	}
	at := ChangeFilter{Exclude: landPathFilter.Exclude, MaxAdded: 3}
	if r, err := landWith(repo, "prog", "feat: x", at); err != nil || r.Status != "landed" {
		t.Fatalf("integration tree at the cap: %+v, %v", r, err)
	}
}

func TestLandPathEmptyAndBadBaseline(t *testing.T) {
	repo := landRepo(t)
	dir := filepath.Join(t.TempDir(), "agent")
	git(t, repo, "worktree", "add", "-q", "-b", "worktree-agent-y", dir)
	if r, err := landPathWith(repo, dir, "", "feat: x", landPathFilter); err != nil || r.Status != "empty" {
		t.Fatalf("clean agent tree: %+v, %v", r, err)
	}

	git(t, repo, "checkout", "-q", "-b", "side")
	writeFile(t, filepath.Join(repo, "x.txt"), "x\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "side: x")
	side := gitOut(t, repo, "rev-parse", "HEAD")
	git(t, repo, "checkout", "-q", "main")
	if r, err := landPathWith(repo, dir, side, "feat: x", landPathFilter); err != nil || r.Status != "refused" {
		t.Fatalf("baseline not an ancestor: %+v, %v", r, err)
	}
}
