package isolate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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
// basename up to config.CarryScanMaxDepth. Only ignored directories qualify (a tracked one
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
			if depth < config.CarryScanMaxDepth {
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
//
// When path runs under pnpm's global virtual store (PnpmGVSTree) every pnpm
// project of path (config.PnpmProjects) gets its node_modules rebuilt
// instead of copied: any node_modules already under it is removed and the
// project installed, one project at a time. A project whose install fails
// gets its node_modules copied after all, which note reports. Otherwise the
// copies are checked for links that dangle at their new depth
// (repairDangling). Only a linked worktree (a disposable orchestra tree) is
// ever rebuilt or repaired; a main checkout just gets the copies.
func CarryDependencies(root, path string, extra []string) (carried []string, method string, present []string, note string) {
	carried, present = []string{}, nil
	method = carryNone
	dirs := DetectCarryDirs(root, extra)
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
	if !config.LinkedWorktree(path) {
		carry(dirs)
		return carried, method, present, ""
	}
	projects := config.PnpmProjects(path)
	if !config.PnpmGVSTree(config.PnpmGVSOn(), path) {
		carry(dirs)
		carried, note = repairDangling(path, projects, carried)
		return carried, method, present, note
	}
	// Each node_modules of root inside a project belongs to its innermost
	// project: the fallback copy of a failed install.
	owned := map[string][]string{}
	var rest []string
	for _, rel := range dirs {
		if filepath.Base(rel) == "node_modules" {
			if p, ok := innermostProject(projects, rel); ok {
				owned[p] = append(owned[p], rel)
				continue
			}
		}
		rest = append(rest, rel)
	}
	carry(rest)
	for _, p := range projects {
		removeNodeModules(path, p)
	}
	// Sequential (installs are memory-heavy), parents before the projects
	// nested in them (sorted order), each project once.
	var failures []string
	for _, p := range projects {
		err := pnpmInstall(filepath.Join(path, p), path, true)
		if err == nil {
			continue
		}
		// What a failed install left behind would block the copy.
		removeNodeModules(path, p)
		carry(owned[p])
		failures = append(failures, "pnpm install (gvs) failed in "+p+", node_modules copied instead: "+err.Error())
	}
	if len(failures) == 0 && method != carryFailed {
		method = carryPnpmGVS
	}
	return carried, method, present, strings.Join(failures, "; ")
}

// innermostProject is the deepest of projects (paths relative to the tree,
// "." for its root) holding rel.
func innermostProject(projects []string, rel string) (string, bool) {
	best, found := "", false
	for _, p := range projects {
		if (p == "." || rel == p || strings.HasPrefix(rel, p+string(filepath.Separator))) &&
			(!found || len(p) > len(best) || best == ".") {
			best, found = p, true
		}
	}
	return best, found
}

// removeNodeModules removes every node_modules in tree under the project
// directory project (relative to tree), no deeper than
// config.CarryScanMaxDepth below tree's root. The walk follows no symlink and
// skips dot directories; a symlinked node_modules is removed as a link, so
// nothing outside tree is ever touched.
func removeNodeModules(tree, project string) {
	depth := 1
	if project != "." {
		depth = len(strings.Split(project, string(filepath.Separator))) + 1
	}
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			name := e.Name()
			full := filepath.Join(dir, name)
			switch {
			case name == "node_modules":
				removeEntry(full)
			case e.IsDir() && depth < config.CarryScanMaxDepth && !strings.HasPrefix(name, "."):
				walk(full, depth+1) // lstat type: a symlinked dir is not walked
			}
		}
	}
	walk(filepath.Join(tree, project), depth)
}

// removeEntry removes p without following it: a symlink goes as a link, a
// directory with its contents (RemoveAll unlinks the links inside it).
func removeEntry(p string) {
	st, err := os.Lstat(p)
	if err != nil {
		return
	}
	if st.Mode()&os.ModeSymlink != 0 {
		_ = os.Remove(p)
		return
	}
	_ = os.RemoveAll(p)
}

// repairDangling checks each node_modules of carried (copied into tree) for
// links that no longer resolve: a global-virtual-store node_modules links
// into the store by a path relative to its own depth, so a copy at another
// depth dangles. Such a node_modules is removed, and the pnpm project
// holding it (its innermost one of projects) reinstalled without the global
// virtual store, once per project. carried loses what is gone for good;
// note reports each removal that no install restored.
func repairDangling(tree string, projects, carried []string) ([]string, string) {
	reinstall := map[string]bool{}
	var notes []string
	for _, rel := range carried {
		if filepath.Base(rel) != "node_modules" || !danglingLinks(filepath.Join(tree, rel)) {
			continue
		}
		removeEntry(filepath.Join(tree, rel))
		if p, ok := innermostProject(projects, rel); ok {
			reinstall[p] = true
		} else {
			notes = append(notes, rel+" had dangling links and was removed; no pnpm project to reinstall it")
		}
	}
	if len(reinstall) == 0 && len(notes) == 0 {
		return carried, ""
	}
	order := make([]string, 0, len(reinstall))
	for p := range reinstall {
		order = append(order, p)
	}
	sort.Strings(order)
	for _, p := range order {
		if err := pnpmInstall(filepath.Join(tree, p), tree, false); err != nil {
			notes = append(notes, "pnpm install failed in "+p+" after removing a node_modules with dangling links: "+err.Error())
		}
	}
	kept := carried[:0:0]
	for _, rel := range carried {
		if lexists(filepath.Join(tree, rel)) {
			kept = append(kept, rel)
		}
	}
	return kept, strings.Join(notes, "; ")
}

// danglingLinks reports whether node_modules dir nm holds a symlink whose
// target does not exist, among its top-level entries and the entries one
// level inside its @scope directories and .bin. Deeper levels are not read.
func danglingLinks(nm string) bool {
	dangling := func(p string) bool {
		st, err := os.Lstat(p)
		if err != nil || st.Mode()&os.ModeSymlink == 0 {
			return false
		}
		_, err = os.Stat(p)
		return errors.Is(err, fs.ErrNotExist)
	}
	entries, err := os.ReadDir(nm)
	if err != nil {
		return false
	}
	for _, e := range entries {
		full := filepath.Join(nm, e.Name())
		if dangling(full) {
			return true
		}
		if !e.IsDir() || (e.Name() != ".bin" && !strings.HasPrefix(e.Name(), "@")) {
			continue
		}
		inner, err := os.ReadDir(full)
		if err != nil {
			continue
		}
		for _, ie := range inner {
			if dangling(filepath.Join(full, ie.Name())) {
				return true
			}
		}
	}
	return false
}

// pnpmInstallTimeout bounds each install attempt (the online one can hang
// on the network).
const pnpmInstallTimeout = 10 * time.Minute

// pnpmInstall installs the locked dependencies of the pnpm project at dir in
// the orchestra tree tree, offline first (the store usually holds every
// package), then online; with gvs against the global virtual store, else
// with the switch removed from the env. The error is the last attempt's
// final output line.
func pnpmInstall(dir, tree string, gvs bool) error {
	label := "pnpm install"
	env := config.WithTreeEnv(false, tree, os.Environ())
	if gvs {
		label = "pnpm install (gvs)"
		env = config.WithPnpmGVS(env)
	} else {
		name, _, _ := strings.Cut(config.PnpmGVSEnv, "=")
		env = slices.DeleteFunc(env, func(kv string) bool {
			n, _, _ := strings.Cut(kv, "=")
			return n == name
		})
	}
	var last error
	for _, offline := range []bool{true, false} {
		argv := []string{"pnpm", "install", "--frozen-lockfile"}
		if offline {
			argv = append(argv, "--offline")
		}
		ctx, cancel := context.WithTimeout(context.Background(), pnpmInstallTimeout)
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir = dir
		cmd.Env = env
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		end := trace.Begin(trace.KindFS, label, trace.Attrs{Cwd: dir, Detail: strings.Join(argv, " ")})
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
// the user's tree). Tracked edits under excluded directories are not
// carried either. Without it a worker would work against stale code
// whenever the user has work in progress.
func carryUncommitted(root, worktree string, excluded []string) {
	if patch, err := gitx.Run(root, append([]string{"diff", "HEAD", "--binary"}, CurrentFilter().Pathspec()...)...); err == nil && strings.TrimSpace(patch) != "" {
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
