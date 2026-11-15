//go:build !windows

package fsutil

import (
	"os"
	"syscall"
)

// Flock modes for Flock: one of LockSh, LockEx or LockUn, optionally or'ed
// with LockNB. They are flock(2)'s own values here.
const (
	LockSh = syscall.LOCK_SH
	LockEx = syscall.LOCK_EX
	LockNB = syscall.LOCK_NB
	LockUn = syscall.LOCK_UN
)

// Flock is flock(2) on f. A busy lock under LockNB is syscall.EWOULDBLOCK and
// an interrupted call is retried.
func Flock(f *os.File, how int) error {
	return flockRetry(int(f.Fd()), how)
}

func flockRetry(fd, how int) error {
	for {
		err := syscall.Flock(fd, how)
		if err != syscall.EINTR {
			return err
		}
	}
}

// PIDAlive reports whether a process with this pid exists (a process owned
// by another user counts as alive).
func PIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// OpenNoFollow opens path read-only and fails when its last element is a
// symbolic link.
func OpenNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

// OwnedBy reports whether info (from os.Lstat) belongs to uid.
func OwnedBy(info os.FileInfo, uid int) bool {
	st, _ := info.Sys().(*syscall.Stat_t)
	return st != nil && int(st.Uid) == uid
}

// Executable reports whether the current user may execute path.
func Executable(path string) bool {
	return syscall.Access(path, 1) == nil
}

// Writable reports whether the current user may write path.
func Writable(path string) bool {
	return syscall.Access(path, 2) == nil
}

// ReadFile is os.ReadFile. (Windows needs a read that never blocks a
// concurrent AtomicWriteFile over the same path.)
func ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func renameReplace(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }
