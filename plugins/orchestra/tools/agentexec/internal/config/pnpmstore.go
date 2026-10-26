package config

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// PnpmStorePinName is the variable pnpm 12 reads its store directory from.
// npm_config_store_dir is never set: npm warns about the key and pnpm 12
// ignores it.
const PnpmStorePinName = "pnpm_config_store_dir"

// pnpmStoreNpmName is the name an explicit store dir may also come in, as npm
// reads it; pnpm 12 ignores it, so it is only honoured as a user's choice.
const pnpmStoreNpmName = "npm_config_store_dir"

// pnpmStoreNpmrcKey and pnpmStoreWorkspaceKey match a project's own store
// choice in its .npmrc and pnpm-workspace.yaml.
var (
	pnpmStoreNpmrcKey     = regexp.MustCompile(`(?m)^\s*store-dir\s*=`)
	pnpmStoreWorkspaceKey = regexp.MustCompile(`(?m)^\s*storeDir\s*:`)
)

// PnpmStoreDir is pnpm's own default store root (pnpm appends /v<N>):
// $PNPM_HOME/store, else $XDG_DATA_HOME/pnpm/store, else
// <home>/Library/pnpm/store on darwin and <home>/.local/share/pnpm/store
// elsewhere. A relative PNPM_HOME or XDG_DATA_HOME is ignored, as pnpm
// ignores it. The result has its existing prefix's symlinks resolved, so a
// pin spelled through a symlink still matches the path the kernel reports.
func PnpmStoreDir(platform, home string, getenv func(string) string) string {
	if h := getenv("PNPM_HOME"); strings.HasPrefix(h, "/") {
		return paths.RealPath(filepath.Join(h, "store"))
	}
	if x := getenv("XDG_DATA_HOME"); strings.HasPrefix(x, "/") {
		return paths.RealPath(filepath.Join(x, "pnpm", "store"))
	}
	if platform == "darwin" {
		return paths.RealPath(filepath.Join(home, "Library", "pnpm", "store"))
	}
	return paths.RealPath(filepath.Join(home, ".local", "share", "pnpm", "store"))
}

// PnpmStorePin is the env entry that pins pnpm's store for a process whose
// environment would otherwise be env: nil when env already sets
// pnpm_config_store_dir under any letter case, since the user chose a store
// there; else one entry "pnpm_config_store_dir=<v>", v being an explicit
// npm_config_store_dir (any letter case) from env, else store. An empty
// npm_config_store_dir is no choice: it would pin pnpm to "".
func PnpmStorePin(env []string, store string) []string {
	explicit := ""
	for _, kv := range env {
		name, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch {
		case strings.EqualFold(name, PnpmStorePinName):
			return nil
		case strings.EqualFold(name, pnpmStoreNpmName) && v != "":
			explicit = v
		}
	}
	if explicit != "" {
		store = explicit
	}
	return []string{PnpmStorePinName + "=" + store}
}

// pnpmStoreTreePin is the PnpmStorePin entry for a process started in tree,
// given env, or nil when tree gets none: tree is "", a main checkout (its
// .git is a directory, the user's own), or a linked worktree whose project
// chose its own store (pnpmStoreProjectChose). HOME is env's when absolute,
// else the user's home.
func pnpmStoreTreePin(tree string, env []string) []string {
	if tree == "" || !isFile(filepath.Join(tree, ".git")) || pnpmStoreProjectChose(tree) {
		return nil
	}
	home, ok := envValue(env, "HOME")
	if !ok || !filepath.IsAbs(home) {
		home = paths.Home()
	}
	getenv := func(name string) string {
		v, _ := envValue(env, name)
		return v
	}
	return PnpmStorePin(env, PnpmStoreDir(runtime.GOOS, home, getenv))
}

// pnpmStoreProjectChose reports whether the project at tree's root sets its
// own store: an .npmrc store-dir line or a pnpm-workspace.yaml storeDir line.
// The env variable would override that choice, so such a tree gets no pin.
// Only those two files are read; a missing or unreadable one is not set.
func pnpmStoreProjectChose(tree string) bool {
	npmrc, _ := os.ReadFile(filepath.Join(tree, ".npmrc"))
	if pnpmStoreNpmrcKey.Match(npmrc) {
		return true
	}
	workspace, _ := os.ReadFile(filepath.Join(tree, "pnpm-workspace.yaml"))
	return pnpmStoreWorkspaceKey.Match(workspace)
}
