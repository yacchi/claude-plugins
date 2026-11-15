//go:build windows

package watch

import (
	"time"

	"golang.org/x/sys/windows"
)

// lutimes stamps the symlink at path itself, not its target.
func lutimes(path string, mtime time.Time) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(p, windows.FILE_WRITE_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	ft := windows.NsecToFiletime(mtime.UnixNano())
	return windows.SetFileTime(h, nil, &ft, &ft)
}
