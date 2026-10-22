package runner

import "golang.org/x/sys/unix"

// probeMemoryPressure reads the kernel's memory pressure level.
func probeMemoryPressure() (bool, string) {
	level, err := unix.SysctlUint32("kern.memorystatus_vm_pressure_level")
	if err != nil {
		return false, ""
	}
	return darwinPressure(level)
}
