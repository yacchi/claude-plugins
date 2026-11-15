//go:build windows

package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// Flock modes for Flock: one of LockSh, LockEx or LockUn, optionally or'ed
// with LockNB. Windows has no flock(2); these are this package's own values.
const (
	LockSh = 1
	LockEx = 2
	LockNB = 4
	LockUn = 8
)

// lockOffset is where the locked byte range starts. LockFileEx locks are
// mandatory: a locked range cannot be read or written through any other
// handle. Locking one byte far past any real content keeps the lock purely
// advisory, so a holder line written into a lock file stays readable by the
// process that failed to take it (a range past EOF may be locked).
const lockOffset = 1 << 62

// Flock emulates flock(2) on f with LockFileEx on one byte at lockOffset. A
// busy lock under LockNB is syscall.EWOULDBLOCK, as on Unix. f may be opened
// read-only.
func Flock(f *os.File, how int) error {
	h := windows.Handle(f.Fd())
	ol := &windows.Overlapped{Offset: uint32(lockOffset & 0xffffffff), OffsetHigh: uint32(lockOffset >> 32)}
	if how&LockUn != 0 {
		err := windows.UnlockFileEx(h, 0, 1, 0, ol)
		if errors.Is(err, windows.ERROR_NOT_LOCKED) {
			return nil
		}
		return err
	}
	var flags uint32
	if how&LockEx != 0 {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	if how&LockNB != 0 {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	err := windows.LockFileEx(h, flags, 0, 1, 0, ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return syscall.EWOULDBLOCK
	}
	return err
}

// PIDAlive reports whether a process with this pid exists and has not
// exited (a process this user may not open counts as alive).
func PIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == 259 // STILL_ACTIVE
}

// OpenNoFollow opens path read-only and fails when its last element is a
// symbolic link (or another reparse point).
func OpenNoFollow(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return nil, &os.PathError{Op: "open", Path: path, Err: syscall.ELOOP}
	}
	return os.Open(path)
}

// OwnedBy is always true on Windows: there is no numeric uid, and the
// per-user directories it guards already sit under the user's profile.
func OwnedBy(info os.FileInfo, uid int) bool { return true }

// Executable reports whether path looks runnable: a regular file whose
// extension is listed in PATHEXT, or a file without an extension (a script
// run through sh).
func Executable(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == "" {
		return true
	}
	pathext := os.Getenv("PATHEXT")
	if pathext == "" {
		pathext = ".com;.exe;.bat;.cmd"
	}
	for _, e := range strings.Split(strings.ToLower(pathext), ";") {
		if e == ext {
			return true
		}
	}
	return false
}

// Writable reports whether path is not read-only.
func Writable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().Perm()&0o200 != 0
}
