//go:build !windows

package srt

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/sandbox"
)

// --- control FIFO ------------------------------------------------------------

// MakeFIFO creates a fresh control FIFO at path (dir 0700, FIFO 0600) and
// opens it read-write: srt reads it as its control fd and, holding a
// writer itself, never sees EOF when a pushing process closes its end.
func MakeFIFO(path string) (*os.File, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path); err == nil {
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	if err := unix.Mkfifo(path, 0o600); err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

// PushLine writes data (one full-config line, or nothing to only probe) to
// the control FIFO at path. It returns "" or why the channel cannot be
// used: missing, not a FIFO, or no reader (the srt behind it is gone). It
// never blocks on a FIFO nobody reads, and a line is written whole.
func PushLine(path string, data []byte) string {
	st, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "no control channel (" + path + ")"
	}
	if err != nil {
		return "control channel: " + err.Error()
	}
	if st.Mode()&os.ModeNamedPipe == 0 {
		return "control channel is not a FIFO (" + path + ")"
	}
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENXIO) {
			return "nothing reads the control channel (" + path + ")"
		}
		return "control channel: " + err.Error()
	}
	defer unix.Close(fd)
	// Blocking from here: a non-blocking write of a line longer than the
	// pipe buffer could stop half-way and leave srt a broken line.
	if err := unix.SetNonblock(fd, false); err != nil {
		return "control channel: " + err.Error()
	}
	for len(data) > 0 {
		n, err := unix.Write(fd, data)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return "control channel: " + err.Error()
		}
		data = data[n:]
	}
	return ""
}

// execControlFD puts /dev/null on the srt control fd for an exec.
func execControlFD() error {
	fd, err := unix.Open(os.DevNull, unix.O_RDONLY, 0) // inheritable on purpose
	if err != nil {
		return err
	}
	if fd == sandbox.SrtControlFD {
		return nil
	}
	defer unix.Close(fd)
	if _, err := unix.FcntlInt(uintptr(sandbox.SrtControlFD), unix.F_GETFD, 0); err == nil {
		return fmt.Errorf("srt: fd %d is in use; spawn srt with its control fd instead of exec", sandbox.SrtControlFD)
	}
	return unix.Dup2(fd, sandbox.SrtControlFD)
}
