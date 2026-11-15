package herdr

import "runtime"

// unsupportedOS is why the herdr host cannot run on this OS, "" where it
// can: herdr is a Unix terminal multiplexer with no Windows build, and the
// host leans on unix sockets and Unix process sessions. Available reports
// it and Start refuses with it as a need. Tests clear it to drive the fake
// herdr, which runs anywhere.
var unsupportedOS = func() string {
	if runtime.GOOS == "windows" {
		return "herdr is not supported on Windows (herdr has no Windows build)"
	}
	return ""
}()
