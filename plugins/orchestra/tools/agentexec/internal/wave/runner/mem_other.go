//go:build !darwin && !linux

package runner

// physicalMemory is unknown off darwin/linux: the default width applies.
func physicalMemory() uint64 { return 0 }
