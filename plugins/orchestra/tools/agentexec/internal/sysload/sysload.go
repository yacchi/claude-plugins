// Package sysload reads how loaded the machine is, without shelling out:
// the load averages, the OS memory pressure and how many git processes
// run. Every probe reports when it could not read anything, so a caller
// degrades that one value alone.
package sysload

import (
	"fmt"
	"strconv"
	"strings"
)

// Memory pressure levels.
const (
	Normal   = "normal"
	Warn     = "warn"
	Critical = "critical"
	Unknown  = "unknown"
)

// Pressure is the OS memory pressure: Level, and for warn or worse the
// Reason (the reading that put it there).
type Pressure struct {
	Level  string
	Reason string
}

// High reports warn or worse: the level that holds new starts.
func (p Pressure) High() bool { return p.Level == Warn || p.Level == Critical }

// darwinPressure grades kern.memorystatus_vm_pressure_level: 1 normal, 2
// warn, 4 critical.
func darwinPressure(level uint32) Pressure {
	switch {
	case level >= 4:
		return Pressure{Critical, "memory pressure critical (kern.memorystatus_vm_pressure_level 4)"}
	case level >= 2:
		return Pressure{Warn, fmt.Sprintf("memory pressure warn (kern.memorystatus_vm_pressure_level %d)", level)}
	}
	return Pressure{Level: Normal}
}

// linuxPressure grades /proc/pressure/memory's `some avg10`: 20 or more is
// warn, 50 or more critical. Without PSI it falls back to MemAvailable:
// below 10 % of MemTotal is warn, below 5 % critical. A nil input is a file
// that could not be read; nothing readable is unknown.
func linuxPressure(psi, meminfo []byte) Pressure {
	if psi != nil {
		for _, line := range strings.Split(string(psi), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 || fields[0] != "some" {
				continue
			}
			for _, f := range fields[1:] {
				if v, ok := strings.CutPrefix(f, "avg10="); ok {
					if avg, err := strconv.ParseFloat(v, 64); err == nil {
						reason := fmt.Sprintf("memory pressure (PSI some avg10 %.1f)", avg)
						switch {
						case avg >= 50:
							return Pressure{Critical, reason}
						case avg >= 20:
							return Pressure{Warn, reason}
						}
						return Pressure{Level: Normal}
					}
				}
			}
		}
	}
	if meminfo == nil {
		return Pressure{Level: Unknown}
	}
	kb := map[string]float64{}
	for _, line := range strings.Split(string(meminfo), "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if fields := strings.Fields(rest); len(fields) > 0 {
			if v, err := strconv.ParseFloat(fields[0], 64); err == nil {
				kb[name] = v
			}
		}
	}
	total, okT := kb["MemTotal"]
	avail, okA := kb["MemAvailable"]
	if !okT || !okA || total <= 0 {
		return Pressure{Level: Unknown}
	}
	reason := fmt.Sprintf("low memory (MemAvailable %.0f%% of MemTotal)", 100*avail/total)
	switch {
	case avail/total < 0.05:
		return Pressure{Critical, reason}
	case avail/total < 0.10:
		return Pressure{Warn, reason}
	}
	return Pressure{Level: Normal}
}

// parseLoadavg reads the first two fields of /proc/loadavg.
func parseLoadavg(raw string) (float64, float64, bool) {
	fields := strings.Fields(raw)
	if len(fields) < 2 {
		return 0, 0, false
	}
	l1, err1 := strconv.ParseFloat(fields[0], 64)
	l5, err5 := strconv.ParseFloat(fields[1], 64)
	if err1 != nil || err5 != nil {
		return 0, 0, false
	}
	return l1, l5, true
}
