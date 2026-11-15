//go:build windows

package proctree

import (
	"errors"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows has no signals and no process groups. sigStop and sigCont map to
// NtSuspendProcess / NtResumeProcess (the freeze before the kill still holds
// a forking tree still), SIGKILL to TerminateProcess; every other signal is
// unsupported. Each process is its own group (PGID = PID), so the group
// closure of coreTree adds nothing and only this process is protected as
// "our group".
const (
	sigStop = syscall.Signal(0x13)
	sigCont = syscall.Signal(0x12)
)

var (
	ntdll             = windows.NewLazySystemDLL("ntdll.dll")
	procNtSuspend     = ntdll.NewProc("NtSuspendProcess")
	procNtResume      = ntdll.NewProc("NtResumeProcess")
	kernel32          = windows.NewLazySystemDLL("kernel32.dll")
	procMemInfo       = kernel32.NewProc("K32GetProcessMemoryInfo")
	procGlobalMemStat = kernel32.NewProc("GlobalMemoryStatusEx")
)

func init() {
	Snapshot = sysWinSnapshot
	PhysicalMemory = sysWinPhysicalMemory
}

func sysSignal(pid int, sig syscall.Signal) error {
	var access uint32
	switch sig {
	case syscall.SIGKILL:
		access = windows.PROCESS_TERMINATE
	case sigStop, sigCont:
		access = windows.PROCESS_SUSPEND_RESUME
	default:
		return syscall.EWINDOWS
	}
	h, err := windows.OpenProcess(access, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return syscall.ESRCH // no such pid
		}
		return err
	}
	defer windows.CloseHandle(h)
	switch sig {
	case syscall.SIGKILL:
		return windows.TerminateProcess(h, 1)
	case sigStop:
		return ntStatus(procNtSuspend.Call(uintptr(h)))
	default:
		return ntStatus(procNtResume.Call(uintptr(h)))
	}
}

func ntStatus(r uintptr, _ uintptr, callErr error) error {
	if r != 0 {
		return windows.NTStatus(r)
	}
	return nil
}

func sysGetpgrp() int { return os.Getpid() }

// winProc is one Toolhelp entry plus its creation time (0: unknown).
type winProc struct {
	Proc
	start int64
}

// sysWinSnapshot walks a Toolhelp snapshot. Windows reuses pids and keeps a
// child's parent pid after the parent exits, so a parent pid is kept only
// when that parent is known to have started before the child; otherwise the
// process is listed as an orphan (PPID 0) and no unrelated process is ever
// pulled into a tree by a recycled pid.
func sysWinSnapshot() (Table, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	var procs []winProc
	byPID := map[int]int{}
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		pid := int(e.ProcessID)
		start, mem := sysWinQuery(e.ProcessID)
		byPID[pid] = len(procs)
		procs = append(procs, winProc{Proc: Proc{PID: pid, PPID: int(e.ParentProcessID), PGID: pid, Mem: mem}, start: start})
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, err
	}
	t := make(Table, 0, len(procs))
	for _, p := range procs {
		if i, ok := byPID[p.PPID]; !ok || p.start == 0 || procs[i].start == 0 || procs[i].start > p.start {
			p.PPID = 0
		}
		t = append(t, p.Proc)
	}
	return t, nil
}

// processMemoryCounters is PROCESS_MEMORY_COUNTERS.
type processMemoryCounters struct {
	cb                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

// sysWinQuery is pid's creation time (100ns ticks) and working set in bytes;
// 0 for whatever this user may not read.
func sysWinQuery(pid uint32) (start, mem int64) {
	if pid == 0 {
		return 0, 0
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return 0, 0
	}
	defer windows.CloseHandle(h)
	var c, x, k, u windows.Filetime
	if windows.GetProcessTimes(h, &c, &x, &k, &u) == nil {
		start = c.Nanoseconds()
	}
	var pmc processMemoryCounters
	pmc.cb = uint32(unsafe.Sizeof(pmc))
	if r, _, _ := procMemInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&pmc)), uintptr(pmc.cb)); r != 0 {
		mem = int64(pmc.WorkingSetSize)
	}
	return start, mem
}

// memoryStatusEx is MEMORYSTATUSEX.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func sysWinPhysicalMemory() int64 {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	if r, _, _ := procGlobalMemStat.Call(uintptr(unsafe.Pointer(&m))); r == 0 {
		return 0
	}
	return int64(m.TotalPhys)
}
