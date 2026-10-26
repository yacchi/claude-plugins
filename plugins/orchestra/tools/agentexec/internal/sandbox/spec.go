package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
)

// Spec is how one child is sandboxed. Policy is nil when nothing wraps the
// child (mode off, no backend, or a site that spawns nothing). Refuse is
// set when `mode: required` found no backend: the caller must not spawn.
type Spec struct {
	Backend, Reason, Mode, Tree string
	Refuse                      bool

	Enforced bool
	Limits   []string
	Writable []string
	Policy   *Policy
	ABI      int      // landlock
	Srt      *SrtInfo // backend srt
	// Cwd is the directory relative paths in denial messages resolve
	// against (the run's workdir); Tree when empty.
	Cwd string
	// PnpmGVS: pnpm's global virtual store is on for the run's config
	// (config.PnpmGVS); ChildEnv applies it to Tree (config.TreeEntries).
	PnpmGVS bool
}

// SrtInfo is the srt part of a Spec with backend srt.
type SrtInfo struct {
	Path, ID, Session, SettingsPath, LogPath string
	Domains, Quiet                           []string
	Settings                                 map[string]any
}

// Network is the network plan of one CLI executor run (srt.cliNetwork):
// the srt binary ("" = unavailable, with Reason), the domains its settings
// allow, and the run's settings/log name and audit session.
type Network struct {
	Srt, Reason string
	Domains     []string
	ID, Session string
}

// NetworkPlanner plans a run's network; package srt registers it (srt
// sits above sandbox in the package graph).
type NetworkPlanner func(cfg *config.Resolved, executor, tree, model, task string) *Network

// ExecutorEnv returns the child-env additions orchestra owns for an
// executor (pi's managed agent dir); registered by the package that owns
// them. The policy must see them: pi's state dir is writable.
type ExecutorEnv func(executor string, cfg *config.Resolved) map[string]string

var hooks struct {
	sync.RWMutex
	network NetworkPlanner
	env     ExecutorEnv
}

// SetNetworkPlanner registers the network planner Prepare uses.
func SetNetworkPlanner(f NetworkPlanner) {
	hooks.Lock()
	hooks.network = f
	hooks.Unlock()
}

// SetExecutorEnv registers the executor env overrides Prepare uses.
func SetExecutorEnv(f ExecutorEnv) {
	hooks.Lock()
	hooks.env = f
	hooks.Unlock()
}

func newSpec(mode string) Spec {
	return Spec{Backend: "none", Mode: mode, Limits: []string{}, Writable: []string{}}
}

// Prepare decides how executor's run in tree is sandboxed under cfg: under
// srt (network allowlist) when it is usable, with this executor's API
// hosts (model picks pi's provider) and the tree's registry presets; task
// names the run's srt settings/log and its audit session. Otherwise
// Seatbelt, bwrap or Landlock, with `network not restricted: <why>` in
// Limits and Reason.
func Prepare(executor, tree string, cfg *config.Resolved, model, task string) Spec {
	sc := ConfigOf(cfg)
	hooks.RLock()
	planner, envHook := hooks.network, hooks.env
	hooks.RUnlock()
	var o policyOpts
	if envHook != nil {
		if overrides := envHook(executor, cfg); len(overrides) > 0 {
			o.env = envMap(os.Environ())
			for k, v := range overrides {
				o.env[k] = v
			}
		}
	}
	var network *Network
	if sc.Mode != "off" && planner != nil {
		network = planner(cfg, executor, tree, model, task)
	}
	s := prepare(executor, tree, sc, nil, o, network)
	s.Tree = tree
	s.PnpmGVS = config.PnpmGVS(cfg)
	return s
}

// prepare builds the Spec for one executor run. network nil keeps the
// network question out (open network, no note).
func prepare(executor, tree string, sc Config, results *ProbeReport, o policyOpts, network *Network) Spec {
	spec := newSpec(sc.Mode)
	spec.Tree = tree
	if sc.Mode == "off" {
		spec.Reason = "sandbox.mode is off"
		return spec
	}
	why := ""
	if network != nil {
		if results == nil {
			r := Probes()
			results = &r
		}
		why = srtSpec(&spec, executor, tree, sc, *results, o, network)
		if why == "" {
			return spec
		}
	}
	spec = prepareOS(spec, executor, tree, sc, results, o)
	if why != "" {
		note := "network not restricted: " + why
		spec.Limits = append(spec.Limits, note)
		if spec.Reason != "" {
			spec.Reason += "; " + note
		} else {
			spec.Reason = note
		}
	}
	return spec
}

// srtSpec fills spec for an srt run and returns "", or why srt does not
// apply.
func srtSpec(spec *Spec, executor, tree string, sc Config, results ProbeReport, o policyOpts, n *Network) string {
	if sc.Network.Mode != "allowlist" {
		return "sandbox.network.mode is off"
	}
	if n.Srt == "" {
		if n.Reason != "" {
			return n.Reason
		}
		return "srt not available"
	}
	if !results.Seatbelt.OK && !results.Bwrap.OK {
		// srt confines through Seatbelt / bubblewrap itself.
		return "srt needs a working Seatbelt or bubblewrap (" + backendDetail(results, false) + ")"
	}
	policy := buildPolicy(tree, executor, sc, policyOpts{env: o.env})
	domains := dedupe(n.Domains)
	runID := n.ID
	if runID == "" {
		runID = "run"
	}
	session := n.Session
	if session == "" {
		session = runID
	}
	dir := filepath.Join(stateDir(""), CLISrtDir)
	spec.Backend, spec.Enforced, spec.Policy = "srt", true, policy
	spec.Writable = append([]string(nil), policy.Writable...)
	spec.Reason = "sandbox-runtime (srt): filesystem policy and network allowlist"
	spec.Srt = &SrtInfo{
		Path: n.Srt, ID: runID, Session: session,
		SettingsPath: filepath.Join(dir, runID+".json"),
		LogPath:      filepath.Join(dir, runID+".log"),
		Domains:      domains,
		Quiet:        append([]string{}, sc.Network.QuietDomains...),
		Settings:     SrtSettings(policy, nil, domains, sc.Network.DeniedDomains, false),
	}
	return ""
}

// prepareOS is prepare with orchestra's own backends.
func prepareOS(spec Spec, executor, tree string, sc Config, results *ProbeReport, o policyOpts) Spec {
	if results == nil {
		r := Probes()
		results = &r
	}
	policy := buildPolicy(tree, executor, sc, o)
	switch {
	case results.Seatbelt.OK:
		spec.Backend, spec.Enforced, spec.Reason = "seatbelt", true, "macOS Seatbelt (sandbox-exec)"
	case results.Bwrap.OK:
		spec.Backend, spec.Enforced, spec.Reason = "bwrap", true, "bubblewrap (user namespaces)"
		spec.Limits = bwrapLimits(policy, exists)
	case results.Landlock.OK:
		abi := results.Landlock.ABI
		limits := []string{"deny_read"}
		if abi < 6 {
			limits = append(limits, "signal")
		}
		if len(policy.Readonly) > 0 {
			limits = append(limits, "readonly")
		}
		var extra []string
		policy, extra = landlockPolicy(policy, realPath(tree))
		spec.Backend, spec.Enforced, spec.ABI = "landlock", true, abi
		spec.Limits = append(limits, extra...)
		spec.Reason = fmt.Sprintf("Landlock ABI %d (bwrap unavailable: %s)", abi, results.Bwrap.Detail)
	default:
		detail := backendDetail(*results, true)
		if sc.Mode == "required" {
			spec.Refuse = true
			spec.Reason = "sandbox.mode is required and no backend is usable (" + detail + ")"
		} else {
			spec.Reason = "no sandbox backend usable; running unsandboxed (" + detail + ")"
		}
		return spec
	}
	spec.Policy = policy
	spec.Writable = append([]string(nil), policy.Writable...)
	return spec
}

// landlockPolicy: Landlock cannot deny inside an allowed subtree, so every
// writable root other than the task tree that holds (or sits in) a
// tamper-protected path is dropped, and what stays unprotected is reported.
func landlockPolicy(policy *Policy, tree string) (*Policy, []string) {
	out := policy.clone()
	tamper := out.Tamper
	if len(tamper) == 0 {
		tamper = tamperPaths("")
	}
	kept := []string{}
	for _, w := range out.Writable {
		bad := false
		for _, t := range tamper {
			if overlaps(t, w) {
				bad = true
				break
			}
		}
		if w == tree || !bad {
			kept = append(kept, w)
		}
	}
	out.Writable = kept
	limits := []string{}
	piConfig := false
	for _, d := range out.DenyWrite {
		if contains(tamper, d) {
			continue
		}
		for _, w := range out.Writable {
			if under(d, w) {
				piConfig = true
			}
		}
	}
	if piConfig {
		limits = append(limits, "pi-config")
	}
	if len(out.DenyWriteRegex) > 0 {
		limits = append(limits, "orchestra-config")
	}
	return out, limits
}

// bwrapLimits: bwrap can only re-bind read-only what exists; a protected
// path that does not exist yet inside a writable root can be created.
func bwrapLimits(policy *Policy, exists func(string) bool) []string {
	tamper := policy.Tamper
	if len(tamper) == 0 {
		tamper = tamperPaths("")
	}
	var pi, tam bool
	for _, t := range policy.DenyWrite {
		if exists(t) {
			continue
		}
		inside := false
		for _, w := range policy.Writable {
			if under(t, w) {
				inside = true
				break
			}
		}
		if !inside {
			continue
		}
		if contains(tamper, t) {
			tam = true
		} else {
			pi = true
		}
	}
	limits := []string{}
	if pi {
		limits = append(limits, "pi-config")
	}
	if tam {
		limits = append(limits, "tamper")
	}
	return limits
}

// ChildEnv adds what a sandboxed child needs to env: uvx builds its
// throwaway tool envs in temp (never in ~/.local/share/uv/tools, run later
// unsandboxed; an explicit UV_TOOL_DIR is left alone), pnpm is pinned to
// the seeded store (else it silently moves its store into the tree; an
// explicit store dir, under either name, is kept), and under srt a Node child (pi) is told to honor srt's
// HTTP(S)_PROXY. Sandboxed or not, a child in a tree on pnpm's global
// virtual store gets its switch (Spec.PnpmGVS), and a child in a tree mise
// trusts gets it in MISE_TRUSTED_CONFIG_PATHS (config.TreeEntries).
func ChildEnv(s Spec, env []string) []string {
	out := append([]string(nil), env...)
	m := envMap(env)
	switch s.Backend {
	case "seatbelt", "bwrap", "landlock", "srt":
		if _, set := m["UV_TOOL_DIR"]; s.Policy != nil && !set {
			tmp := m["TMPDIR"]
			if tmp == "" {
				tmp = "/tmp"
			}
			out = append(out, "UV_TOOL_DIR="+filepath.Join(realPath(tmp), uvToolDirName))
		}
		if s.Policy != nil && s.Policy.PnpmStore != "" {
			out = append(out, config.PnpmStorePin(out, s.Policy.PnpmStore)...)
		}
	}
	if s.Backend == "srt" {
		if _, set := m["NODE_USE_ENV_PROXY"]; !set {
			out = append(out, "NODE_USE_ENV_PROXY=1")
		}
	}
	return config.WithTreeEnv(s.PnpmGVS, s.Tree, out)
}

// Unwrapped is the spec of a site that spawns nothing itself.
func Unwrapped(reason string) Spec {
	s := newSpec("")
	s.Reason = reason
	return s
}

// ReportView is the public projection of a Spec.
type ReportView struct {
	Backend  string   `json:"backend"`
	Enforced bool     `json:"enforced"`
	Reason   string   `json:"reason"`
	Limits   []string `json:"limits"`
	Writable []string `json:"writable"`
}

// Report is the spec's public projection (dispatch output, ledger).
func Report(s Spec) any {
	r := ReportView{Backend: s.Backend, Enforced: s.Enforced, Reason: s.Reason,
		Limits: s.Limits, Writable: s.Writable}
	if r.Limits == nil {
		r.Limits = []string{}
	}
	if r.Writable == nil {
		r.Writable = []string{}
	}
	return r
}

// IsNetworkNote reports whether a limit is the "network not restricted" note.
func IsNetworkNote(limit string) bool {
	return strings.HasPrefix(limit, "network not restricted")
}
