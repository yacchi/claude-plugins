//go:build !darwin && !linux

package runner

// probeMemoryPressure has no probe off darwin/linux: starts are never held.
func probeMemoryPressure() (bool, string) { return false, "" }
