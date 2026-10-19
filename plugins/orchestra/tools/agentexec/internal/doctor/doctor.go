// Package doctor builds the readiness report behind `agent-exec doctor`: shim,
// permission rule, build, resolved config, per-executor readiness, a routing
// preview, cooldowns, subscription usage limits, the sandbox, and the hosts'
// network view.
package doctor

import (
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/cooldown"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host/herdr"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/quota"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/readiness"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/route"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/sandbox"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/srt"
)

// previewClasses are the classes the report previews a route for.
var previewClasses = []string{"light", "standard", "deep", "review"}

// Report is the doctor report. Object renders it in `doctor --json` key
// order.
type Report struct {
	Shim       readiness.Shim
	Build      map[string]any
	Config     map[string]any
	Executors  map[string]any
	Permission readiness.Permission
	Ready      map[string]readiness.ExecReady
	Route      []route.Route
	Cooldowns  map[string]any
	Usage      map[string]any
	Sandbox    any
	HerdrHost  any
	UserConfig map[string]any
	CLINetwork any
}

// Build gathers the report. cfgErr is the error from config.Resolve; the
// built-in defaults then describe the machine and the config-derived
// sections are empty.
func Build(cfg *config.Resolved, cfgErr error) Report {
	ok := cfgErr == nil && cfg != nil
	dcfg := cfg
	if !ok {
		dcfg = &config.Resolved{Values: config.Defaults(), Root: config.ProjectBase()}
	}

	var ready readiness.Report
	if ok {
		ready = readiness.Compute(cfg)
		annotatePiLogin(ready.Executors, cfg)
	} else {
		ready = readiness.Report{
			Shim:       readiness.FindShim(),
			Permission: readiness.ScanPermission(),
			Executors:  map[string]any{},
			Ready:      map[string]readiness.ExecReady{},
		}
	}

	var active map[string]cooldown.Entry
	if cooldown.Enabled(dcfg) {
		active = cooldown.Active(dcfg, time.Now())
	}
	activeObj := map[string]any{}
	names := make([]string, 0, len(active))
	for k := range active {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		activeObj[name] = map[string]any{"reason": active[name].Reason, "until": active[name].Until}
	}

	var usage map[string]quota.Entry
	usageObj := map[string]any{"enabled": false}
	if quota.Enabled(dcfg) {
		usage = quota.Snapshot(dcfg, quota.Groups, time.Now())
		groups := map[string]any{}
		for _, g := range quota.Groups {
			if e, ok := usage[g]; ok {
				groups[g] = e.Object()
			} else {
				groups[g] = nil
			}
		}
		thresholds := map[string]any{}
		for w, n := range quota.Thresholds(dcfg) {
			thresholds[w] = n
		}
		usageObj = map[string]any{"enabled": true, "thresholds": thresholds, "groups": groups}
	}

	configSection := map[string]any{
		"layers_found": []any{},
		"config_root":  absPath(config.ProjectBase()),
		"resolved":     ok,
		"error":        nil,
		"warnings":     []any{},
		"values":       nil,
	}
	if ok {
		layers := []any{}
		for _, l := range cfg.Layers {
			layers = append(layers, absPath(l.Path))
		}
		configSection["layers_found"] = layers
		configSection["values"] = cfg.Values
	} else if cfgErr != nil {
		configSection["error"] = "agent-exec: " + cfgErr.Error()
	}

	cwd, _ := os.Getwd()
	r := Report{
		Shim:       ready.Shim,
		Build:      buildInfo(),
		Config:     configSection,
		Executors:  ready.Executors,
		Permission: ready.Permission,
		Ready:      ready.Ready,
		Cooldowns:  map[string]any{"path": cooldown.StatePath(dcfg), "active": activeObj},
		Usage:      usageObj,
		Sandbox:    sandbox.DoctorSection(dcfg),
		HerdrHost:  herdr.HostReport(dcfg),
		UserConfig: map[string]any{
			"herdr": userConfig(dcfg, "herdr"),
			"orca":  userConfig(dcfg, "orca"),
		},
		CLINetwork: srt.CLINetwork(dcfg, cwd),
	}
	if ok {
		for _, cls := range previewClasses {
			r.Route = append(r.Route, route.Resolve(cfg, ready.Ready, cls, "default", nil, active, usage))
		}
	}
	return r
}

// Object is the report in `doctor --json` key order.
func (r Report) Object() map[string]any {
	routes := map[string]any{}
	for _, rt := range r.Route {
		routes[rt.Class] = rt.Object()
	}
	return map[string]any{
		"shim":         r.Shim,
		"build":        r.Build,
		"config":       r.Config,
		"executors":    r.Executors,
		"permission":   r.Permission,
		"ready":        r.Ready,
		"route":        routes,
		"cooldowns":    r.Cooldowns,
		"usage_limits": r.Usage,
		"sandbox":      r.Sandbox,
		"herdr_host":   r.HerdrHost,
		"user_config":  r.UserConfig,
		"cli_network":  r.CLINetwork,
	}
}

func userConfig(cfg *config.Resolved, section string) string {
	if v, ok := cfg.String(section, "user_config"); ok && v != "" {
		return v
	}
	v, _ := (&config.Resolved{Values: config.Defaults()}).String(section, "user_config")
	return v
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// buildInfo says which binary is answering: its path, the source hash it was
// built from (the bootstrap's cache directory name, else the VCS revision
// stamped into the binary, else null) and the Go toolchain.
func buildInfo() map[string]any {
	var exe, hash any
	if p, err := os.Executable(); err == nil {
		exe = p
		dir := filepath.Dir(p)
		if filepath.Base(filepath.Dir(dir)) == "agent-exec" &&
			filepath.Base(filepath.Dir(filepath.Dir(dir))) == "claude-orchestra" {
			hash = filepath.Base(dir)
		}
	}
	if hash == nil {
		if bi, ok := debug.ReadBuildInfo(); ok {
			for _, s := range bi.Settings {
				if s.Key == "vcs.revision" && s.Value != "" {
					hash = s.Value
				}
			}
		}
	}
	return map[string]any{"binary": exe, "source_hash": hash, "go": runtime.Version()}
}
