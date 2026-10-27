package isolate

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// lookupPlant makes an assembly for request id in repo and saves the request
// under the pinned root, created offset after 2026-10-28 09:00.
func lookupPlant(t *testing.T, repo, id, session string, offset time.Duration) (Request, string) {
	t.Helper()
	root, err := RequestRoot()
	if err != nil {
		t.Fatal(err)
	}
	common, err := RepoCommon(repo)
	if err != nil {
		t.Fatal(err)
	}
	asm := create(t, repo, id, "")
	if err := WriteRole(asm, RoleAssembly); err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 10, 28, 9, 0, 0, 0, time.Local).Add(offset)
	r := requestRootSave(t, root, Request{
		ID:      id,
		Session: session,
		Created: created.Format(time.RFC3339),
		Repos:   []RequestRepo{{Common: common, Assembly: asm, Task: id}},
	})
	return r, asm
}

func TestLookupAssemblyForPrefersCallingSession(t *testing.T) {
	repo := newRepo(t)
	r1, _ := lookupPlant(t, repo, "20261028-one", sessA, 0)
	r2, _ := lookupPlant(t, repo, "20261028-two", "", time.Hour)

	t.Setenv("CLAUDE_CODE_SESSION_ID", sessA)
	task, candidates, err := AssemblyFor(repo)
	if err != nil || task != r1.ID || candidates != nil {
		t.Fatalf("AssemblyFor() with s-one = %q, %v, %v; want %q, nil", task, candidates, err, r1.ID)
	}
	cur, ok, err := CurrentRequest()
	if err != nil || !ok || cur.ID != r1.ID {
		t.Fatalf("CurrentRequest() with s-one = %q, %v, %v; want %q", cur.ID, ok, err, r1.ID)
	}

	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	task, candidates, err = AssemblyFor(repo)
	if err != nil || task != r2.ID || candidates != nil {
		t.Fatalf("AssemblyFor() outside a session = %q, %v, %v; want %q, nil", task, candidates, err, r2.ID)
	}
	cur, ok, err = CurrentRequest()
	if err != nil || !ok || cur.ID != r2.ID {
		t.Fatalf("CurrentRequest() outside a session = %q, %v, %v; want %q", cur.ID, ok, err, r2.ID)
	}
}

func TestLookupAssemblyForAmbiguousThenOnlyOne(t *testing.T) {
	repo := newRepo(t)
	r1, _ := lookupPlant(t, repo, "20261028-one", sessA, 0)
	r2, asm2 := lookupPlant(t, repo, "20261028-two", "", time.Hour)

	t.Setenv("CLAUDE_CODE_SESSION_ID", sessB)
	task, candidates, err := AssemblyFor(repo)
	if err != nil || task != "" || !slices.Equal(candidates, []string{r1.ID, r2.ID}) {
		t.Fatalf("AssemblyFor() = %q, %v, %v; want \"\" and [%s %s]", task, candidates, err, r1.ID, r2.ID)
	}

	git(t, repo, "worktree", "remove", "--force", asm2)
	task, candidates, err = AssemblyFor(repo)
	if err != nil || task != r1.ID || candidates != nil {
		t.Fatalf("AssemblyFor() after second assembly removed = %q, %v, %v; want %q, nil", task, candidates, err, r1.ID)
	}
	if exists(filepath.Join(r2.Dir, requestFile)) {
		t.Fatal("request with no live assembly was not ended by the lookup")
	}
}

func TestLookupRequestForFollowsSymlink(t *testing.T) {
	repo := newRepo(t)
	r1, asm := lookupPlant(t, repo, "20261028-one", sessA, 0)

	link := filepath.Join(t.TempDir(), "asm-link")
	if err := os.Symlink(asm, link); err != nil {
		t.Fatal(err)
	}
	got, ok, err := RequestFor(link)
	if err != nil || !ok || got.ID != r1.ID {
		t.Fatalf("RequestFor(symlink) = %q, %v, %v; want %q", got.ID, ok, err, r1.ID)
	}
}

func TestLookupFindRequest(t *testing.T) {
	repo := newRepo(t)
	r1, _ := lookupPlant(t, repo, "20261028-one", sessA, 0)

	got, ok, err := FindRequest(r1.ID)
	if err != nil || !ok || got.ID != r1.ID {
		t.Fatalf("FindRequest(%q) = %q, %v, %v; want found", r1.ID, got.ID, ok, err)
	}
	if _, ok, err := FindRequest("nope"); err != nil || ok {
		t.Fatalf("FindRequest(nope) found = %v, err %v; want not found", ok, err)
	}
}

func TestLookupEndRequestIfDone(t *testing.T) {
	repo := newRepo(t)
	r1, asm := lookupPlant(t, repo, "20261028-one", sessA, 0)

	done, err := EndRequestIfDone(r1.ID)
	if err != nil || done {
		t.Fatalf("EndRequestIfDone() with assembly alive = %v, %v; want false", done, err)
	}

	git(t, repo, "worktree", "remove", "--force", asm)
	done, err = EndRequestIfDone(r1.ID)
	if err != nil || !done {
		t.Fatalf("EndRequestIfDone() after assembly removed = %v, %v; want true", done, err)
	}
	open, err := ListRequests()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range open {
		if r.ID == r1.ID {
			t.Fatalf("ListRequests() still has %q after it ended", r1.ID)
		}
	}
}

func TestLookupRequestsForRepo(t *testing.T) {
	repo := newRepo(t)
	other := filepath.Join(t.TempDir(), "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, other, "init", "-q", "-b", "main")

	r1, _ := lookupPlant(t, repo, "20261028-one", sessA, 0)
	r2, _ := lookupPlant(t, repo, "20261028-two", "", time.Hour)

	got, err := RequestsFor(repo)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, r := range got {
		ids = append(ids, r.ID)
	}
	if !slices.Equal(ids, []string{r1.ID, r2.ID}) {
		t.Fatalf("RequestsFor(repo) = %v; want [%s %s]", ids, r1.ID, r2.ID)
	}

	none, err := RequestsFor(other)
	if err != nil || none == nil || len(none) != 0 {
		t.Fatalf("RequestsFor(repo with no request) = %v, %v; want empty non-nil", none, err)
	}
}
