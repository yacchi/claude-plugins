package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/proctree"
)

// procWatchMaxAge bounds how long a file in the watch directory is kept: a
// record nobody read in a day belongs to a command that is long gone. The
// watcher itself has no maximum age: it lives as long as its owner.
const procWatchMaxAge = 24 * time.Hour

// procWatchRecord is the trip record guard-result reads back by its JSON
// keys. It is written before the first signal, so a record exists for every
// kill.
type procWatchRecord struct {
	AtMS     int64  `json:"at_ms"`
	Owner    int    `json:"owner"`
	Root     int    `json:"root"`
	Procs    int    `json:"procs"`
	MemBytes int64  `json:"mem_bytes"`
	Reason   string `json:"reason"`
}

// The seams. Tests replace them and restore them with t.Cleanup.
// procWatchKill signals nothing from inside a test binary: a made-up pid can
// be a real process on this machine.
var (
	procWatchSnapshot = func() (proctree.Table, error) {
		if proctree.Snapshot == nil {
			return nil, proctree.ErrUnsupported
		}
		return proctree.Snapshot()
	}
	procWatchKill = func(owner, child int) (int, error) {
		if testing.Testing() {
			return 0, errors.New("proc watch: no kill under test")
		}
		return proctree.KillChildTree(owner, child)
	}
	procWatchNow   = time.Now
	procWatchSleep = time.Sleep
)

func procWatchDir() string {
	return filepath.Join(paths.StateDir(), "procguard")
}

func procWatchLockPath(owner int) string {
	return filepath.Join(procWatchDir(), "watch-"+strconv.Itoa(owner)+".lock")
}

func procWatchRecordPath(owner, child int, atMS int64) string {
	return filepath.Join(procWatchDir(), fmt.Sprintf("%d-%d-%d.json", owner, child, atMS))
}

// procWatchLockHeld reports whether someone holds the flock on path right
// now. A probe only: the file is never created, and anything unexpected
// reads as "not held". The deferred Close drops whatever the probe got.
func procWatchLockHeld(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
}

// procWatchHeld reports whether a watcher for owner is running.
func procWatchHeld(owner int) bool {
	return procWatchLockHeld(procWatchLockPath(owner))
}

// procWatchServe is the watcher of one Claude Code process: it kills the tree
// under any direct child of owner that outgrows the limits, and returns once
// owner is gone. It prints nothing; the record is its only output.
func procWatchServe(owner int) error {
	// pid 1's children are every orphan on the machine.
	if owner <= 1 {
		return fmt.Errorf("proc watch: owner must be a pid > 1, got %d", owner)
	}
	if err := os.MkdirAll(procWatchDir(), 0o755); err != nil {
		return err
	}
	// The lock file stays in place on return: unlinking it races a watcher
	// that already opened it.
	unlock, err := fsutil.Lock(procWatchLockPath(owner), fsutil.Exclusive, 0)
	if errors.Is(err, fsutil.ErrLockTimeout) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unlock()
	procWatchExpire(owner, procWatchNow())
	// Physical memory does not change, so the limits are read once.
	limits := proctree.DefaultLimits()
	self := os.Getpid()
	for {
		t, err := procWatchSnapshot()
		if errors.Is(err, proctree.ErrUnsupported) {
			return err
		}
		if err == nil {
			if !slices.ContainsFunc(t, func(p proctree.Proc) bool { return p.PID == owner }) {
				return nil
			}
			for _, child := range proctree.Children(t, owner) {
				// The watcher never judges itself.
				if child == self {
					continue
				}
				u, ok := proctree.MeasureIn(t, child)
				if !ok {
					continue
				}
				if text, tripped := limits.Exceeded(u); tripped {
					procWatchTrip(owner, child, u, text)
				}
			}
		}
		procWatchSleep(proctree.DefaultInterval)
	}
}

// procWatchTrip writes the record and then kills the tree under child. The
// order is the point: guard-result runs only after the Bash call returned,
// which needs the command's processes dead, so the record is already there
// for every kill. child itself is spared and may trip again on a later tick.
func procWatchTrip(owner, child int, u proctree.Usage, text string) {
	// The directory may have been removed since the watcher started.
	if err := os.MkdirAll(procWatchDir(), 0o755); err == nil {
		rec := procWatchRecord{
			AtMS:     procWatchNow().UnixMilli(),
			Owner:    owner,
			Root:     child,
			Procs:    u.Procs,
			MemBytes: u.Mem,
			Reason:   text,
		}
		if data, err := fsutil.MarshalJSON(rec); err == nil {
			_ = fsutil.AtomicWriteFile(procWatchRecordPath(owner, child, rec.AtMS), data, 0o644)
		}
	}
	// A failed record write does not stop the kill: the trip still happened.
	_, _ = procWatchKill(owner, child)
}

// procWatchExpire removes the files older than procWatchMaxAge: records, old
// token records, temp leftovers and locks alike. It keeps this watcher's lock
// and any lock another watcher holds: a session older than a day keeps its
// watcher, and removing that lock would let `proc watch` start a second one.
// Errors are ignored: a stale file that survives one sweep is removed by the
// next.
func procWatchExpire(owner int, now time.Time) {
	dir := procWatchDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := now.Add(-procWatchMaxAge)
	own := filepath.Base(procWatchLockPath(owner))
	for _, e := range entries {
		if !e.Type().IsRegular() || e.Name() == own {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if strings.HasSuffix(e.Name(), ".lock") && procWatchLockHeld(path) {
			continue
		}
		_ = os.Remove(path)
	}
}
