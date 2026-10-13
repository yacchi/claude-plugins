// Package fsutil holds the file-system primitives every agent-exec package
// shares: atomic writes, advisory locks, JSON(L) files, PID liveness and the
// two timestamp formats agent-exec writes.
package fsutil

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// AtomicWriteFile writes data to a temp file in path's directory, fsyncs it
// and renames it over path, so readers see the old or the new content, never
// a torn write. The parent directory must already exist.
func AtomicWriteFile(path string, data []byte, perm fs.FileMode) error {
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	f, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	fail := func(err error) error {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Chmod(perm); err != nil {
		return fail(err)
	}
	if _, err := f.Write(data); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// AtomicWriteJSON writes v as indented JSON plus a trailing newline.
func AtomicWriteJSON(path string, v any, perm fs.FileMode) error {
	data, err := encodeJSON(v, true)
	if err != nil {
		return err
	}
	return AtomicWriteFile(path, append(data, '\n'), perm)
}

// EnsureDir creates path and its parents with perm (an existing directory is
// left as it is).
func EnsureDir(path string, perm fs.FileMode) error {
	return os.MkdirAll(path, perm)
}

// ReadJSONFile decodes the JSON file at path into v. Pass a *jsonx.Object to
// keep key order.
func ReadJSONFile(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// AppendJSONLine appends v as one compact JSON line, creating the file with
// perm if needed. The line is written with a single write(2) on an O_APPEND
// descriptor, so concurrent appenders never interleave within a line.
func AppendJSONLine(path string, v any, perm fs.FileMode) error {
	data, err := encodeJSON(v, false)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func encodeJSON(v any, indent bool) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if indent {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// PIDAlive reports whether a process with this pid exists (a process owned
// by another user counts as alive).
func PIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// TailLines returns the last n lines of s (all of s when it has fewer).
// A trailing newline does not count as an extra empty line.
func TailLines(s string, n int) string {
	if n <= 0 {
		return ""
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// ExpandUser replaces a leading "~" or "~/" with $HOME, as Python's
// os.path.expanduser does; any other path is returned unchanged.
func ExpandUser(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return home + p[1:]
}

// NowISO is Python's datetime.now(timezone.utc).isoformat():
// 2026-10-13T08:00:00.123456+00:00 (the fraction is omitted when zero).
func NowISO() string {
	return FormatISO(time.Now())
}

// FormatISO renders t in UTC the way NowISO does.
func FormatISO(t time.Time) string {
	t = t.UTC()
	s := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	return s + "+00:00"
}

// NowISOZ is the second-precision form: 2026-10-13T08:00:00Z.
func NowISOZ() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z")
}
