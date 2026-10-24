package isolate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/trace"
)

// carryDirNames are directories holding installed dependencies or build
// caches: expensive to recreate, invisible to `git worktree add` (being
// ignored), and cheap to clone on a copy-on-write filesystem. A worktree
// without them makes every worker re-run an install.
var carryDirNames = []string{
	"node_modules", ".venv", "venv", ".tox", "vendor", "target",
	".next", ".nuxt", ".svelte-kit", ".gradle", "Pods", ".bundle", ".dart_tool",
}

// carryScanMaxDepth covers the usual monorepo shapes
// (packages/<pkg>/node_modules) without walking a whole large tree.
const carryScanMaxDepth = 3

// Carry methods reported by create. pnpm-gvs: node_modules came from a
// `pnpm install` against the global virtual store instead of a copy.
const (
	carryNone    = "none"
	carryClone   = "clone"
	carryCopy    = "copy"
	carryFailed  = "failed"
	carryPnpmGVS = "pnpm-gvs"
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

// copyTreeMethod copies and reports clone, copy or failed.
func copyTreeMethod(src, dst string) string {
	if !isDir(src) {
		return carryFailed
	}
	if err := os.MkdirAll(filepath.Dir(absPath(dst)), 0o755); err != nil {
		return carryFailed
	}
	if !lexists(dst) {
		end := trace.Begin(trace.KindFS, "clonefile", trace.Attrs{Detail: src + " -> " + dst})
		err := cloneDir(src, dst)
		end(exitOf(err))
		if err == nil {
			return carryClone
		}
		if lexists(dst) {
			_ = os.RemoveAll(dst) // a failed directory clone can leave a partial tree
		}
	}
	attempts := [][]string{{"cp", "-R", "--reflink=auto", src, dst}}
	if runtime.GOOS == "darwin" {
		attempts = [][]string{{"cp", "-Rc", src, dst}}
	}
	attempts = append(attempts, []string{"cp", "-R", src, dst})
	for i, argv := range attempts {
		// The operation is the argv up to its two trailing paths.
		end := trace.Begin(trace.KindFS, strings.Join(argv[:len(argv)-2], " "), trace.Attrs{Detail: src + " -> " + dst})
		cmd := exec.Command(argv[0], argv[1:]...)
		err := cmd.Run()
		end(procExit(err))
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

// procExit is the timing-log exit code of a subprocess: its status, 0, or
// -1 when it could not be started.
func procExit(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
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

// CarryDependencies copies root's dependency dirs into path. A directory
// already present in path (gtr copy patterns, a setup script, a worker's own
// install) is left alone: copying onto it would nest the copy inside it.
// When path runs under pnpm's global virtual store (PnpmGVSTree) the
// node_modules dirs are installed instead of copied, and copied after all
// when the install fails, which note reports.
func CarryDependencies(root, path string, extra []string) (carried []string, method string, present []string, note string) {
	carried, present = []string{}, nil
	method = carryNone
	dirs := DetectCarryDirs(root, extra)
	gvs := config.PnpmGVSTree(config.PnpmGVSOn(), path)
	var nodeModules []string
	if gvs {
		var rest []string
		for _, rel := range dirs {
			if filepath.Base(rel) == "node_modules" {
				nodeModules = append(nodeModules, rel)
			} else {
				rest = append(rest, rel)
			}
		}
		dirs = rest
	}
	carry := func(rels []string) {
		for _, rel := range rels {
			target := filepath.Join(path, rel)
			if lexists(target) {
				present = append(present, rel)
				continue
			}
			m := carryDir(root, path, rel)
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
	}
	carry(dirs)
	if !gvs {
		return carried, method, present, ""
	}
	if err := pnpmInstallGVS(path); err != nil {
		// What a failed install left behind would block the copy.
		_ = os.RemoveAll(filepath.Join(path, "node_modules"))
		for _, rel := range nodeModules {
			_ = os.RemoveAll(filepath.Join(path, rel))
		}
		carry(nodeModules)
		return carried, method, present, "pnpm install (gvs) failed, node_modules copied instead: " + err.Error()
	}
	if method != carryFailed {
		method = carryPnpmGVS
	}
	return carried, method, present, ""
}

// pnpmInstallTimeout bounds each install attempt (the online one can hang
// on the network).
const pnpmInstallTimeout = 10 * time.Minute

// pnpmInstallGVS installs tree's locked dependencies against the global
// virtual store, offline first (the store usually holds every package),
// then online. The error is the last attempt's final output line.
func pnpmInstallGVS(tree string) error {
	var last error
	for _, offline := range []bool{true, false} {
		argv := []string{"pnpm", "install", "--frozen-lockfile"}
		if offline {
			argv = append(argv, "--offline")
		}
		ctx, cancel := context.WithTimeout(context.Background(), pnpmInstallTimeout)
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir = tree
		cmd.Env = config.WithPnpmGVS(config.WithTreeEnv(false, tree, os.Environ()))
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		end := trace.Begin(trace.KindFS, "pnpm install (gvs)", trace.Attrs{Cwd: tree, Detail: strings.Join(argv, " ")})
		err := cmd.Run()
		end(procExit(err))
		cancel()
		if err == nil {
			return nil
		}
		last = errors.New(strings.Join(argv, " ") + ": " + lastLine(out.String(), err))
	}
	return last
}

// lastLine is the last non-empty line of output, else err's text.
func lastLine(output string, err error) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if l := strings.TrimSpace(lines[len(lines)-1]); l != "" {
		return l
	}
	return err.Error()
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
// copied, except those named in excluded and those under the
// ChangeFilter's excluded directories (an unignored .pnpm-store stays in
// the user's tree). Without it a worker would work against stale code
// whenever the user has work in progress.
func carryUncommitted(root, worktree string, excluded []string) {
	if patch, err := gitx.Run(root, "diff", "HEAD", "--binary"); err == nil && strings.TrimSpace(patch) != "" {
		_, _ = gitx.RunInput(worktree, []byte(patch), "apply", "--binary", "--whitespace=nowarn", "-")
	}
	out, err := gitx.Run(root, append([]string{"ls-files", "--others", "--exclude-standard"}, CurrentFilter().Pathspec()...)...)
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
