package proctree

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

// The platform readers are not parallel-safe with proctree-core's seam
// swaps, so none of these tests call t.Parallel.

func TestSysSnapshotSelf(t *testing.T) {
	if Snapshot == nil {
		t.Skip("no process table reader on this platform")
	}
	tab, err := Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	var self *Proc
	for i := range tab {
		if tab[i].PID == os.Getpid() {
			self = &tab[i]
			break
		}
	}
	if self == nil {
		t.Fatalf("no row for this process (pid %d)", os.Getpid())
	}
	if self.PPID != os.Getppid() {
		t.Errorf("PPID = %d, want %d", self.PPID, os.Getppid())
	}
	if self.PGID != syscall.Getpgrp() {
		t.Errorf("PGID = %d, want %d", self.PGID, syscall.Getpgrp())
	}
	// A Go test binary holds well over 1 MiB; the upper bound catches a
	// KiB/page unit mixup on the way to bytes.
	if phys := PhysicalMemory(); self.Mem < 1<<20 || self.Mem > phys {
		t.Errorf("Mem = %d, want within [1 MiB, %d]", self.Mem, phys)
	}
}

func TestSysSnapshotForeign(t *testing.T) {
	if Snapshot == nil {
		t.Skip("no process table reader on this platform")
	}
	tab, err := Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	// pid 1 belongs to another user: its per-process read fails, which must
	// not fail the table.
	for _, p := range tab {
		if p.PID == 1 {
			if p.Mem < 0 {
				t.Errorf("pid 1 Mem = %d, want >= 0", p.Mem)
			}
			return
		}
	}
	t.Fatal("no row for pid 1")
}

func TestSysSnapshotChild(t *testing.T) {
	if Snapshot == nil {
		t.Skip("no process table reader on this platform")
	}
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not on PATH")
	}
	// The name holds a space and parentheses, which the stat parser must
	// survive.
	link := filepath.Join(t.TempDir(), "sys) (sleep")
	if err := os.Symlink(sleep, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	cmd := exec.Command(link, "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	tab, err := Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	for _, p := range tab {
		if p.PID == cmd.Process.Pid {
			if p.PPID != os.Getpid() {
				t.Errorf("child PPID = %d, want %d", p.PPID, os.Getpid())
			}
			return
		}
	}
	t.Fatalf("no row for child pid %d", cmd.Process.Pid)
}

func TestSysPhysicalMemory(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("physical memory reader is darwin and linux only")
	}
	if PhysicalMemory == nil {
		t.Fatal("PhysicalMemory is nil on " + runtime.GOOS)
	}
	if n := PhysicalMemory(); n <= 0 {
		t.Errorf("PhysicalMemory() = %d, want > 0", n)
	}
}
