package trace

import (
	"encoding/binary"

	"golang.org/x/sys/unix"
)

// load1 is the one-minute load average (0 when unreadable). vm.loadavg is
// struct loadavg { fixpt_t ldavg[3]; long fscale; }.
func load1() float64 {
	raw, err := unix.SysctlRaw("vm.loadavg")
	if err != nil || len(raw) < 24 {
		return 0
	}
	ld := binary.LittleEndian.Uint32(raw[0:4])
	scale := binary.LittleEndian.Uint64(raw[16:24])
	if scale == 0 {
		return 0
	}
	return float64(ld) / float64(scale)
}
