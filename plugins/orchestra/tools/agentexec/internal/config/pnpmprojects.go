package config

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// CarryScanMaxDepth is how deep, in path components below a tree's root, the
// dependency scans look: deep enough for the usual monorepo shapes
// (packages/<pkg>/node_modules, packages/<pkg>/pnpm-lock.yaml) without
// walking a whole large tree.
const CarryScanMaxDepth = 3

// The marker files orchestra keeps in a worktree's own git dir. Their names are
// the single source of truth: isolate writes and reads them under these names.
const (
	// BaselineMarker holds the commit a worker started from.
	BaselineMarker = "orchestra-baseline"
	// AdoptedMarker holds the task of a worktree orchestra did not create.
	AdoptedMarker = "orchestra-adopted"
)

// LinkedWorktree reports whether tree is a linked worktree: its .git is a
// file, so it is never the user's main checkout.
func LinkedWorktree(tree string) bool {
	return tree != "" && isFile(filepath.Join(tree, ".git"))
}

// OrchestraTree reports whether tree is a linked worktree that orchestra
// prepared: its own git dir (not the common one) holds a regular file named
// BaselineMarker or AdoptedMarker. A main checkout, a worktree without a
// marker, or one whose git dir cannot be found, is not.
func OrchestraTree(tree string) bool {
	if !LinkedWorktree(tree) {
		return false
	}
	gitdir := worktreeGitDir(tree)
	if gitdir == "" {
		return false
	}
	for _, name := range []string{BaselineMarker, AdoptedMarker} {
		if st, err := os.Lstat(filepath.Join(gitdir, name)); err == nil && st.Mode().IsRegular() {
			return true
		}
	}
	return false
}

// worktreeGitDir is the git dir named by the gitdir line of tree's .git file,
// a relative path resolved against tree. "" when the file is unreadable or
// names no git dir.
func worktreeGitDir(tree string) string {
	raw, err := os.ReadFile(filepath.Join(tree, ".git"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		v, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if v == "" {
			return ""
		}
		if !filepath.IsAbs(v) {
			v = filepath.Join(tree, v)
		}
		return v
	}
	return ""
}

// PnpmClassicLayout reports whether a project of tree keeps its pnpm virtual
// store in its own node_modules (the classic layout). A project is classic
// when its node_modules/.modules.yaml has a top-level virtualStoreDir that
// resolves to <project>/node_modules/.pnpm: a relative value resolves against
// that node_modules, an absolute one stands as it is. The global layout's
// links dir is not classic; a missing or unreadable file is not classic.
func PnpmClassicLayout(tree string) bool {
	for _, p := range PnpmProjects(tree) {
		nm := filepath.Join(tree, p, "node_modules")
		raw, err := os.ReadFile(filepath.Join(nm, ".modules.yaml"))
		if err != nil {
			continue
		}
		value, ok := virtualStoreDirValue(raw)
		if !ok {
			continue
		}
		if !filepath.IsAbs(value) {
			value = filepath.Join(nm, value)
		}
		if filepath.Clean(value) == filepath.Join(nm, ".pnpm") {
			return true
		}
	}
	return false
}

// virtualStoreDirValue is the value of the first top-level (unindented)
// virtualStoreDir key of a .modules.yaml, trimmed, with one pair of matching
// single or double quotes removed.
func virtualStoreDirValue(raw []byte) (string, bool) {
	for _, line := range strings.Split(string(raw), "\n") {
		v, ok := strings.CutPrefix(line, "virtualStoreDir:")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		return v, true
	}
	return "", false
}

// PnpmProjects are the pnpm projects of tree: the directories, tree's root
// included ("."), holding a pnpm-lock.yaml no deeper than CarryScanMaxDepth
// components below the root, as paths relative to tree in sorted order. The
// walk skips node_modules, dot directories and symlinked directories. A
// workspace member without its own lockfile is not a project: its
// workspace's install covers it.
func PnpmProjects(tree string) []string {
	if tree == "" {
		return nil
	}
	var found []string
	var walk func(dir, rel string, depth int)
	walk = func(dir, rel string, depth int) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			name := e.Name()
			switch {
			case name == "pnpm-lock.yaml" && e.Type().IsRegular():
				found = append(found, rel)
			case e.IsDir() && depth < CarryScanMaxDepth &&
				name != "node_modules" && !strings.HasPrefix(name, "."):
				// DirEntry types come from lstat: a symlinked directory is
				// not a directory here, so the walk never leaves tree.
				walk(filepath.Join(dir, name), filepath.Join(rel, name), depth+1)
			}
		}
	}
	walk(tree, ".", 1)
	sort.Strings(found)
	return found
}
