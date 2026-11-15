//go:build !windows

package procx

import (
	"os/exec"
	"syscall"
)

// ShellPath is the POSIX shell commands run through.
func ShellPath() string { return "/bin/sh" }

func shellScript(script string) string { return script }

// Detach makes cmd the leader of a new session (setsid), out of the caller's
// process group and controlling terminal, so KillGroup(cmd.Process.Pid, …)
// reaches it and everything it starts.
func Detach(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}

// KillGroup signals the process group pgid (a Detach'ed child's pid):
// SIGKILL when hard, else SIGTERM.
func KillGroup(pgid int, hard bool) error {
	sig := syscall.SIGTERM
	if hard {
		sig = syscall.SIGKILL
	}
	return syscall.Kill(-pgid, sig)
}

// Detached reports whether Detach was applied to cmd.
func Detached(cmd *exec.Cmd) bool { return cmd.SysProcAttr != nil && cmd.SysProcAttr.Setsid }
