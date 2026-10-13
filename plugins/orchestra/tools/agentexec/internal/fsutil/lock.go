package fsutil

import (
	"errors"
	"os"
	"syscall"
	"time"
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
	if timeout < 0 {
		if err := flockRetry(fd, how); err != nil {
			f.Close()
			return nil, err
		}
		return release(f), nil
	}
	deadline := time.Now().Add(timeout)
	for {
		err := flockRetry(fd, how|syscall.LOCK_NB)
		if err == nil {
			return release(f), nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			f.Close()
			return nil, ErrLockTimeout
		}
		time.Sleep(min(lockPoll, remaining))
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
