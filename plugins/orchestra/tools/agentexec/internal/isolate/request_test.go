package isolate

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// requestRootPin points request workspaces at a fresh directory for one test.
func requestRootPin(t *testing.T) string {
	t.Helper()
	root := filepath.Join(paths.RealPath(t.TempDir()), "requests")
	t.Setenv(RequestRootEnv, root)
	return root
}

// requestRootClock fixes requestNow at 2026-10-28 (local) for one test.
func requestRootClock(t *testing.T) {
	t.Helper()
	prev := requestNow
	requestNow = func() time.Time { return time.Date(2026, 10, 28, 9, 0, 0, 0, time.Local) }
	t.Cleanup(func() { requestNow = prev })
}

// requestRootSave writes r's manifest into <root>/<r.ID>.
func requestRootSave(t *testing.T, root string, r Request) Request {
	t.Helper()
	r.Dir = filepath.Join(root, r.ID)
	if err := saveRequest(r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRequestRootFromEnv(t *testing.T) {
	want := paths.RealPath(t.TempDir())
	t.Setenv(RequestRootEnv, want)
	got, err := RequestRoot()
	if err != nil || got != want {
		t.Fatalf("RequestRoot() = %q, %v; want %q", got, err, want)
	}
}

func TestRequestRootDefaultsToStateDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(RequestRootEnv, "")
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	got, err := RequestRoot()
	want := paths.RealPath(filepath.Join(home, "state", "claude-orchestra", "requests"))
	if err != nil || got != want {
		t.Fatalf("RequestRoot() = %q, %v; want %q", got, err, want)
	}
}

func TestRequestRootRejectsRelativePath(t *testing.T) {
	t.Setenv(RequestRootEnv, "rel/requests")
	_, err := RequestRoot()
	if err == nil || !strings.Contains(err.Error(), RequestRootEnv) {
		t.Fatalf("RequestRoot() error = %v; want one naming %s", err, RequestRootEnv)
	}
}

func TestNewRequestIDUsesSessionAndFirstFreeSuffix(t *testing.T) {
	root := requestRootPin(t)
	requestRootClock(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", sessA)
	if got := newRequestID(root); got != "20261028-aaaaaaaa" {
		t.Fatalf("newRequestID() = %q; want 20261028-aaaaaaaa", got)
	}
	for _, taken := range []string{"20261028-aaaaaaaa", "20261028-aaaaaaaa-2"} {
		if err := saveRequest(Request{Dir: filepath.Join(root, taken)}); err != nil {
			t.Fatal(err)
		}
	}
	if got := newRequestID(root); got != "20261028-aaaaaaaa-3" {
		t.Fatalf("newRequestID() with -2 taken = %q; want 20261028-aaaaaaaa-3", got)
	}
}

func TestNewRequestIDOutsideSessionIsLocal(t *testing.T) {
	root := requestRootPin(t)
	requestRootClock(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	if got := newRequestID(root); got != "20261028-local" {
		t.Fatalf("newRequestID() = %q; want 20261028-local", got)
	}
}

func TestWithRequestsEndsRequestsWithoutLiveAssembly(t *testing.T) {
	repo := newRepo(t)
	root, err := RequestRoot()
	if err != nil {
		t.Fatal(err)
	}
	common, err := RepoCommon(repo)
	if err != nil {
		t.Fatal(err)
	}
	asm := create(t, repo, "live", "")
	if err := WriteRole(asm, RoleAssembly); err != nil {
		t.Fatal(err)
	}
	requestRootSave(t, root, Request{ID: "20261028-live", Repos: []RequestRepo{{Common: common, Assembly: asm}}})
	ended := requestRootSave(t, root, Request{ID: "20261028-ended", Repos: []RequestRepo{{Common: common, Assembly: filepath.Join(root, "gone")}}})
	writeFile(t, filepath.Join(root, ended.ID, "stray", "notes.txt"), "keep me\n")
	if err := os.MkdirAll(filepath.Join(root, ended.ID, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	var got []Request
	if err := withRequests(func(_ string, open []Request) error {
		got = open
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "20261028-live" || got[0].Dir != filepath.Join(root, "20261028-live") {
		t.Fatalf("open requests = %+v; want only 20261028-live in its workspace", got)
	}
	if exists(filepath.Join(root, ended.ID, requestFile)) || exists(filepath.Join(root, ended.ID, "empty")) {
		t.Fatal("ended workspace kept its manifest or empty directories")
	}
	if !exists(filepath.Join(root, ended.ID, "stray", "notes.txt")) {
		t.Fatal("ended workspace lost a stray file")
	}
}

func TestWithRequestsKeepsUnreadableManifest(t *testing.T) {
	root := requestRootPin(t)
	broken := filepath.Join(root, "broken", requestFile)
	writeFile(t, broken, "{not json")
	var n int
	if err := withRequests(func(_ string, open []Request) error {
		n = len(open)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n != 0 || !exists(broken) {
		t.Fatalf("open = %d, manifest kept = %v; want 0 and true", n, exists(broken))
	}
}

func TestWithRequestsRunsOneAtATime(t *testing.T) {
	requestRootPin(t)
	var inside atomic.Int32
	var overlapped atomic.Bool
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := withRequests(func(string, []Request) error {
				if inside.Add(1) > 1 {
					overlapped.Store(true)
				}
				time.Sleep(10 * time.Millisecond)
				inside.Add(-1)
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if overlapped.Load() {
		t.Fatal("two request operations ran at once")
	}
}

func TestPickCurrentMatchesFullSessionID(t *testing.T) {
	t0 := time.Date(2026, 10, 28, 9, 0, 0, 0, time.Local)
	sameShortID := "aaaaaaaa-9999-4000-8000-000000000009"
	open := []Request{
		{ID: "old", Session: sessA, Created: t0.Format(time.RFC3339)},
		{ID: "newer", Session: sessA, Created: t0.Add(time.Hour).Format(time.RFC3339)},
		{ID: "other", Session: sameShortID, Created: t0.Add(2 * time.Hour).Format(time.RFC3339)},
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", sessA)
	got, ok := pickCurrent(open)
	if !ok || got.ID != "newer" {
		t.Fatalf("pickCurrent() = %q, %v; want newer", got.ID, ok)
	}
}

func TestRepoCommonIsSharedByWorktrees(t *testing.T) {
	repo := newRepo(t)
	asm := create(t, repo, "shared", "")
	fromRepo, err := RepoCommon(repo)
	if err != nil {
		t.Fatal(err)
	}
	fromTree, err := RepoCommon(asm)
	if err != nil {
		t.Fatal(err)
	}
	if fromRepo != fromTree || fromRepo != paths.RealPath(filepath.Join(repo, ".git")) {
		t.Fatalf("RepoCommon: repo %q, worktree %q; want both %q", fromRepo, fromTree, filepath.Join(repo, ".git"))
	}
}
