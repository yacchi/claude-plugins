package sysload

import (
	"os"
	"path/filepath"
	"strings"
)

// Load is the one- and five-minute load averages from /proc/loadavg; ok is
// false when unreadable.
func Load() (load1, load5 float64, ok bool) {
	raw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, 0, false
	}
	return parseLoadavg(string(raw))
}

// MemoryPressure reads PSI, else /proc/meminfo.
func MemoryPressure() Pressure {
	psi, err := os.ReadFile("/proc/pressure/memory")
	if err != nil {
		psi = nil
	}
	meminfo, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		meminfo = nil
	}
	return linuxPressure(psi, meminfo)
}

// GitProcs counts the /proc/<pid>/comm that read git; -1 when /proc lists
// no process.
func GitProcs() int {
	comms, err := filepath.Glob("/proc/[0-9]*/comm")
	if err != nil || len(comms) == 0 {
		return -1
	}
	n := 0
	for _, c := range comms {
		if raw, err := os.ReadFile(c); err == nil && strings.TrimSpace(string(raw)) == "git" {
			n++
		}
	}
	return n
}
