package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/trace"
)

type LockMode int

const (
	Shared LockMode = iota
	Exclusive
)

// ErrLockTimeout is returned by Lock when the lock stayed busy for the whole
// timeout.
var ErrLockTimeout = errors.New("lock timeout")

const lockPoll = 50 * time.Millisecond

// minTracedWait is the shortest busy wait on a lock that is written to the
// trace log.
const minTracedWait = 50 * time.Millisecond

// Lock takes a flock(2) on path (created 0600 if missing). timeout 0 tries
// once; a negative timeout waits indefinitely. unlock releases the lock and
// closes the descriptor.
func Lock(path string, mode LockMode, timeout time.Duration) (unlock func() error, err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	how := syscall.LOCK_SH
	if mode == Exclusive {
		how = syscall.LOCK_EX
	}
	fd := int(f.Fd())
	start := time.Now()
	deadline := start.Add(timeout)
	err = flockRetry(fd, how|syscall.LOCK_NB)
	if err == nil {
		return release(f), nil
	}
	if !errors.Is(err, syscall.EWOULDBLOCK) {
		f.Close()
		return nil, err
	}
	// Only a wait that found the lock busy and lasted at least minTracedWait
	// is traced: an uncontended lock is taken constantly and short waits
	// would flood the log.
	end := trace.Begin(trace.KindLock, "flock "+filepath.Base(path), trace.Attrs{Detail: path})
	done := func(code int) {
		if time.Since(start) >= minTracedWait {
			end(code)
		}
	}
	if timeout < 0 {
		if err := flockRetry(fd, how); err != nil {
			done(1)
			f.Close()
			return nil, err
		}
		done(0)
		return release(f), nil
	}
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			done(1)
			f.Close()
			return nil, ErrLockTimeout
		}
		time.Sleep(min(lockPoll, remaining))
		err := flockRetry(fd, how|syscall.LOCK_NB)
		if err == nil {
			done(0)
			return release(f), nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			done(1)
			f.Close()
			return nil, err
		}
	}
}

func flockRetry(fd, how int) error {
	for {
		err := syscall.Flock(fd, how)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

func release(f *os.File) func() error {
	return func() error {
		uerr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		cerr := f.Close()
		if uerr != nil {
			return uerr
		}
		return cerr
	}
}
