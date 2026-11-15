//go:build windows

package srt

import (
	"errors"
	"os"
)

// srt (sandbox-runtime) runs on macOS and Linux only; Windows never resolves
// an srt backend, so its control channel does not exist there.
var errNoFIFO = errors.New("srt: control FIFO is not supported on Windows")

// MakeFIFO is unsupported on Windows.
func MakeFIFO(path string) (*os.File, error) { return nil, errNoFIFO }

// PushLine reports that Windows has no control channel.
func PushLine(path string, data []byte) string {
	return "no control channel on Windows (" + path + ")"
}

func execControlFD() error { return errNoFIFO }
