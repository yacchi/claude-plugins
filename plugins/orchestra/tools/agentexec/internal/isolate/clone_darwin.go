package isolate

import "golang.org/x/sys/unix"

// cloneDir clones the whole tree with one clonefile(2): APFS does it in the
// kernel, where `cp -Rc` makes a clone (and a file-event) per entry. On a
// 200k-entry pnpm node_modules that is seconds against minutes, and several
// workers starting at once no longer pin every core.
func cloneDir(src, dst string) error {
	return unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW)
}
