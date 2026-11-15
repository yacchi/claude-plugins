//go:build !windows

package isolate

import "errors"

// copyLinkWindows is never reached off Windows: copyTree copies links as
// links there.
func copyLinkWindows(string, string, string, string, bool) error {
	return errors.ErrUnsupported
}
