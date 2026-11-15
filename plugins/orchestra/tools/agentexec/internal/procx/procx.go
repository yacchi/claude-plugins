// Package procx hides the process primitives that differ between Unix and
// Windows: starting a child in its own session or process group, killing
// that group, and running a POSIX shell command line.
package procx

import (
	"context"
	"os/exec"
)

// Shell is the command for `sh -c script`: /bin/sh on Unix, the POSIX sh of
// Git for Windows on Windows (see ShellPath). ctx nil means no context.
func Shell(ctx context.Context, script string) *exec.Cmd {
	sh := ShellPath()
	script = shellScript(script)
	if ctx == nil {
		return exec.Command(sh, "-c", script)
	}
	return exec.CommandContext(ctx, sh, "-c", script)
}
