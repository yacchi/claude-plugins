package runner

import "os"

// probeMemoryPressure reads PSI, else /proc/meminfo.
func probeMemoryPressure() (bool, string) {
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
