package runner

import "golang.org/x/sys/unix"

// physicalMemory is the machine's RAM in bytes (0 when unreadable).
func physicalMemory() uint64 {
	n, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0
	}
	return n
}
