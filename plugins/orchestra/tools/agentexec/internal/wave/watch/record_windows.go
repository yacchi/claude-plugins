//go:build windows

package watch

import (
	"errors"
	"time"

	"golang.org/x/sys/windows"
)

// stillActive is GetExitCodeProcess's code for a process still running.
const stillActive = 259

// processAgeOS reads the age from the process's creation time, gone when
// the pid names no process or one that has exited (Windows has no zombies,
// but an exited process lives on while a handle holds it); known is false
// when the process could not be opened for another reason.
func processAgeOS(pid int) (age float64, gone, known bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return 0, true, true
		}
		return 0, false, false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if windows.GetExitCodeProcess(h, &code) == nil && code != stillActive {
		return 0, true, true
	}
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(h, &created, &exited, &kernel, &user) != nil {
		return 0, false, false
	}
	return time.Since(time.Unix(0, created.Nanoseconds())).Seconds(), false, true
}
