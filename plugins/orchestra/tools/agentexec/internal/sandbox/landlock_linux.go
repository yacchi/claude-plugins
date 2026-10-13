//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// LandlockABI is the kernel's Landlock ABI version, or 0 when unsupported
// or disabled.
func LandlockABI() int {
	abi, _, errno := unix.Syscall(sysLandlockCreateRuleset, 0, 0, createRulesetVersion)
	if errno != 0 || int(abi) <= 0 {
		return 0
	}
	return int(abi)
}

// landlockRestrict restricts the CALLING THREAD: read+execute on /, full
// access on writes, signals scoped to the domain when ABI >= 6. The caller
// must hold runtime.LockOSThread and exec from the same thread (a Landlock
// domain and no_new_privs are per task; execve carries the calling task's
// into the new image). Only ever run in the re-executed child.
func landlockRestrict(writes []string) error {
	abi := LandlockABI()
	if abi <= 0 {
		return errors.New("Landlock is not supported by this kernel")
	}
	handled := landlockFSRights(abi)
	var scoped uint64
	if abi >= 6 {
		scoped = scopeSignal
	}
	attr := rulesetAttrBytes(handled, scoped, abi)
	rs, _, errno := unix.Syscall(sysLandlockCreateRuleset,
		uintptr(unsafe.Pointer(&attr[0])), uintptr(len(attr)), 0)
	runtime.KeepAlive(attr)
	if errno != 0 {
		return fmt.Errorf("landlock_create_ruleset failed: %w", errno)
	}
	ruleset := int(rs)
	defer unix.Close(ruleset)
	for _, r := range landlockRules(writes, handled) {
		fd, err := unix.Open(r.path, unix.O_PATH|unix.O_CLOEXEC, 0)
		if err != nil {
			continue // a writable path that does not exist yet
		}
		access := r.access
		if st, err := os.Stat(r.path); err != nil || !st.IsDir() {
			access &= fsFileRights
		}
		rule := pathBeneathBytes(access&handled, int32(fd))
		_, _, errno := unix.Syscall6(sysLandlockAddRule, uintptr(ruleset), rulePathBeneath,
			uintptr(unsafe.Pointer(&rule[0])), 0, 0, 0)
		runtime.KeepAlive(rule)
		unix.Close(fd)
		if errno != 0 {
			return fmt.Errorf("landlock_add_rule failed for %s: %w", r.path, errno)
		}
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("prctl(PR_SET_NO_NEW_PRIVS) failed: %w", err)
	}
	if _, _, errno := unix.Syscall(sysLandlockRestrictSelf, uintptr(ruleset), 0, 0); errno != 0 {
		return fmt.Errorf("landlock_restrict_self failed: %w", errno)
	}
	return nil
}
