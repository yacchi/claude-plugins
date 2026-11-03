//go:build darwin

package proctree

import (
	"encoding/binary"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func init() {
	Snapshot = sysDarwinSnapshot
	PhysicalMemory = sysDarwinPhysicalMemory
}

// sysDarwinSnapshot reads kern.proc.all, the sysctl the other readers use. It
// starts no process: a forking reader would fail exactly when a fork bomb has
// filled the table.
func sysDarwinSnapshot() (Table, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	t := make(Table, 0, len(procs))
	for i := range procs {
		p := &procs[i]
		pid := int(p.Proc.P_pid)
		t = append(t, Proc{
			PID:  pid,
			PPID: int(p.Eproc.Ppid),
			PGID: int(p.Eproc.Pgid),
			Mem:  sysDarwinFootprint(pid),
		})
	}
	return t, nil
}

// sysDarwinFootprint is the process's ri_phys_footprint from proc_pid_rusage,
// called raw so no cgo is needed. A failed call (EPERM for another user's
// process, ESRCH for one that exited) reads as 0 instead of failing the table.
func sysDarwinFootprint(pid int) int64 {
	var buf [96]byte // struct rusage_info_v0
	_, _, errno := syscall.Syscall6(syscall.SYS_PROC_INFO, 9 /* PROC_INFO_CALL_PIDRUSAGE */, uintptr(pid), 0 /* RUSAGE_INFO_V0 */, 0, uintptr(unsafe.Pointer(&buf[0])), 96)
	if errno != 0 {
		return 0
	}
	// Offset 64 is the resident size; the footprint includes compressed pages.
	return int64(binary.LittleEndian.Uint64(buf[72:80]))
}

// sysDarwinPhysicalMemory is hw.memsize, or 0 when unreadable.
func sysDarwinPhysicalMemory() int64 {
	v, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0
	}
	return int64(v)
}
