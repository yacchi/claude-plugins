//go:build !darwin

package trace

import (
	"os"
	"strconv"
	"strings"
)

// load1 is the one-minute load average from /proc/loadavg (0 when
// unreadable).
func load1() float64 {
	raw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(fields[0], 64)
	return v
}
