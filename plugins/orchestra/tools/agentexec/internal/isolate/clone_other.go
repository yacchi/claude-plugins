//go:build !darwin

package isolate

import "errors"

// cloneDir has no whole-tree clone off macOS; cp --reflink takes over.
func cloneDir(string, string) error {
	return errors.ErrUnsupported
}
