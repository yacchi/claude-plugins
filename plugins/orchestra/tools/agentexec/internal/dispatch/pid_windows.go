//go:build windows

package dispatch

import "golang.org/x/sys/windows"

// ownPIDAlive is kill(pid, 0) on Windows: a process this user can open and
// that has not exited. One it may not open (another user's) is not ours, so
// it reads as dead, as EPERM does on Unix.
func ownPIDAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	return windows.GetExitCodeProcess(h, &code) == nil && code == 259 // STILL_ACTIVE
}
