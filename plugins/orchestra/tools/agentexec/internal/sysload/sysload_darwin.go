package sysload

import (
	"bytes"
	"encoding/binary"

	"golang.org/x/sys/unix"
)

// Load is the one- and five-minute load averages from vm.loadavg (struct
// loadavg { fixpt_t ldavg[3]; long fscale; }); ok is false when unreadable.
func Load() (load1, load5 float64, ok bool) {
	raw, err := unix.SysctlRaw("vm.loadavg")
	if err != nil || len(raw) < 24 {
		return 0, 0, false
	}
	scale := float64(binary.LittleEndian.Uint64(raw[16:24]))
	if scale == 0 {
		return 0, 0, false
	}
	return float64(binary.LittleEndian.Uint32(raw[0:4])) / scale,
		float64(binary.LittleEndian.Uint32(raw[4:8])) / scale, true
}

// MemoryPressure reads kern.memorystatus_vm_pressure_level and, to tell a
// real shortage from a stale warn, kern.memorystatus_level (percent free).
func MemoryPressure() Pressure {
	level, err := unix.SysctlUint32("kern.memorystatus_vm_pressure_level")
	if err != nil {
		return Pressure{Level: Unknown}
	}
	freePct := -1
	if free, err := unix.SysctlUint32("kern.memorystatus_level"); err == nil {
		freePct = int(free)
	}
	return darwinPressure(level, freePct)
}

// GitProcs counts the processes named git in kern.proc.all; -1 when the
// process table is unreadable.
func GitProcs() int {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return -1
	}
	n := 0
	for i := range procs {
		comm := procs[i].Proc.P_comm[:]
		if j := bytes.IndexByte(comm, 0); j >= 0 {
			comm = comm[:j]
		}
		if string(comm) == "git" {
			n++
		}
	}
	return n
}
