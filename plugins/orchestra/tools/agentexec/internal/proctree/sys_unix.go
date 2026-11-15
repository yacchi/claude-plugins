//go:build !windows

package proctree

import "syscall"

// sigStop and sigCont freeze and resume one process.
const (
	sigStop = syscall.SIGSTOP
	sigCont = syscall.SIGCONT
)

func sysSignal(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }

func sysGetpgrp() int { return syscall.Getpgrp() }
