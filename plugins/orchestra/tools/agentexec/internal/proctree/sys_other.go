//go:build !darwin

package proctree

import (
	"os"
	"strconv"
	"strings"
)

func init() {
	// Without /proc (or on a platform that lacks it) both readers stay nil and
	// the guard is off, rather than failing on every tick.
	if _, err := os.ReadFile("/proc/self/stat"); err != nil {
		return
	}
	Snapshot = sysProcSnapshot
	PhysicalMemory = sysProcMemTotal
}

// sysProcSnapshot reads every numeric entry of /proc. It starts no process,
// for the same reason as the darwin reader.
func sysProcSnapshot() (Table, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	pageSize := int64(os.Getpagesize())
	var t Table
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		// A process that exits while being read is simply left out.
		raw, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		ppid, pgid, ok := sysProcParseStat(string(raw))
		if !ok {
			continue
		}
		t = append(t, Proc{
			PID:  pid,
			PPID: ppid,
			PGID: pgid,
			Mem:  sysProcResident(e.Name(), pageSize),
		})
	}
	return t, nil
}

// sysProcParseStat reads ppid and pgid from a /proc/<pid>/stat line. The
// command name sits in parentheses and may hold spaces or parentheses, so the
// fields start after the last ')'.
func sysProcParseStat(line string) (ppid, pgid int, ok bool) {
	i := strings.LastIndexByte(line, ')')
	if i < 0 {
		return 0, 0, false
	}
	fields := strings.Fields(line[i+1:])
	if len(fields) < 3 {
		return 0, 0, false
	}
	// fields[0] is the state.
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, false
	}
	pgid, err = strconv.Atoi(fields[2])
	if err != nil {
		return 0, 0, false
	}
	return ppid, pgid, true
}

// sysProcResident is the resident set size from /proc/<pid>/statm, in bytes;
// 0 when unreadable, so the pid is still listed.
func sysProcResident(pid string, pageSize int64) int64 {
	raw, err := os.ReadFile("/proc/" + pid + "/statm")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 2 {
		return 0
	}
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return pages * pageSize
}

// sysProcMemTotal is MemTotal from /proc/meminfo (kB) in bytes; 0 on error.
func sysProcMemTotal() int64 {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}
