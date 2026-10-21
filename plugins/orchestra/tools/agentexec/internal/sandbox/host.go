package sandbox

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
)

// HostInput describes the herdr server a HostPolicy confines.
type HostInput struct {
	Repo          string   // the user's repository
	WorktreeBase  string   // orchestra's worktree base of Repo
	HerdrHome     string   // the server's private home
	ExtraWritable []string // e.g. the managed pi agent dir
}

// HostSpec is how the herdr SERVER (and so every pane and the Claude Code
// inside) is sandboxed: Seatbelt or bubblewrap only (Landlock cannot keep
// the learned store read-only inside the writable state dir). Its Spec
// feeds WrapArgv and SrtSettings like any other. (The plan named this type
// HostPolicy too, which Go cannot declare next to the function.)
type HostSpec struct {
	Spec
}

// HostPolicy decides the herdr server's sandbox under cfg.
func HostPolicy(cfg *config.Resolved, in HostInput) HostSpec {
	return HostSpec{prepareHost(ConfigOf(cfg), in, nil, policyOpts{})}
}

func prepareHost(sc Config, in HostInput, results *ProbeReport, o policyOpts) Spec {
	spec := newSpec(sc.Mode)
	if sc.Mode == "off" {
		spec.Reason = "sandbox.mode is off"
		return spec
	}
	if results == nil {
		r := Probes()
		results = &r
	}
	backend := ""
	switch {
	case results.Seatbelt.OK:
		backend = "seatbelt"
	case results.Bwrap.OK:
		backend = "bwrap"
	}
	if backend == "" {
		spec.Reason = "no sandbox backend usable for the herdr host (seatbelt: " +
			results.Seatbelt.Detail + "; bwrap: " + results.Bwrap.Detail + ")"
		if sc.Mode == "required" {
			spec.Refuse = true
		}
		return spec
	}
	policy := hostPolicy(in, sc, o)
	spec.Backend, spec.Enforced, spec.Policy = backend, true, policy
	spec.Writable = append([]string(nil), policy.Writable...)
	if backend == "seatbelt" {
		spec.Reason = "macOS Seatbelt (sandbox-exec)"
	} else {
		spec.Reason = "bubblewrap (user namespaces)"
		spec.Limits = bwrapLimits(policy, exists)
	}
	return spec
}

// claudePaths is (writable, deny_write) for a Claude Code session: its
// config dir and the real path of every symlinked child (a managed config
// dir links projects/sessions/... into a shared dir), and the
// code/instruction names under those dirs.
func claudePaths(home string, o policyOpts) ([]string, []string) {
	home = homeOf(home)
	dir := o.getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		dir = "~/.claude"
	}
	real := realPath(expand(dir, home))
	writable, parents := []string{real}, []string{real}
	if entries, err := os.ReadDir(real); err == nil && isDir(real) {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		sort.Strings(names)
		for _, name := range names {
			p := filepath.Join(real, name)
			if isLink(p) {
				target := realPath(p)
				writable = append(writable, target)
				parents = append(parents, filepath.Dir(target))
			}
		}
	}
	var deny []string
	for _, parent := range dedupe(parents) {
		for _, name := range claudeTamperNames {
			p := filepath.Join(parent, name)
			deny = append(deny, p)
			if lexists(p) {
				deny = append(deny, realPath(p))
			}
		}
	}
	return dedupe(writable), dedupe(deny)
}

func hostPolicy(in HostInput, sc Config, o policyOpts) *Policy {
	home := o.home
	if home == "" {
		home = expandUser("~")
	}
	home = realPath(home)
	platform := o.goos()
	base := realPath(in.WorktreeBase)

	writable := []string{base, realPath(stateDir(home)), realPath(in.HerdrHome)}
	for _, p := range in.ExtraWritable {
		if p != "" {
			writable = append(writable, realPath(p))
		}
	}
	var readonly []string
	_, commonGit := gitDirs(in.Repo)
	if commonGit != "" {
		writable = append(writable, filepath.Join(commonGit, "worktrees"), filepath.Join(commonGit, "objects"))
	}
	claudeW, claudeDeny := claudePaths(home, o)
	writable = append(writable, claudeW...)
	// plugins/ becomes a read-only carve-out with only the inline plugin's
	// data dir writable inside it.
	plugins := filepath.Join(claudeW[0], "plugins")
	inline := filepath.Join(claudeW[0], inlinePluginData)
	_ = os.MkdirAll(inline, 0o700)
	if isDir(inline) {
		realPlugins := realPath(plugins)
		kept := claudeDeny[:0:0]
		for _, d := range claudeDeny {
			if d != plugins && d != realPlugins {
				kept = append(kept, d)
			}
		}
		claudeDeny = kept
		readonly = append(readonly, realPlugins)
		writable = append(writable, realPath(inline))
		for _, n := range claudePluginNames {
			claudeDeny = append(claudeDeny, filepath.Join(realPlugins, n))
		}
	}
	writable = append(writable, filepath.Join(home, ".local", "state", "claude"))
	// The Security framework locks and rebuilds its MDS cache there on every
	// Keychain lookup; denied, Claude Code finds no login ("Not logged in").
	if cache := o.darwinUserCache(); platform == "darwin" && cache != "" {
		writable = append(writable, realPath(filepath.Join(cache, "mds")))
	}
	for _, p := range seeds(platform) {
		writable = append(writable, expand(p, home))
	}
	if tmp := o.getenv("TMPDIR"); tmp != "" {
		writable = append(writable, realPath(tmp))
	}
	writable = append(writable, tmpRoot(platform), "/dev")
	for _, p := range sc.AllowWrite {
		writable = append(writable, expand(p, home))
	}
	var denyRead []string
	for _, p := range DefaultDenyRead {
		if !contains(hostReadable, p) {
			denyRead = append(denyRead, expand(p, home))
		}
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
	// ~/.claude.json and its lock/temp siblings: Seatbelt matches them by
	// regex, the other backends by what exists.
	writableRx := []string{"^" + rxEscape(home) + `/\.claude\.json.*$`}
	if platform != "darwin" {
		matches, _ := filepath.Glob(filepath.Join(globEscape(home), ".claude.json*"))
		writable = append(writable, matches...)
	}
	state := realPath(stateDir(home))
	denyWrite := append(stateDenies(home), claudeDeny...)
	denyWrite = append(denyWrite, filepath.Join(state, "pi-agent"))
	denyRx := []string{}
	for _, tree := range []string{in.Repo, base} {
		root := ""
		if isDir(tree) {
			root = repoRoot(tree)
		}
		if root == "" {
			root = realPath(tree)
		}
		denyRx = append(denyRx, RepoConfigRegex(root))
	}
	if commonGit != "" && underAnyOther(commonGit, writable) {
		readonly = append(readonly, commonGit)
	}
	return &Policy{
		Writable:       dedupe(writable),
		WritableRegex:  writableRx,
		Readonly:       dedupe(readonly),
		DenyRead:       dedupe(denyRead),
		DenyWrite:      dedupe(denyWrite),
		DenyWriteRegex: denyRx,
		Tamper:         dedupe(denyWrite),
		RepoRoot:       repoRoot(in.Repo),
		Learned:        grantPaths(learned),
	}
}

// globEscape is glob.escape: magic characters become one-char classes.
func globEscape(p string) string {
	var b strings.Builder
	for _, r := range p {
		switch r {
		case '*', '?', '[':
			b.WriteString("[" + string(r) + "]")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
