package config

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// MiseTrustedEnv is the mise setting that lists the config paths mise trusts
// without asking. A worker in a linked worktree gets its own tree appended to
// it, so mise reads the tree's mise config (its [env], its tool versions)
// instead of refusing it.
const MiseTrustedEnv = "MISE_TRUSTED_CONFIG_PATHS"

// miseConfigFiles are the mise config files of a tree's root: a tree with none
// of them has nothing for mise to trust.
var miseConfigFiles = []string{
	"mise.toml", ".mise.toml", "mise.local.toml", ".mise/config.toml",
	"mise/config.toml", ".config/mise.toml", ".tool-versions",
}

// MiseShowSeam replaces the `mise trust --show` probe (a test seam). Inside a
// test binary the probe is off without one, so no test depends on the mise on
// PATH.
var MiseShowSeam func(dir string) ([]byte, error)

var (
	miseMu     sync.Mutex
	miseTrusts = map[string]bool{}
)

// TreeEntries are the env entries of a process orchestra starts in tree, given
// env (the env it would otherwise get): the Node CPU cap of the process's test
// pool (NodeCapEntries); the PnpmStorePin of a linked worktree
// (pnpmStoreTreePin), before the GVS switch so that it stays right before the
// command; PnpmGVSEnv when on and tree runs under pnpm's global virtual store
// (PnpmGVSTree); and MISE_TRUSTED_CONFIG_PATHS with tree appended to env's
// value when mise trusts tree (miseTrustedTree). Nil when none applies.
func TreeEntries(on bool, tree string, env []string) []string {
	out := append(NodeCapEntries(ProcessTestSlots(), env), pnpmStoreTreePin(tree, env)...)
	if PnpmGVSTree(on, tree) {
		warnPnpmCI(env)
		out = append(out, PnpmGVSEnv)
	}
	if miseTrustedTree(tree) {
		value := tree
		if prev, ok := envValue(env, MiseTrustedEnv); ok && prev != "" {
			value = prev + ":" + tree
		}
		out = append(out, MiseTrustedEnv+"="+value)
	}
	return out
}

// WithTreeEnv is env with TreeEntries (the Node CPU cap, the store pin, the
// GVS switch and the mise trust) set: an entry replaces a variable of the same name, so each name
// appears once.
func WithTreeEnv(on bool, tree string, env []string) []string {
	return withEntries(env, TreeEntries(on, tree, env))
}

// withEntries is env with each of entries set, replacing a variable of the
// same name.
func withEntries(env, entries []string) []string {
	out := append([]string(nil), env...)
	for _, kv := range entries {
		name, _, _ := strings.Cut(kv, "=")
		out = slices.DeleteFunc(out, func(e string) bool {
			n, _, _ := strings.Cut(e, "=")
			return n == name
		})
		out = append(out, kv)
	}
	return out
}

// envValue is the value of name in env: the last entry naming it wins, as
// exec does.
func envValue(env []string, name string) (string, bool) {
	value, found := "", false
	for _, kv := range env {
		if n, v, ok := strings.Cut(kv, "="); ok && n == name {
			value, found = v, true
		}
	}
	return value, found
}

// miseTrustedTree reports whether mise trusts the config of tree: tree is a
// linked worktree with a mise config file at its root, and the main checkout
// of its repository is trusted (miseTrustedCheckout).
func miseTrustedTree(tree string) bool {
	if tree == "" || !isFile(filepath.Join(tree, ".git")) || !hasMiseConfig(tree) {
		return false
	}
	main := miseCheckout(tree)
	return main != "" && miseTrustedCheckout(main)
}

// hasMiseConfig reports whether tree's root holds a mise config file.
func hasMiseConfig(tree string) bool {
	for _, name := range miseConfigFiles {
		if isFile(filepath.Join(tree, name)) {
			return true
		}
	}
	return false
}

// miseCheckout is the main checkout that the linked worktree tree belongs to,
// read from the gitdir line of its .git file (.git/worktrees/<name> under the
// main checkout). "" when the file has another layout.
func miseCheckout(tree string) string {
	raw, err := os.ReadFile(filepath.Join(tree, ".git"))
	if err != nil {
		return ""
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir:")
	if !ok {
		return ""
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(tree, gitdir)
	}
	worktrees := filepath.Dir(gitdir)
	common := filepath.Dir(worktrees)
	if filepath.Base(worktrees) != "worktrees" || filepath.Base(common) != ".git" {
		return ""
	}
	return filepath.Dir(common)
}

// miseTrustedCheckout reports whether mise trusts the config of the main
// checkout at dir. The probe runs once per checkout per process.
func miseTrustedCheckout(dir string) bool {
	miseMu.Lock()
	defer miseMu.Unlock()
	trusted, ok := miseTrusts[dir]
	if !ok {
		trusted = miseShowTrusts(dir)
		miseTrusts[dir] = trusted
	}
	return trusted
}

// miseShowTrusts parses `mise trust --show` in dir: trusted when every line
// reads "<path>: trusted" and at least one of those paths lies under dir. The
// listing also names the user's global config, so a trusted line outside dir
// does not count. Any error, or a line that reads otherwise, is untrusted.
func miseShowTrusts(dir string) bool {
	probe := MiseShowSeam
	if probe == nil {
		if testing.Testing() {
			return false
		}
		probe = runMiseShow
	}
	out, err := probe(dir)
	if err != nil {
		return false
	}
	under := false
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		path, ok := strings.CutSuffix(line, ": trusted")
		if !ok {
			return false
		}
		under = under || isUnder(dir, path)
	}
	return under
}

// isUnder reports whether path lies inside dir. Both are compared with their
// directories resolved, since mise and git may print /private/var where a
// temporary path reads /var.
func isUnder(dir, path string) bool {
	dir, path = resolveDir(dir), resolveDir(path)
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolveDir is p with its parent directory's symlinks resolved, so p need not
// exist. p as is when the parent cannot be resolved.
func resolveDir(p string) string {
	if real, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		return filepath.Join(real, filepath.Base(p))
	}
	return p
}

// runMiseShow is `mise trust --show` in dir with a short timeout. It only reads
// mise's trust store; `mise trust` itself is never run.
func runMiseShow(dir string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "mise", "trust", "--show")
	cmd.Dir = dir
	cmd.WaitDelay = 2 * time.Second
	return cmd.Output()
}
