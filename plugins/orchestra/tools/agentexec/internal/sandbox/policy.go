package sandbox

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
)

// Policy is the one policy every backend enforces, paths realpath'd.
//
// Readonly lists carve-outs INSIDE a writable path that stay read-only (a
// non-isolated checkout's own .git); writable entries under a carve-out
// are re-allowed after it. DenyWrite (subpaths) and DenyWriteRegex are
// emitted AFTER every allow and win over all of them: tamper protection
// and pi's code-loading config. WritableRegex (host policy only) adds
// regex allows (~/.claude.json and its siblings).
type Policy struct {
	Writable       []string `json:"writable"`
	WritableRegex  []string `json:"writable_regex,omitempty"`
	Readonly       []string `json:"readonly"`
	DenyRead       []string `json:"deny_read"`
	DenyWrite      []string `json:"deny_write"`
	DenyWriteRegex []string `json:"deny_write_regex"`
	Tamper         []string `json:"tamper"`
	RepoRoot       string   `json:"repo_root"`
	Learned        []string `json:"learned"`
}

func (p *Policy) clone() *Policy {
	if p == nil {
		return nil
	}
	c := *p
	cp := func(s []string) []string { return append([]string(nil), s...) }
	c.Writable, c.WritableRegex, c.Readonly = cp(p.Writable), cp(p.WritableRegex), cp(p.Readonly)
	c.DenyRead, c.DenyWrite, c.DenyWriteRegex = cp(p.DenyRead), cp(p.DenyWrite), cp(p.DenyWriteRegex)
	c.Tamper, c.Learned = cp(p.Tamper), cp(p.Learned)
	return &c
}

// policyOpts are the knobs tests (and the host policy) override; zero
// values mean the process's own: $HOME, os.Environ, runtime.GOOS and the
// learned store. userCache stands in for DARWIN_USER_CACHE_DIR whenever
// platform is set.
type policyOpts struct {
	home      string
	env       map[string]string
	platform  string
	learned   *[]Grant
	userCache string
}

func (o policyOpts) getenv(k string) string {
	if o.env != nil {
		return o.env[k]
	}
	return os.Getenv(k)
}

func (o policyOpts) goos() string {
	if o.platform != "" {
		return o.platform
	}
	return runtime.GOOS
}

// darwinUserCache is DARWIN_USER_CACHE_DIR (/var/folders/../C), "" off
// macOS. It does not follow TMPDIR, so it is asked of getconf, not derived.
func (o policyOpts) darwinUserCache() string {
	if o.platform != "" {
		return o.userCache
	}
	if runtime.GOOS != "darwin" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/getconf", "DARWIN_USER_CACHE_DIR").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}

func gitOut(dir string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return string(out), true
}

// gitDirs is (own git dir, common git dir) of tree, or ("", "") outside git.
func gitDirs(tree string) (string, string) {
	out, ok := gitOut(tree, "rev-parse", "--absolute-git-dir", "--git-common-dir")
	if !ok {
		return "", ""
	}
	lines := fsutil.SplitLines(out)
	if len(lines) < 2 {
		return "", ""
	}
	own := realPath(lines[0])
	common := lines[1]
	if !strings.HasPrefix(common, "/") {
		common = filepath.Join(tree, common)
	}
	return own, realPath(common)
}

func repoRoot(tree string) string {
	out, ok := gitOut(tree, "rev-parse", "--show-toplevel")
	if !ok || strings.TrimSpace(out) == "" {
		return ""
	}
	return realPath(strings.TrimSpace(out))
}

func executorStateDirs(executor, home string, o policyOpts) []string {
	if executor != "pi" {
		return nil
	}
	// pi mkdirs auth.json.lock / settings.json.lock next to those files on
	// every run and rewrites auth.json on token refresh.
	base := o.getenv("PI_CODING_AGENT_DIR")
	if base == "" {
		base = "~/.pi/agent"
	}
	dirs := []string{base}
	// A managed agent dir symlinks auth.json to the user's real file.
	auth := filepath.Join(expand(base, home), "auth.json")
	if isLink(auth) {
		realDir := filepath.Dir(realPath(auth))
		if !contains(dirs, realDir) {
			dirs = append(dirs, realDir)
		}
	}
	return dirs
}

var rxMeta = regexp.MustCompile(`([.^$*+?()\[\]{}|\\])`)

// rxEscape escapes only POSIX ERE metacharacters (SBPL's regex engine need
// not accept an escaped `-`).
func rxEscape(text string) string { return rxMeta.ReplaceAllString(text, `\$1`) }

// RepoConfigRegex matches every .claude/orchestra*.y*ml under root (SBPL
// and Go regexp syntax alike).
func RepoConfigRegex(root string) string {
	return "^" + rxEscape(strings.TrimRight(root, "/")) + `/(.*/)?\.claude/orchestra[^/]*\.y[^/]*ml$`
}

// repoConfigFiles are the existing .claude/orchestra*.y*ml files under root
// (for backends that can only protect what exists), skipping .git and
// node_modules, in os.walk order.
func repoConfigFiles(root string, limit int) []string {
	rx, err := regexp.Compile(RepoConfigRegex(root))
	if err != nil {
		return nil
	}
	var found []string
	var walk func(dir string) bool
	walk = func(dir string) bool {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return true
		}
		if filepath.Base(dir) == ".claude" {
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
			}
			sort.Strings(names)
			for _, name := range names {
				full := filepath.Join(dir, name)
				if st, err := os.Stat(full); err == nil && st.Mode().IsRegular() && rx.MatchString(full) {
					found = append(found, full)
					if len(found) >= limit {
						return false
					}
				}
			}
		}
		for _, e := range entries {
			isDirEntry := e.IsDir()
			if e.Type()&fs.ModeSymlink != 0 {
				isDirEntry = false // os.walk does not follow links
			}
			if !isDirEntry || e.Name() == ".git" || e.Name() == "node_modules" {
				continue
			}
			if !walk(filepath.Join(dir, e.Name())) {
				return false
			}
		}
		return true
	}
	walk(root)
	return found
}

func piDenyPaths(stateDir string) []string {
	stateDir = realPath(stateDir)
	var out []string
	for _, d := range piDenySubdirs {
		out = append(out, filepath.Join(stateDir, d))
	}
	for _, f := range piDenyFiles {
		out = append(out, filepath.Join(stateDir, f))
	}
	return out
}

func tmpRoot(platform string) string {
	if platform == "darwin" {
		return realPath("/private/tmp")
	}
	return realPath("/tmp")
}

func seeds(platform string) []string {
	s := append([]string(nil), seedCacheWrites...)
	if platform == "darwin" {
		s = append(s, seedCacheWritesDarwin...)
	}
	return s
}

func grantPaths(learned []Grant) []string {
	out := []string{}
	for _, g := range learned {
		if g.Path != "" {
			out = append(out, g.Path)
		}
	}
	return out
}

// buildPolicy is the policy for executor's run in tree.
func buildPolicy(tree, executor string, sc Config, o policyOpts) *Policy {
	home := o.home
	if home == "" {
		home = expandUser("~")
	}
	platform := o.goos()
	tree = realPath(tree)

	writable := []string{tree}
	var readonly []string
	ownGit, commonGit := gitDirs(tree)
	if commonGit != "" {
		// A linked worktree's own .git/worktrees/<name> and the shared
		// object store; refs, config and hooks stay read-only.
		if ownGit != commonGit {
			writable = append(writable, ownGit)
		}
		writable = append(writable, filepath.Join(commonGit, "objects"))
	}
	if tmp := o.getenv("TMPDIR"); tmp != "" {
		writable = append(writable, realPath(tmp))
	}
	writable = append(writable, tmpRoot(platform), "/dev")

	// Not the whole state dir: the managed pi agent dir lives inside it.
	tamper := stateDenies(home)
	denyWrite := append([]string(nil), tamper...)
	for _, p := range executorStateDirs(executor, home, o) {
		state := expand(p, home)
		writable = append(writable, state)
		if executor == "pi" {
			denyWrite = append(denyWrite, piDenyPaths(state)...)
		}
	}
	for _, p := range seeds(platform) {
		writable = append(writable, expand(p, home))
	}
	for _, p := range sc.AllowWrite {
		writable = append(writable, expand(p, home))
	}
	var denyRead []string
	for _, p := range DefaultDenyRead {
		denyRead = append(denyRead, expand(p, home))
	}
	for _, p := range sc.DenyRead {
		denyRead = append(denyRead, expand(p, home))
	}
	var learned []Grant
	if o.learned != nil {
		learned = *o.learned
	} else {
		learned, _, _ = loadGrants(home, denyRead)
	}
	for _, g := range learned {
		if g.Path != "" {
			writable = append(writable, realPath(g.Path))
		}
	}

	denyRx := []string{}
	root := repoRoot(tree)
	if root != "" {
		denyRx = append(denyRx, RepoConfigRegex(root))
	}
	// The common git dir stays read-only even inside something writable.
	if commonGit != "" && underAnyOther(commonGit, writable) {
		readonly = append(readonly, commonGit)
	}
	return &Policy{
		Writable:       dedupe(writable),
		Readonly:       dedupe(readonly),
		DenyRead:       dedupe(denyRead),
		DenyWrite:      dedupe(denyWrite),
		DenyWriteRegex: denyRx,
		Tamper:         tamper,
		RepoRoot:       root,
		Learned:        grantPaths(learned),
	}
}

func underAnyOther(p string, list []string) bool {
	for _, w := range list {
		if w != p && under(p, w) {
			return true
		}
	}
	return false
}
