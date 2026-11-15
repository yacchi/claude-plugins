//go:build windows

package fsutil

import (
	"errors"
	"io"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// shareRetry bounds the wait for another process to let go of a file. Readers
// and writers hold it for milliseconds; anything longer is a real failure.
const shareRetry = 2 * time.Second

func sharingBusy(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

// ReadFile reads path opened with FILE_SHARE_DELETE, so an AtomicWriteFile
// renaming a new version over it never fails because of this reader (os.Open
// does not share delete). A file another process holds unshared is retried
// for a short while.
func ReadFile(path string) ([]byte, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(shareRetry)
	for {
		h, err := windows.CreateFile(p, windows.GENERIC_READ,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if err == nil {
			f := os.NewFile(uintptr(h), path)
			data, rerr := io.ReadAll(f)
			f.Close()
			return data, rerr
		}
		if !sharingBusy(err) || time.Now().After(deadline) {
			if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
				return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrNotExist}
			}
			return nil, &os.PathError{Op: "open", Path: path, Err: err}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// renameReplace is os.Rename, retried for a short while when another process
// has the target open without FILE_SHARE_DELETE (a reader using plain
// os.Open, an editor, an antivirus scan).
func renameReplace(oldpath, newpath string) error {
	deadline := time.Now().Add(shareRetry)
	for {
		err := os.Rename(oldpath, newpath)
		if err == nil || !sharingBusy(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}
