package isolate

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// carryDirNames are directories holding installed dependencies or build
// caches: expensive to recreate, invisible to `git worktree add` (being
// ignored), and ~2s to copy. A worktree without them makes every worker
// re-run an install.
var carryDirNames = []string{
	"node_modules", ".venv", "venv", ".tox", "vendor", "target",
	".next", ".nuxt", ".svelte-kit", ".gradle", "Pods", ".bundle", ".dart_tool",
}

// carryScanMaxDepth covers the usual monorepo shapes
// (packages/<pkg>/node_modules) without walking a whole large tree.
const carryScanMaxDepth = 3

// Carry methods reported by create.
const (
	carryNone   = "none"
	carryClone  = "clone"
	carryCopy   = "copy"
	carryFailed = "failed"
)

// DefaultCarryFiles are the local, normally-untracked files carried when the
// config names none.
var DefaultCarryFiles = []string{
	".claude/settings.local.json", ".claude/orchestra.local.yaml", "CLAUDE.local.md",
}

// DetectCarryDirs finds gitignored dependency/build directories under root
// worth copying into a worktree: CARRY names plus extra, matched by
// basename up to depth 3. Only ignored directories qualify (a tracked one
// arrives with the worktree; an untracked one would leak into the diff).
// Relative paths, in walk order.
func DetectCarryDirs(root string, extra []string) []string {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	names := map[string]bool{}
	for _, n := range carryDirNames {
		names[n] = true
	}
	for _, n := range extra {
		names[n] = true
	}
	var found []string
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		list := make([]string, 0, len(entries))
		for _, e := range entries {
			list = append(list, e.Name())
		}
		sort.Strings(list)
		for _, name := range list {
			if name == ".git" {
				continue
			}
			full := filepath.Join(dir, name)
			if !isDir(full) || isSymlink(full) {
				continue
			}
			rel, err := filepath.Rel(abs, full)
			if err != nil {
				continue
			}
			if names[name] && isIgnored(abs, rel) {
				// Nested copies (node_modules/.pnpm/*/node_modules) ride
				// along inside the parent.
				found = append(found, rel)
				continue
			}
			if depth < carryScanMaxDepth {
				walk(full, depth+1)
			}
		}
	}
	walk(abs, 1)
	return found
}

func isIgnored(root, rel string) bool {
	_, err := gitx.Run(root, "check-ignore", "-q", "--", rel)
	return err == nil
}

func isSymlink(p string) bool {
	st, err := os.Lstat(p)
	return err == nil && st.Mode()&os.ModeSymlink != 0
}

// CopyTreeFast copies directory src to dst, preferring a copy-on-write clone
// (APFS `cp -c`, reflink on Linux) and degrading to a plain copy.
func CopyTreeFast(src, dst string) error {
	if copyTreeMethod(src, dst) == carryFailed {
		return errors.New("could not copy " + src)
	}
	return nil
}

// copyTreeMethod copies and reports clone, copy or failed.
func copyTreeMethod(src, dst string) string {
	if !isDir(src) {
		return carryFailed
	}
	if err := os.MkdirAll(filepath.Dir(absPath(dst)), 0o755); err != nil {
		return carryFailed
	}
	attempts := [][]string{{"cp", "-R", "--reflink=auto", src, dst}}
	if runtime.GOOS == "darwin" {
		attempts = [][]string{{"cp", "-Rc", src, dst}}
	}
	attempts = append(attempts, []string{"cp", "-R", src, dst})
	for i, argv := range attempts {
		cmd := exec.Command(argv[0], argv[1:]...)
		err := cmd.Run()
		if err == nil {
			if i == 0 {
				return carryClone
			}
			return carryCopy
		}
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			continue // cp could not be started: nothing was written
		}
		if lexists(dst) {
			_ = os.RemoveAll(dst)
		}
	}
	if err := copyTree(src, dst); err != nil {
		return carryFailed
	}
	return carryCopy
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// copyTree is the in-process fallback: symlinks are copied as links.
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		default:
			return copyFile(path, target)
		}
	})
}

// copyFile is shutil.copy2: content, permission bits and times.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, st.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	_ = os.Chmod(dst, st.Mode().Perm())
	_ = os.Chtimes(dst, st.ModTime(), st.ModTime())
	return nil
}

// carryDependencies copies root's dependency dirs into path. A directory
// already present in path (gtr copy patterns, a setup script, a worker's own
// install) is left alone: copying onto it would nest the copy inside it.
func carryDependencies(root, path string, extra []string) (carried []string, method string, present []string) {
	carried, present = []string{}, nil
	method = carryNone
	for _, rel := range DetectCarryDirs(root, extra) {
		target := filepath.Join(path, rel)
		if lexists(target) {
			present = append(present, rel)
			continue
		}
		m := copyTreeMethod(filepath.Join(root, rel), target)
		switch {
		case m == carryFailed:
			method = carryFailed
		case method != carryFailed && m == carryCopy:
			method = carryCopy
		case method == carryNone:
			method = carryClone
		}
		if m != carryFailed {
			carried = append(carried, rel)
		}
	}
	return carried, method, present
}

func pathInside(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, strings.TrimRight(dir, string(os.PathSeparator))+string(os.PathSeparator))
}

// carryLocalFiles copies the configured local files (settings.local.json
// and friends) from root into path when absent there. Only regular,
// non-symlink files that stay inside both trees qualify.
func carryLocalFiles(root, path string, files []string) []string {
	copied := []string{}
	realRoot, realPath := paths.RealPath(root), paths.RealPath(path)
	if realRoot == realPath {
		return copied
	}
	for _, rel := range files {
		source := filepath.Join(root, rel)
		target := filepath.Join(path, rel)
		st, err := os.Stat(source)
		if err != nil || !st.Mode().IsRegular() || isSymlink(source) || lexists(target) {
			continue
		}
		if !pathInside(paths.RealPath(source), realRoot) || !pathInside(paths.RealPath(target), realPath) {
			continue
		}
		if os.MkdirAll(filepath.Dir(target), 0o755) != nil || copyFile(source, target) != nil {
			continue
		}
		copied = append(copied, rel)
	}
	return copied
}

// carryUncommitted reproduces root's uncommitted state in the fresh
// worktree: tracked edits as a binary patch, untracked (not ignored) files
// copied, except those named in excluded. Without it a worker would work
// against stale code whenever the user has work in progress.
func carryUncommitted(root, worktree string, excluded []string) {
	if patch, err := gitx.Run(root, "diff", "HEAD", "--binary"); err == nil && strings.TrimSpace(patch) != "" {
		_, _ = gitx.RunInput(worktree, []byte(patch), "apply", "--binary", "--whitespace=nowarn", "-")
	}
	out, err := gitx.Run(root, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return
	}
	skip := map[string]bool{}
	for _, f := range excluded {
		skip[f] = true
	}
	for _, rel := range strings.Split(out, "\n") {
		rel = strings.TrimSpace(rel)
		if rel == "" || skip[rel] {
			continue
		}
		src := filepath.Join(root, rel)
		st, err := os.Stat(src)
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		dst := filepath.Join(worktree, rel)
		if os.MkdirAll(filepath.Dir(dst), 0o755) == nil {
			_ = copyFile(src, dst)
		}
	}
}
