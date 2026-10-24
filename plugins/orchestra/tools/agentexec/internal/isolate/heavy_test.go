package isolate

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/slot"
)

// onePool makes the heavy pool a real pool of one slot for the test.
func onePool(t *testing.T) slot.Pool {
	t.Helper()
	t.Setenv("ORCHESTRA_SLOT_DIR", t.TempDir())
	p := slot.New(slot.Heavy, 1)
	saved := slot.HeavyPool
	slot.HeavyPool = func() slot.Pool { return p }
	t.Cleanup(func() { slot.HeavyPool = saved })
	return p
}

// within fails the test when fn does not return in time: a slot taken
// twice in one process would block forever.
func within(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatalf("%s blocked: a heavy slot leaked or was taken twice", what)
	}
}

func TestRefusedCheckoutsReleaseTheirSlot(t *testing.T) {
	repo := newRepo(t)
	p := onePool(t)
	// gtr is "installed" but refuses every tree, and git refuses too: the
	// branch already exists.
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "git-gtr"), "#!/bin/sh\n[ \"$1\" = version ] && exit 0\nexit 1\n")
	if err := os.Chmod(filepath.Join(bin, "git-gtr"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	git(t, repo, "branch", BranchPrefix+"t1")
	var res CreateResult
	within(t, "create", func() { res, _ = Create(repo, "t1", CreateOptions{}) })
	if res.Status != "error" {
		t.Fatalf("expected both checkouts to fail: %+v", res)
	}
	if leases, _ := p.Leases(); len(leases) != 0 {
		t.Fatalf("a refused checkout kept its slot: %+v", leases)
	}
	git(t, repo, "branch", "-D", BranchPrefix+"t1")
	within(t, "create", func() { res, _ = Create(repo, "t1", CreateOptions{Backend: BackendGit}) })
	if res.Status != "created" {
		t.Fatalf("%+v", res)
	}
}

func TestRefreshRemovesAndRecreatesWithOneSlot(t *testing.T) {
	repo := newRepo(t)
	onePool(t)
	wt := create(t, repo, "alpha", "")
	writeFile(t, filepath.Join(wt, "README.md"), "hello\nALPHA\n")
	writeFile(t, filepath.Join(repo, "main-only.txt"), "main\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "advance")
	var res RefreshResult
	var err error
	within(t, "refresh", func() { res, err = Refresh(repo, "alpha", "", "") })
	if err != nil || res.Status != "ok" {
		t.Fatalf("refresh: %+v %v", res, err)
	}
}
