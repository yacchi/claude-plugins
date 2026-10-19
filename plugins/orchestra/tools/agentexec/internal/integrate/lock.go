package integrate

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
)

// Lock modes of Options.Lock.
const (
	LockWait = "wait"
	LockFail = "fail"
)

const lockPoll = 100 * time.Millisecond

// LockPath is the lock file of one integration target, keyed by the task id
// its tree is created under (isolate owns the layout: remove probes the same
// lock to leave a running integrate alone). "" when into has no usable task
// id, so integrate itself reports that.
func LockPath(repo, into string) string {
	task, err := isolate.SanitizeTaskID(into)
	if err != nil {
		return ""
	}
	return isolate.IntegrateLockPath(repo, task)
}

// heldLock is an acquired integrate lock; release closes it.
type heldLock struct{ f *os.File }

func (l *heldLock) release() {
	if l != nil && l.f != nil {
		_ = l.f.Close()
	}
}

// acquireLock flocks path exclusively, polling until timeout in wait mode
// (fail mode tries once). The holder's "pid timestamp" line is written into
// the file; on failure the current holder's line is returned instead.
func acquireLock(path, mode string, timeout time.Duration) (*heldLock, string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, "", err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, "", err
	}
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if mode == LockFail || !time.Now().Before(deadline) {
			holder := ""
			if b, rerr := io.ReadAll(f); rerr == nil {
				holder = strings.TrimSpace(string(b))
			}
			_ = f.Close()
			return nil, holder, nil
		}
		time.Sleep(lockPoll)
	}
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(fmt.Sprintf("%d %s\n", os.Getpid(), fsutil.NowISO())), 0)
	}
	return &heldLock{f: f}, "", nil
}
