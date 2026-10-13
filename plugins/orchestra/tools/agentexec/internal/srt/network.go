// Package srt is the sandbox-runtime (srt) plumbing shared by the herdr
// host and the CLI executors (pi).
//
// srt confines a process with its own Seatbelt / bubblewrap profile, built
// from the settings file orchestra writes (sandbox.SrtSettings), and lets
// it reach only allowedDomains through srt's proxy. Run with -d, srt prints
// `[SandboxDebug] Connection blocked to <host>:<port>` lines on its stderr;
// with --control-fd 3 it reads one full-config JSON line per update, of
// which only the domain lists change live.
//
// Run is one process under srt: settings file (dir 0700, file 0600),
// control channel, debug-log tailing into <state>/audit/network.jsonl and
// the blocked hosts of this run, and the live push of learned domains. A
// CLI run's control channel is a pipe its parent holds; the herdr server
// outlives the process that started it, so its channel is a FIFO
// (<state>/herdr-srt/<session>.ctl) that any later process pushes to
// (PushLine).
//
// The package registers its network planner with sandbox (init), so
// sandbox.Prepare puts CLI runs under srt whenever this package is linked.
package srt

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/sandbox"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/tools"
)

func init() { sandbox.SetNetworkPlanner(planNetwork) }

// ecosystemPresets are the package registries a tree's tracked files say
// it needs, by basename. Registry hosts only.
var ecosystemPresets = []struct{ patterns, domains []string }{
	{[]string{"package.json", "package-lock.json", "pnpm-lock.yaml", "yarn.lock", "bun.lock", "bun.lockb"},
		[]string{"registry.npmjs.org"}},
	{[]string{"yarn.lock"}, []string{"registry.yarnpkg.com"}},
	{[]string{"pyproject.toml", "requirements*.txt", "uv.lock", "Pipfile", "setup.py"},
		[]string{"pypi.org", "files.pythonhosted.org"}},
	{[]string{"go.mod"}, []string{"proxy.golang.org", "sum.golang.org"}},
	{[]string{"Cargo.toml"}, []string{"index.crates.io", "static.crates.io", "crates.io"}},
	{[]string{"Gemfile"}, []string{"rubygems.org", "index.rubygems.org"}},
	{[]string{"pom.xml", "build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts"},
		[]string{"repo.maven.apache.org", "repo1.maven.org", "plugins.gradle.org", "services.gradle.org"}},
}

// piProviderDomains: pi reaches its model through the provider named by
// the model's prefix; used when pi's network_domains is unset.
var piProviderDomains = map[string][]string{
	"openai-codex":   {"chatgpt.com", "auth.openai.com"},
	"openai":         {"api.openai.com"},
	"anthropic":      {"api.anthropic.com"},
	"github-copilot": {"api.githubcopilot.com", "*.githubcopilot.com", "api.github.com"},
	"google":         {"generativelanguage.googleapis.com"},
}

const npmrcLimit = 1 << 16

var npmrcRe = regexp.MustCompile(`^\s*(?:@[^\s:=]+:)?registry\s*=\s*(\S+)`)

// StatusInfo says whether sandbox-runtime can run here: its path ("" when
// not), version, and why not.
type StatusInfo struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	Reason  string `json:"reason"`
}

// Status is ResolveStatus as the doctor/host reports carry it.
func Status(cfg *config.Resolved) any { return ResolveStatus(cfg) }

// ResolveStatus resolves srt under cfg. On Linux srt also needs bwrap and
// socat; the sandbox test hook (AGENT_EXEC_SANDBOX_BACKEND=none) makes a
// resolved srt unavailable.
func ResolveStatus(cfg *config.Resolved) StatusInfo {
	if os.Getenv(sandbox.ForceEnv) == "none" {
		return StatusInfo{Reason: "srt forced unavailable by " + sandbox.ForceEnv + "=none"}
	}
	info := tools.Resolve("srt", cfg)
	if info.Path == "" {
		reason := info.Reason
		if reason == "" {
			reason = "not installed"
		}
		return StatusInfo{Reason: "srt not found (" + reason + ")"}
	}
	return withLinuxDeps(StatusInfo{Path: info.Path, Version: info.Version}, runtime.GOOS, exec.LookPath)
}

func withLinuxDeps(s StatusInfo, goos string, which func(string) (string, error)) StatusInfo {
	if goos != "linux" {
		return s
	}
	var missing []string
	for _, b := range []string{"bwrap", "socat"} {
		if _, err := which(b); err != nil {
			missing = append(missing, b)
		}
	}
	if len(missing) > 0 {
		return StatusInfo{Version: s.Version, Reason: "srt needs " + strings.Join(missing, " and ") + " on Linux"}
	}
	return s
}

func dedupe(list []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, s := range list {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func cleanDomains(values []any) []string {
	var out []string
	for _, v := range values {
		if s, ok := v.(string); ok && sandbox.DomainRefusal(s) == "" {
			out = append(out, strings.ToLower(strings.TrimSpace(s)))
		}
	}
	return dedupe(out)
}

func gitOut(dir string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return "", false
	}
	return string(out), true
}

// npmrcHosts are the hosts of every registry= / @scope:registry= URL in an
// .npmrc; anything DomainRefusal refuses is skipped.
func npmrcHosts(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return []string{}
	}
	defer f.Close()
	buf := make([]byte, npmrcLimit)
	n, _ := f.Read(buf)
	text := strings.ToValidUTF8(string(buf[:n]), string(utf8.RuneError))
	var hosts []string
	for _, line := range strings.Split(strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n"), "\n") {
		if t := strings.TrimLeft(line, " \t\v\f"); strings.HasPrefix(t, "#") || strings.HasPrefix(t, ";") {
			continue
		}
		m := npmrcRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		raw := strings.Trim(m[1], `"'`)
		if !strings.Contains(raw, "://") {
			raw = "//" + raw
		}
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		host := strings.ToLower(u.Hostname())
		if host != "" && sandbox.DomainRefusal(host) == "" {
			hosts = append(hosts, host)
		}
	}
	return dedupe(hosts)
}

// EcosystemDomains are the registry hosts for the tracked files of the
// repository holding tree (`git ls-files`, basename match; untracked and
// ignored files do not count). Only a tracked root .npmrc is read.
func EcosystemDomains(tree string) []string {
	out, _ := gitOut(tree, "rev-parse", "--show-toplevel")
	root := strings.TrimSpace(out)
	if root == "" {
		return []string{}
	}
	listing, _ := gitOut(root, "ls-files", "-z")
	if listing == "" {
		return []string{}
	}
	names := map[string]bool{}
	npmrc := false
	for _, p := range strings.Split(listing, "\x00") {
		if p == "" {
			continue
		}
		if p == ".npmrc" {
			npmrc = true
		}
		names[p[strings.LastIndex(p, "/")+1:]] = true
	}
	var domains []string
	for _, preset := range ecosystemPresets {
		for _, pattern := range preset.patterns {
			hit := false
			if strings.Contains(pattern, "*") {
				for n := range names {
					if ok, _ := filepath.Match(pattern, n); ok {
						hit = true
						break
					}
				}
			} else {
				hit = names[pattern]
			}
			if hit {
				domains = append(domains, preset.domains...)
				break
			}
		}
	}
	if npmrc {
		domains = append(domains, npmrcHosts(filepath.Join(root, ".npmrc"))...)
	}
	return dedupe(domains)
}

// ExecutorDomains are the API hosts of one CLI executor run: its
// network_domains when set, else (pi) the hosts of the model's provider.
func ExecutorDomains(executor string, ecfg *jsonx.Object, model string) []string {
	if ecfg != nil {
		if v, ok := ecfg.Get("network_domains"); ok {
			if list, isList := v.([]any); isList {
				return cleanDomains(list)
			}
		}
	}
	if executor == "pi" {
		prefix := ""
		if i := strings.Index(model, "/"); i >= 0 {
			prefix = model[:i]
		}
		return append([]string{}, piProviderDomains[strings.ToLower(strings.TrimSpace(prefix))]...)
	}
	return []string{}
}

var unsafeID = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// RunID is the settings/log name of one CLI run: the task id made
// path-safe, or a random id.
func RunID(task string) string {
	if strings.TrimSpace(task) != "" {
		safe := unsafeID.ReplaceAllString(strings.TrimSpace(task), "_")
		if len(safe) > 80 {
			safe = safe[:80]
		}
		if safe = strings.TrimLeft(safe, "."); safe != "" {
			return safe
		}
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func executorConfig(cfg *config.Resolved, executor string) *jsonx.Object {
	var ext *jsonx.Object
	if cfg == nil {
		ext = config.Defaults().Object("external_executors")
	} else {
		ext = cfg.Section("external_executors")
	}
	return ext.Object(executor)
}

// planNetwork is the network of one CLI executor run (Python's
// cli_network); the domains are computed only when srt would be used.
func planNetwork(cfg *config.Resolved, executor, tree, model, task string) *sandbox.Network {
	net := sandbox.ConfigOf(cfg).Network
	rid := RunID(task)
	session := task
	if session == "" {
		session = rid
	}
	out := &sandbox.Network{Domains: []string{}, ID: rid, Session: session}
	if net.Mode != "allowlist" {
		out.Reason = "sandbox.network.mode is off"
		return out
	}
	st := ResolveStatus(cfg)
	out.Srt, out.Reason = st.Path, st.Reason
	if st.Path != "" {
		var presets []string
		if net.Ecosystem {
			presets = EcosystemDomains(tree)
		}
		domains := append(append(append([]string{}, net.AllowedDomains...), presets...),
			ExecutorDomains(executor, executorConfig(cfg, executor), model)...)
		out.Domains = dedupe(domains)
	}
	return out
}

func standardModel(ecfg *jsonx.Object) string {
	policy := ecfg.Object("class_policy").Object("standard")
	m, _ := mustGet(policy, "model").(string)
	return m
}

func mustGet(o *jsonx.Object, k string) any {
	if o == nil {
		return nil
	}
	v, _ := o.Get(k)
	return v
}

// CLINetwork is doctor's view of the CLI executors' network for a run in
// cwd: the tree's registry presets and, per executor, whether srt would
// restrict it (else why not) and its own API hosts (pi: for the model of
// its standard class).
func CLINetwork(cfg *config.Resolved, cwd string) any {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	net := sandbox.ConfigOf(cfg).Network
	presets := []string{}
	if net.Ecosystem {
		presets = EcosystemDomains(cwd)
	}
	names := make([]string, 0, len(tools.KnownBinaries))
	for name := range tools.KnownBinaries {
		names = append(names, name)
	}
	sort.Strings(names)
	executors := jsonx.NewObject()
	for _, name := range names {
		ecfg := executorConfig(cfg, name)
		model := standardModel(ecfg)
		spec := sandbox.Prepare(name, cwd, cfg, model, "")
		reason := ""
		note := ""
		for _, l := range spec.Limits {
			if sandbox.IsNetworkNote(l) {
				note = l
				break
			}
		}
		switch {
		case note != "":
			reason = note
		case spec.Backend != "srt":
			reason = spec.Reason
		}
		executors.Set(name, jsonx.NewObject().
			Set("backend", spec.Backend).
			Set("restricted", spec.Backend == "srt").
			Set("reason", reason).
			Set("domains", ExecutorDomains(name, ecfg, model)))
	}
	return jsonx.NewObject().
		Set("mode", net.Mode).
		Set("ecosystem", net.Ecosystem).
		Set("preset_domains", presets).
		Set("executors", executors)
}
