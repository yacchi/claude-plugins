package sandbox

import "encoding/binary"

// Landlock UAPI (include/uapi/linux/landlock.h). The syscall numbers are the
// same on every architecture (generic table).
const (
	sysLandlockCreateRuleset = 444
	sysLandlockAddRule       = 445
	sysLandlockRestrictSelf  = 446
	createRulesetVersion     = 1 << 0
	rulePathBeneath          = 1
	scopeSignal              = 1 << 1

	fsExecute   = 1 << 0
	fsWriteFile = 1 << 1
	fsReadFile  = 1 << 2
	fsReadDir   = 1 << 3
	// Rights that apply to a regular file (any other right on a file rule is
	// EINVAL): execute, write, read, truncate (ABI 3), ioctl_dev (ABI 5).
	fsFileRights = fsExecute | fsWriteFile | fsReadFile | (1 << 14) | (1 << 15)
)

// landlockFSRights is every filesystem right the given ABI knows about.
func landlockFSRights(abi int) uint64 {
	bits := 13 // ABI 1: EXECUTE .. MAKE_SYM
	if abi >= 2 {
		bits = 14 // REFER
	}
	if abi >= 3 {
		bits = 15 // TRUNCATE
	}
	if abi >= 5 {
		bits = 16 // IOCTL_DEV
	}
	return (1 << bits) - 1
}

// rulesetAttrSize passes only the fields this ABI knows (a larger size with
// unknown non-zero fields is E2BIG on older kernels). handled_access_net
// (ABI 4) stays zero on purpose: network is allowed.
func rulesetAttrSize(abi int) int {
	switch {
	case abi >= 6:
		return 24
	case abi >= 4:
		return 16
	}
	return 8
}

// rulesetAttrBytes is struct landlock_ruleset_attr {u64 handled_access_fs;
// u64 handled_access_net; u64 scoped;} in native byte order, truncated to
// the fields abi knows.
func rulesetAttrBytes(handledFS, scoped uint64, abi int) []byte {
	b := make([]byte, 24)
	binary.NativeEndian.PutUint64(b[0:], handledFS)
	binary.NativeEndian.PutUint64(b[16:], scoped)
	return b[:rulesetAttrSize(abi)]
}

// pathBeneathBytes is struct landlock_path_beneath_attr {u64
// allowed_access; s32 parent_fd;} __attribute__((packed)).
func pathBeneathBytes(access uint64, fd int32) []byte {
	b := make([]byte, 12)
	binary.NativeEndian.PutUint64(b[0:], access)
	binary.NativeEndian.PutUint32(b[8:], uint32(fd))
	return b
}

// landlockRule is one path_beneath rule before it is opened.
type landlockRule struct {
	path   string
	access uint64
}

// landlockRules: read+execute on /, every handled right on each writable
// path. File paths are narrowed to fsFileRights when the rule is added.
func landlockRules(writes []string, handled uint64) []landlockRule {
	rules := []landlockRule{{"/", fsExecute | fsReadFile | fsReadDir}}
	for _, w := range writes {
		rules = append(rules, landlockRule{w, handled})
	}
	return rules
}
