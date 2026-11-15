//go:build !windows

package watch

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
)

// processAgeOS reads the age from ps, gone for a zombie or a pid ps does
// not list; known is false when ps could not tell.
func processAgeOS(pid int) (age float64, gone, known bool) {
	out, err := exec.Command("ps", "-o", "stat=,etime=", "-p", strconv.Itoa(pid)).Output()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return 0, false, false
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return 0, true, true
	}
	secs, ok := parseEtime(fields[1])
	if !ok {
		return 0, false, false
	}
	return secs, strings.HasPrefix(fields[0], "Z"), true
}
