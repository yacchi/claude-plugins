//go:build !darwin && !linux

package sysload

// Load has no probe off darwin/linux.
func Load() (load1, load5 float64, ok bool) { return 0, 0, false }

// MemoryPressure has no probe off darwin/linux: unknown, never pressure.
func MemoryPressure() Pressure { return Pressure{Level: Unknown} }

// GitProcs has no probe off darwin/linux.
func GitProcs() int { return -1 }
