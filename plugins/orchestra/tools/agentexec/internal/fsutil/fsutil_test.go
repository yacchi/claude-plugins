package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLockExclusiveTimesOutWhileHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	unlock, err := Lock(path, Exclusive, 0)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := Lock(path, Shared, 150*time.Millisecond); !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("second lock: err = %v, want ErrLockTimeout", err)
	}
	if waited := time.Since(start); waited < 100*time.Millisecond {
		t.Fatalf("gave up after %v, before the timeout", waited)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	unlock2, err := Lock(path, Exclusive, 0)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	unlock2()
}

func TestLockSharedHoldersCoexist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	u1, err := Lock(path, Shared, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer u1()
	u2, err := Lock(path, Shared, 0)
	if err != nil {
		t.Fatalf("second shared lock: %v", err)
	}
	u2()
}

func TestAtomicWriteFileReplacesAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWriteFile(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "new" {
		t.Fatalf("content = %q", got)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %v", st.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("leftover files: %v", entries)
	}
}

func TestAtomicWriteFileMissingDirFailsCleanly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent", "f")
	if err := AtomicWriteFile(path, []byte("x"), 0o600); err == nil {
		t.Fatal("want error for a missing parent directory")
	}
}

func TestFormatISOMatchesPythonIsoformat(t *testing.T) {
	ts := time.Date(2026, 10, 13, 8, 0, 0, 123456789, time.UTC)
	if got := FormatISO(ts); got != "2026-10-13T08:00:00.123456+00:00" {
		t.Fatal(got)
	}
	if got := FormatISO(ts.Truncate(time.Second)); got != "2026-10-13T08:00:00+00:00" {
		t.Fatal(got)
	}
}
