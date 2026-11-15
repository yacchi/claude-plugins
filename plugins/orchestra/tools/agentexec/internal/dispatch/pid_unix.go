//go:build !windows

package dispatch

import "syscall"

func ownPIDAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }
