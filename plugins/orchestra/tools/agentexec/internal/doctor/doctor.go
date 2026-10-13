// Package doctor builds the readiness report behind `agent-exec doctor`: shim,
// permission rule, build, resolved config, per-executor readiness, a routing
// preview, cooldowns, the sandbox, and the hosts' network view.
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
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
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
	Build      *jsonx.Object
	Config     *jsonx.Object
	Executors  map[string]any
	Permission readiness.Permission
	Ready      map[string]readiness.ExecReady
	Route      []route.Route
	Cooldowns  *jsonx.Object
	Sandbox    any
	HerdrHost  any
	UserConfig *jsonx.Object
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
	activeObj := jsonx.NewObject()
	names := make([]string, 0, len(active))
	for k := range active {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		activeObj.Set(name, jsonx.NewObject().Set("reason", active[name].Reason).Set("until", active[name].Until))
	}

	configSection := jsonx.NewObject().
		Set("layers_found", []any{}).
		Set("config_root", absPath(config.ProjectBase())).
		Set("resolved", ok).
		Set("error", nil).
		Set("warnings", []any{}).
		Set("values", nil)
	if ok {
		layers := []any{}
		for _, l := range cfg.Layers {
			layers = append(layers, absPath(l.Path))
		}
		configSection.Set("layers_found", layers).Set("values", cfg.Values)
	} else if cfgErr != nil {
		configSection.Set("error", "agent-exec: "+cfgErr.Error())
	}

	cwd, _ := os.Getwd()
	r := Report{
		Shim:       ready.Shim,
		Build:      buildInfo(),
		Config:     configSection,
		Executors:  ready.Executors,
		Permission: ready.Permission,
		Ready:      ready.Ready,
		Cooldowns:  jsonx.NewObject().Set("path", cooldown.StatePath(dcfg)).Set("active", activeObj),
		Sandbox:    sandbox.DoctorSection(dcfg),
		HerdrHost:  herdr.HostReport(dcfg),
		UserConfig: jsonx.NewObject().
			Set("herdr", userConfig(dcfg, "herdr")).
			Set("orca", userConfig(dcfg, "orca")),
		CLINetwork: srt.CLINetwork(dcfg, cwd),
	}
	if ok {
		for _, cls := range previewClasses {
			r.Route = append(r.Route, route.Resolve(cfg, ready.Ready, cls, "default", nil, active))
		}
	}
	return r
}

// Object is the report in `doctor --json` key order.
func (r Report) Object() *jsonx.Object {
	routes := jsonx.NewObject()
	for _, rt := range r.Route {
		routes.Set(rt.Class, rt.Object())
	}
	return jsonx.NewObject().
		Set("shim", r.Shim).
		Set("build", r.Build).
		Set("config", r.Config).
		Set("executors", r.Executors).
		Set("permission", r.Permission).
		Set("ready", r.Ready).
		Set("route", routes).
		Set("cooldowns", r.Cooldowns).
		Set("sandbox", r.Sandbox).
		Set("herdr_host", r.HerdrHost).
		Set("user_config", r.UserConfig).
		Set("cli_network", r.CLINetwork)
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
func buildInfo() *jsonx.Object {
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
	return jsonx.NewObject().Set("binary", exe).Set("source_hash", hash).Set("go", runtime.Version())
}
