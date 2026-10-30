package config

import (
	"runtime"
	"strings"
)

// The machine-wide admission pools (see package slot): PoolTest admits test,
// lint and typecheck runs, PoolHeavy every other heavy command.
const (
	PoolHeavy = "heavy"
	PoolTest  = "test"
)

// CPUCount is runtime.NumCPU, a variable so that tests can name a machine.
var CPUCount = runtime.NumCPU

// AutoSize is the heavy pool's `auto` size: a third of the cores, at least 2
// (14 cores -> 5).
func AutoSize(cpus int) int {
	return max(2, (cpus+1)/3)
}

// TestAutoSize is the test pool's `auto` size: half the heavy pool's, at
// least 1 (14 cores -> 2).
func TestAutoSize(cpus int) int {
	return max(1, AutoSize(cpus)/2)
}

// SizeKey is the `throttle` key that sizes the pool name: `test_slots` for
// PoolTest, else `heavy_slots`.
func SizeKey(name string) string {
	if name == PoolTest {
		return "test_slots"
	}
	return "heavy_slots"
}

// PoolSize is the size of the machine-wide pool name (PoolHeavy or PoolTest)
// as its SizeKey in cfg reads (nil: the `auto` size); 0 is off. This is the
// one place a pool's size is read: the slot pools and the Node CPU cap
// (NodeCapEntries) both size from it.
func PoolSize(cfg *Resolved, name string) int {
	auto := AutoSize(CPUCount())
	if name == PoolTest {
		auto = TestAutoSize(CPUCount())
	}
	v, _ := cfg.Lookup("throttle", SizeKey(name))
	return sizeOf(v, auto)
}

// sizeOf reads a `throttle.heavy_slots` / `test_slots` value: a
// non-negative integer is used as is (0 = off), "off" or false is 0,
// anything else (`auto`, unset) is auto.
func sizeOf(v any, auto int) int {
	switch x := v.(type) {
	case int:
		if x >= 0 {
			return x
		}
	case int64:
		if x >= 0 {
			return int(x)
		}
	case float64:
		if x >= 0 && x == float64(int64(x)) {
			return int(x)
		}
	case bool:
		if !x {
			return 0
		}
	case string:
		if strings.EqualFold(strings.TrimSpace(x), "off") {
			return 0
		}
	}
	return auto
}
