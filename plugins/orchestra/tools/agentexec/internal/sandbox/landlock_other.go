//go:build !linux

package sandbox

import "errors"

// LandlockABI is 0 off Linux.
func LandlockABI() int { return 0 }

// landlockRestrict fails closed off Linux: the command is never run.
func landlockRestrict([]string) error {
	return errors.New("Landlock is not supported by this kernel")
}
