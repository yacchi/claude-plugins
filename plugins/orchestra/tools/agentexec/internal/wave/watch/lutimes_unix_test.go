//go:build !windows

package watch

import (
	"time"

	"golang.org/x/sys/unix"
)

// lutimes stamps the symlink at path itself, not its target.
func lutimes(path string, mtime time.Time) error {
	tv := unix.NsecToTimeval(mtime.UnixNano())
	return unix.Lutimes(path, []unix.Timeval{tv, tv})
}
