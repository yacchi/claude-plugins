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

// LinkedWorktree reports whether tree is a linked worktree: its .git is a
// file, so it is never the user's main checkout.
func LinkedWorktree(tree string) bool {
	return tree != "" && isFile(filepath.Join(tree, ".git"))
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
