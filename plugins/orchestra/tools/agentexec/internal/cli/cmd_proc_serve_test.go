package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/proctree"
)

// procWatchBase is a past whole second, so a file the test does not Chtimes
// is never older than a day at the watcher's now.
var procWatchBase = time.Unix(1_700_000_000, 0)

// procWatchTable is owner (PPID 1) plus, per {pid, n}, a child of owner with
// n children of its own (pids pid*1000+1 … pid*1000+n). Every proc has
// PGID = PID and 1 MiB.
func procWatchTable(owner int, kids ...[2]int) proctree.Table {
	t := proctree.Table{{PID: owner, PPID: 1, PGID: owner, Mem: 1 << 20}}
	for _, k := range kids {
		t = append(t, proctree.Proc{PID: k[0], PPID: owner, PGID: k[0], Mem: 1 << 20})
		for i := 1; i <= k[1]; i++ {
			pid := k[0]*1000 + i
			t = append(t, proctree.Proc{PID: pid, PPID: k[0], PGID: pid, Mem: 1 << 20})
		}
	}
	return t
}

type procWatchStep struct {
	table proctree.Table
	err   error
}

type procWatchRig struct {
	now       time.Time
	snapshots int
	kills     [][2]int
	signals   int
	onKill    func(owner, child int)
}

// procWatchSetup isolates the state dir and replaces every seam: the
// snapshots play steps in order and then report owner gone.
func procWatchSetup(t *testing.T, steps ...procWatchStep) *procWatchRig {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	rig := &procWatchRig{now: procWatchBase}
	snapshot, kill, now, sleep, signal := procWatchSnapshot, procWatchKill, procWatchNow, procWatchSleep, proctree.Signal
	t.Cleanup(func() {
		procWatchSnapshot, procWatchKill, procWatchNow, procWatchSleep, proctree.Signal = snapshot, kill, now, sleep, signal
		if rig.signals != 0 {
			t.Errorf("proctree.Signal called %d times, want 0", rig.signals)
		}
	})
	procWatchSnapshot = func() (proctree.Table, error) {
		rig.snapshots++
		if rig.snapshots > len(steps)+3 {
			t.Fatalf("snapshot %d: the watcher did not stop", rig.snapshots)
		}
		if rig.snapshots <= len(steps) {
			s := steps[rig.snapshots-1]
			return s.table, s.err
		}
		return proctree.Table{{PID: 1, PGID: 1}}, nil
	}
	procWatchKill = func(owner, child int) (int, error) {
		if rig.onKill != nil {
			rig.onKill(owner, child)
		}
		rig.kills = append(rig.kills, [2]int{owner, child})
		return 0, nil
	}
	procWatchNow = func() time.Time { return rig.now }
	procWatchSleep = func(d time.Duration) { rig.now = rig.now.Add(d) }
	proctree.Signal = func(int, syscall.Signal) error {
		rig.signals++
		return nil
	}
	return rig
}

func procWatchJSONFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(procWatchDir())
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			out = append(out, e.Name())
		}
	}
	slices.Sort(out)
	return out
}

func procWatchPlant(t *testing.T, name string, age time.Duration) string {
	t.Helper()
	if err := os.MkdirAll(procWatchDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(procWatchDir(), name)
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	at := procWatchBase.Add(-age)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
	return path
}

func procWatchT() proctree.Table {
	return procWatchTable(4242, [2]int{5000, 201}, [2]int{5001, 3})
}

func TestProcWatchServeTrips(t *testing.T) {
	rig := procWatchSetup(t, procWatchStep{table: procWatchT()}, procWatchStep{table: procWatchT()})
	rig.onKill = func(owner, child int) {
		if _, err := os.Stat(procWatchRecordPath(owner, child, rig.now.UnixMilli())); err != nil {
			t.Errorf("record missing at kill: %v", err)
		}
		if !procWatchHeld(4242) {
			t.Error("lock not held while watching")
		}
	}
	if err := procWatchServe(4242); err != nil {
		t.Fatalf("err = %v", err)
	}
	if want := [][2]int{{4242, 5000}, {4242, 5000}}; !reflect.DeepEqual(rig.kills, want) {
		t.Fatalf("kills = %v, want %v", rig.kills, want)
	}
	if got, want := procWatchJSONFiles(t), []string{"4242-5000-1700000000000.json", "4242-5000-1700000002000.json"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("records = %v, want %v", got, want)
	}
	data, err := os.ReadFile(procWatchRecordPath(4242, 5000, 1700000000000))
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal([]byte(`{"at_ms":1700000000000,"owner":4242,"root":5000,"procs":201,"mem_bytes":211812352,"reason":"process guard: 201 processes under pid 5000 (limit 200)"}`), &want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("record = %v, want %v", got, want)
	}
	if procWatchHeld(4242) {
		t.Fatal("lock still held after return")
	}
}

func TestProcWatchServeOwnerGone(t *testing.T) {
	rig := procWatchSetup(t)
	if err := procWatchServe(4242); err != nil {
		t.Fatalf("err = %v", err)
	}
	if rig.snapshots != 1 || len(rig.kills) != 0 {
		t.Fatalf("snapshots = %d, kills = %v; want 1, none", rig.snapshots, rig.kills)
	}

	owner := os.Getpid() + 1
	rig = procWatchSetup(t, procWatchStep{table: procWatchTable(owner, [2]int{os.Getpid(), 201})})
	if err := procWatchServe(owner); err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(rig.kills) != 0 {
		t.Fatalf("kills = %v; the watcher judged itself", rig.kills)
	}
	if files := procWatchJSONFiles(t); len(files) != 0 {
		t.Fatalf("records = %v, want none", files)
	}
}

func TestProcWatchServeSnapshotErrors(t *testing.T) {
	rig := procWatchSetup(t, procWatchStep{err: errors.New("table busy")}, procWatchStep{table: procWatchT()})
	if err := procWatchServe(4242); err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(rig.kills) != 1 {
		t.Fatalf("kills = %v, want one", rig.kills)
	}

	rig = procWatchSetup(t, procWatchStep{err: fmt.Errorf("read: %w", proctree.ErrUnsupported)})
	if err := procWatchServe(4242); !errors.Is(err, proctree.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	if rig.snapshots != 1 || len(rig.kills) != 0 {
		t.Fatalf("snapshots = %d, kills = %v; want 1, none", rig.snapshots, rig.kills)
	}
}

func TestProcWatchServeLockBusy(t *testing.T) {
	rig := procWatchSetup(t, procWatchStep{table: procWatchT()})
	old := procWatchPlant(t, "1-2-3.json", 48*time.Hour)
	unlock, err := fsutil.Lock(procWatchLockPath(4242), fsutil.Exclusive, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := procWatchServe(4242); err != nil {
		t.Fatalf("err = %v", err)
	}
	if rig.snapshots != 0 || len(rig.kills) != 0 {
		t.Fatalf("snapshots = %d, kills = %v; want 0, none", rig.snapshots, rig.kills)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("expired under another watcher's lock: %v", err)
	}
	if !procWatchHeld(4242) {
		t.Fatal("procWatchHeld = false while the lock is held")
	}
}

func TestProcWatchHeld(t *testing.T) {
	procWatchSetup(t)
	if procWatchHeld(4242) {
		t.Fatal("procWatchHeld = true with no state dir")
	}
	if _, err := os.Stat(procWatchLockPath(4242)); !os.IsNotExist(err) {
		t.Fatalf("probe created the lock file: %v", err)
	}
}

func TestProcWatchServeExpires(t *testing.T) {
	procWatchSetup(t)
	gone := []string{
		procWatchPlant(t, "1-2-3.json", 25*time.Hour),
		procWatchPlant(t, "abcdefgh12.json", 25*time.Hour),
		procWatchPlant(t, "watch-777.lock", 25*time.Hour),
	}
	held := procWatchPlant(t, "watch-778.lock", 25*time.Hour)
	unlock, err := fsutil.Lock(held, fsutil.Exclusive, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	kept := []string{
		held,
		procWatchPlant(t, "watch-4242.lock", 48*time.Hour),
		procWatchPlant(t, "3-4-5.json", 24*time.Hour),
		procWatchPlant(t, "5-6-7.json", 23*time.Hour),
	}
	if err := procWatchServe(4242); err != nil {
		t.Fatalf("err = %v", err)
	}
	for _, p := range gone {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s kept: %v", filepath.Base(p), err)
		}
	}
	for _, p := range kept {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s removed: %v", filepath.Base(p), err)
		}
	}
}

func TestProcWatchServeLowOwner(t *testing.T) {
	for _, owner := range []int{0, 1} {
		rig := procWatchSetup(t)
		err := procWatchServe(owner)
		if want := fmt.Sprintf("proc watch: owner must be a pid > 1, got %d", owner); err == nil || err.Error() != want {
			t.Fatalf("err = %v, want %q", err, want)
		}
		if rig.snapshots != 0 {
			t.Fatalf("snapshots = %d, want 0", rig.snapshots)
		}
		if _, err := os.Stat(procWatchLockPath(owner)); !os.IsNotExist(err) {
			t.Fatalf("lock file touched: %v", err)
		}
	}
}
