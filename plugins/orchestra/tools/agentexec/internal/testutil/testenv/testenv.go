// Package testenv holds the test helpers that differ between Unix and
// Windows: symbolic links, executable names and the POSIX shell.
package testenv

import (
	"os"
	"runtime"
	"sync"
	"testing"
)

var symlinkErr = sync.OnceValue(func() error {
	dir, err := os.MkdirTemp("", "testenv-symlink-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	return os.Symlink(dir, dir+string(os.PathSeparator)+"link")
})

// CanSymlink reports whether this process may create symbolic links (on
// Windows, only with Developer Mode or an elevated token).
func CanSymlink() bool { return symlinkErr() == nil }

// MustSymlink skips t when this process may not create symbolic links.
func MustSymlink(t testing.TB) {
	t.Helper()
	if err := symlinkErr(); err != nil {
		t.Skipf("symbolic links are not available: %v", err)
	}
}

// Symlink is os.Symlink that skips t, rather than failing it, when this
// process may not create symbolic links.
func Symlink(t testing.TB, oldname, newname string) {
	t.Helper()
	MustSymlink(t)
	if err := os.Symlink(oldname, newname); err != nil {
		t.Fatalf("symlink: %v", err)
	}
}

// Exe is name with the platform's executable suffix (".exe" on Windows).
func Exe(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// SkipOnWindows skips t on Windows, naming why the test is Unix-only.
func SkipOnWindows(t testing.TB, why string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("unix only: " + why)
	}
}
