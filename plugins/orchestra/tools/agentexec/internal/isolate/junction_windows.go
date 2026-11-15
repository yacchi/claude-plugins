package isolate

import (
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// copyLinkWindows copies the link at path (src's tree, copied to dst) to
// target: a symlink as a symlink when this process may create one. Else a
// directory link (a junction, as pnpm makes, or a symlink) becomes
// a junction, which needs no privilege; one pointing inside src is pointed
// at the same place inside dst, so the copy stands on its own. A file link
// is copied as the file when this process may not create a symlink. A
// dangling link carries nothing.
func copyLinkWindows(src, dst, path, target string, symlink bool) error {
	link, err := os.Readlink(path)
	if err != nil {
		if symlink {
			return err
		}
		return copyFile(path, target) // another kind of reparse point
	}
	if symlink && os.Symlink(link, target) == nil {
		return nil
	}
	resolved := link
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(filepath.Dir(path), link)
	}
	st, err := os.Stat(resolved)
	if err != nil {
		return nil
	}
	if !st.IsDir() {
		return copyFile(resolved, target)
	}
	if rel, err := filepath.Rel(src, resolved); err == nil && rel != ".." &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		resolved = filepath.Join(dst, rel)
	}
	return makeJunction(target, resolved)
}

// makeJunction creates the directory junction link pointing at the
// absolute directory dir (which need not exist yet).
func makeJunction(link, dir string) error {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	subst, err := windows.UTF16FromString(`\??\` + dir)
	if err != nil {
		return err
	}
	print, err := windows.UTF16FromString(dir)
	if err != nil {
		return err
	}
	// REPARSE_DATA_BUFFER for a mount point: tag, data length, reserved,
	// then the substitute and print name offsets and lengths (bytes,
	// without the NULs) and both names, NUL-terminated.
	pathBuf := append(append([]uint16{}, subst...), print...)
	header := []uint16{
		0, uint16((len(subst) - 1) * 2),
		uint16(len(subst) * 2), uint16((len(print) - 1) * 2),
	}
	data := append(header, pathBuf...)
	buf := make([]byte, 8+len(data)*2)
	*(*uint32)(unsafe.Pointer(&buf[0])) = windows.IO_REPARSE_TAG_MOUNT_POINT
	*(*uint16)(unsafe.Pointer(&buf[4])) = uint16(len(data) * 2)
	for i, v := range data {
		*(*uint16)(unsafe.Pointer(&buf[8+2*i])) = v
	}

	if err := os.Mkdir(link, 0o755); err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(link)
	if err != nil {
		_ = os.Remove(link)
		return err
	}
	h, err := windows.CreateFile(name, windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		_ = os.Remove(link)
		return err
	}
	var n uint32
	err = windows.DeviceIoControl(h, windows.FSCTL_SET_REPARSE_POINT, &buf[0], uint32(len(buf)), nil, 0, &n, nil)
	// Closed before any removal: Windows cannot delete a directory with an
	// open handle.
	_ = windows.CloseHandle(h)
	if err != nil {
		_ = os.Remove(link)
		return &os.LinkError{Op: "junction", Old: dir, New: link, Err: err}
	}
	return nil
}
