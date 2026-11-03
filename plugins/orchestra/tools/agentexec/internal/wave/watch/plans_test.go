package watch

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// sweepPlansAge sets the modification time of every entry under root to
// mtime. Symlinks are stamped themselves (Lutimes), not their targets.
func sweepPlansAge(t *testing.T, root string, mtime time.Time) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			tv := unix.NsecToTimeval(mtime.UnixNano())
			return unix.Lutimes(path, []unix.Timeval{tv, tv})
		}
		return os.Chtimes(path, mtime, mtime)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func sweepPlansWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSweepPlansAge(t *testing.T) {
	now := time.Now()
	root := t.TempDir()
	sweepPlansWrite(t, filepath.Join(root, "idle", "f.txt"), "x")
	sweepPlansWrite(t, filepath.Join(root, "fresh", "f.txt"), "x")
	sweepPlansWrite(t, filepath.Join(root, "edge", "f.txt"), "x")
	sweepPlansWrite(t, filepath.Join(root, "almost", "f.txt"), "x")
	sweepPlansAge(t, filepath.Join(root, "idle"), now.Add(-8*24*time.Hour))
	sweepPlansAge(t, filepath.Join(root, "fresh"), now.Add(-time.Hour))
	sweepPlansAge(t, filepath.Join(root, "edge"), now.Add(-7*24*time.Hour))
	sweepPlansAge(t, filepath.Join(root, "almost"), now.Add(-7*24*time.Hour+time.Minute))

	got := SweepPlans(root, now, false)
	want := []PlanSweep{
		{Dir: filepath.Join(root, "edge"), Status: "removed", IdleDays: 7},
		{Dir: filepath.Join(root, "idle"), Status: "removed", IdleDays: 8},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
	for _, kept := range []string{"fresh", "almost"} {
		if _, err := os.Stat(filepath.Join(root, kept)); err != nil {
			t.Errorf("%s was removed: %v", kept, err)
		}
	}
}

func TestSweepPlansDeepWriteKeeps(t *testing.T) {
	now := time.Now()
	root := t.TempDir()
	sub := filepath.Join(root, "plan")
	sweepPlansWrite(t, filepath.Join(sub, "a", "b", "c", "old.txt"), "x")
	sweepPlansAge(t, sub, now.Add(-8*24*time.Hour))
	sweepPlansWrite(t, filepath.Join(sub, "a", "b", "c", "fresh.txt"), "y")

	if got := SweepPlans(root, now, false); len(got) != 0 {
		t.Fatalf("plan with a fresh file three levels deep was swept: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(sub, "a", "b", "c", "fresh.txt")); err != nil {
		t.Fatal(err)
	}
}

func TestSweepPlansLiveRunnerKeeps(t *testing.T) {
	now := time.Now()
	orig := processAge
	processAge = func(int) (float64, bool, bool) { return 0, false, false }
	t.Cleanup(func() { processAge = orig })

	root := t.TempDir()
	live := filepath.Join(root, "live")
	other := filepath.Join(root, "other")
	for _, sub := range []string{live, other} {
		statePath := filepath.Join(sub, "state.json")
		sweepPlansWrite(t, statePath, "{}")
		state := statePath
		if sub == other {
			state = filepath.Join(root, "elsewhere", "state.json")
		}
		sweepPlansWriteRunner(t, sub, state, now)
		sweepPlansAge(t, sub, now.Add(-8*24*time.Hour))
	}

	got := SweepPlans(root, now, false)
	if len(got) != 1 || got[0].Dir != other || got[0].Status != "removed" {
		t.Fatalf("got %+v, want only %s removed", got, other)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("plan with a live runner was removed: %v", err)
	}
}

func sweepPlansWriteRunner(t *testing.T, sub, state string, now time.Time) {
	t.Helper()
	rec := map[string]any{
		"pid":        os.Getpid(),
		"started_at": now.Unix(),
		"argv":       []string{"agent-exec", "wave", "run", "--state", state},
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	sweepPlansWrite(t, filepath.Join(sub, "runner.pid"), string(data))
}

func TestSweepPlansDryRun(t *testing.T) {
	now := time.Now()
	root := t.TempDir()
	sub := filepath.Join(root, "a")
	sweepPlansWrite(t, filepath.Join(sub, "f.txt"), "x")
	sweepPlansAge(t, sub, now.Add(-8*24*time.Hour))

	got := SweepPlans(root, now, true)
	if len(got) != 1 || got[0].Status != "would-remove" {
		t.Fatalf("got %+v, want one would-remove", got)
	}
	if _, err := os.Stat(filepath.Join(sub, "f.txt")); err != nil {
		t.Fatalf("dry run touched the plan: %v", err)
	}
}

func TestSweepPlansIgnoresFilesAndLinksInDir(t *testing.T) {
	now := time.Now()
	root := t.TempDir()
	outside := t.TempDir()
	sweepPlansWrite(t, filepath.Join(outside, "target.txt"), "x")
	sweepPlansWrite(t, filepath.Join(root, "loose.txt"), "x")
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	sweepPlansAge(t, root, now.Add(-8*24*time.Hour))
	sweepPlansAge(t, outside, now.Add(-8*24*time.Hour))

	if got := SweepPlans(root, now, false); len(got) != 0 {
		t.Fatalf("got %+v, want nothing", got)
	}
	for _, p := range []string{
		filepath.Join(root, "loose.txt"),
		filepath.Join(root, "link"),
		filepath.Join(outside, "target.txt"),
	} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s was removed: %v", p, err)
		}
	}
}

func TestSweepPlansSymlinkNotFollowed(t *testing.T) {
	now := time.Now()
	root := t.TempDir()
	outside := t.TempDir()
	sweepPlansWrite(t, filepath.Join(outside, "fresh.txt"), "x")
	sub := filepath.Join(root, "plan")
	sweepPlansWrite(t, filepath.Join(sub, "own.txt"), "x")
	link := filepath.Join(sub, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	sweepPlansAge(t, sub, now.Add(-8*24*time.Hour))

	got := SweepPlans(root, now, false)
	if len(got) != 1 || got[0].Status != "removed" {
		t.Fatalf("got %+v, want plan removed", got)
	}
	if _, err := os.Stat(filepath.Join(outside, "fresh.txt")); err != nil {
		t.Fatalf("symlink target was removed or hidden: %v", err)
	}
}

func TestSweepPlansSortedByDir(t *testing.T) {
	now := time.Now()
	root := t.TempDir()
	for _, name := range []string{"c", "a", "b"} {
		sub := filepath.Join(root, name)
		sweepPlansWrite(t, filepath.Join(sub, "f.txt"), "x")
		sweepPlansAge(t, sub, now.Add(-8*24*time.Hour))
	}

	got := SweepPlans(root, now, true)
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %d entries", got, len(want))
	}
	for i, name := range want {
		if got[i].Dir != filepath.Join(root, name) {
			t.Errorf("entry %d: got %s, want %s", i, got[i].Dir, name)
		}
	}
}

func TestSweepPlansRemoveError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes files in a read-only directory")
	}
	now := time.Now()
	root := t.TempDir()
	sub := filepath.Join(root, "a")
	sweepPlansWrite(t, filepath.Join(sub, "f.txt"), "x")
	sweepPlansAge(t, sub, now.Add(-8*24*time.Hour))
	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	got := SweepPlans(root, now, false)
	if len(got) != 1 || got[0].Status != "error" || got[0].Reason == "" {
		t.Fatalf("got %+v, want one error entry with a reason", got)
	}
}
