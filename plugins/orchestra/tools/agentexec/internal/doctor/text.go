package doctor

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/sandbox"
)

// FormatText is the human view of the report (`doctor --text`).
func FormatText(r Report) string {
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	p("agent-exec doctor")
	p("-----------------")
	if r.Shim.Installed {
		p("shim: installed at %s (on PATH: %s)", pyStr(deref(r.Shim.Path)), pyStr(r.Shim.OnPath))
		if r.Shim.Target != nil {
			p("  target: %s (%s)", *r.Shim.Target, pyStr(deref(r.Shim.AnchoredTo)))
		}
	} else {
		p("shim: NOT installed")
	}

	p("build: binary=%s source_hash=%s go=%s", pyStr(get(r.Build, "binary")),
		pyStr(get(r.Build, "source_hash")), pyStr(get(r.Build, "go")))

	cfg := r.Config
	if get(cfg, "resolved") == true {
		layers := list(get(cfg, "layers_found"))
		p("config: resolved (%d layer(s) found)", len(layers))
		for _, l := range layers {
			p("  - %s", pyStr(l))
		}
	} else {
		p("config: FAILED to resolve: %s", pyStr(get(cfg, "error")))
	}

	p("permission rule %s: %s", r.Permission.Rule, map[bool]string{true: "found", false: "NOT found"}[r.Permission.Found])
	for _, s := range r.Permission.Sources {
		p("  - %s", s)
	}

	if len(r.Executors) > 0 {
		p("executors:")
		for _, name := range sortedKeys(r.Executors) {
			info, _ := r.Executors[name].(map[string]any)
			p("  - %s: enabled=%s dispatch=%s binary=%s available=%s", name,
				pyStr(get(info, "enabled")), pyStr(get(info, "dispatch")),
				pyStr(get(info, "binary")), pyStr(get(info, "available")))
			if note, _ := get(info, "note").(string); note != "" {
				p("      note: %s", note)
			}
		}
	} else {
		p("executors: none known")
	}

	if len(r.Ready) > 0 {
		p("ready:")
		names := make([]string, 0, len(r.Ready))
		for n := range r.Ready {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, name := range names {
			info := r.Ready[name]
			status := "OK"
			if !info.OK {
				status = "NOT READY"
			}
			missing := ""
			if len(info.Missing) > 0 {
				missing = " (missing: " + strings.Join(info.Missing, ", ") + ")"
			}
			p("  - %s: %s%s", name, status, missing)
		}
	}

	if len(r.Route) > 0 {
		p("route (preview):")
		for _, rt := range r.Route {
			if rt.Executor != "" {
				p("  - %s: %s (dispatch=%s model=%s effort=%s source=%s)", rt.Class, rt.Executor,
					pyStr(rt.Dispatch), pyStr(rt.Model), pyStr(rt.Effort), rt.Source)
			} else {
				p("  - %s: UNROUTABLE", rt.Class)
			}
		}
	}

	if active := obj(get(r.Cooldowns, "active")); len(active) > 0 {
		p("cooldowns:")
		for _, name := range slices.Sorted(maps.Keys(active)) {
			e := obj(get(active, name))
			reason := get(e, "reason")
			if reason == nil {
				reason = "unknown"
			}
			p("  - %s: %s (until %s)", name, pyStr(reason), pyStr(get(e, "until")))
		}
	}

	sb := norm(r.Sandbox)
	if len(sb) > 0 {
		p("sandbox: mode=%s backend=%s enforced=%s (%s)", pyStr(get(sb, "mode")),
			pyStr(get(sb, "backend")), pyStr(get(sb, "enforced")), pyStr(get(sb, "reason")))
		probes := obj(get(sb, "probes"))
		for _, name := range config.OrderedKeys(probes, sandbox.ProbeOrder) {
			probe := obj(get(probes, name))
			state := "unavailable"
			if truthy(get(probe, "ok")) {
				state = "ok"
			}
			p("  - %s: %s (%s)", name, state, pyStr(get(probe, "detail")))
		}
		host := norm(r.HerdrHost)
		p("herdr host: home=%s sandboxed=%s (%s)", pyStr(get(host, "home")),
			pyStr(get(host, "sandboxed")), pyStr(get(host, "reason")))
		uc := r.UserConfig
		p("herdr host: user_config=%s", pyStr(get(uc, "herdr")))
		p("orca host: user_config=%s", pyStr(get(uc, "orca")))
		if net := obj(get(host, "network")); len(net) > 0 {
			srt := obj(get(net, "srt"))
			path := get(srt, "path")
			if !truthy(path) {
				path = "-"
			}
			version := ""
			if v := get(srt, "version"); truthy(v) {
				version = " (" + pyStr(v) + ")"
			}
			reason := ""
			if v := get(net, "reason"); truthy(v) {
				reason = "  " + pyStr(v)
			}
			p("  network: mode=%s restricted=%s srt=%s%s%s", pyStr(get(net, "mode")),
				pyStr(get(net, "restricted")), pyStr(path), version, reason)
		}
		learned := obj(get(sb, "learned"))
		count := int64(0)
		if n, ok := get(learned, "count").(int64); ok {
			count = n
		}
		p("  learned grants: %d (%s)", count, pyStr(get(learned, "path")))
		if w := get(learned, "warning"); truthy(w) {
			p("  WARNING: %s", pyStr(w))
		}
		if cli := norm(r.CLINetwork); len(cli) > 0 {
			presets := joinList(get(cli, "preset_domains"))
			if presets == "" {
				presets = "-"
			}
			p("CLI executors network: mode=%s presets=%s", pyStr(get(cli, "mode")), presets)
			execs := obj(get(cli, "executors"))
			for _, name := range sortedStrings(slices.Sorted(maps.Keys(execs))) {
				item := obj(get(execs, name))
				domains := joinList(get(item, "domains"))
				if domains == "" {
					domains = "-"
				}
				reason := ""
				if v := get(item, "reason"); truthy(v) {
					reason = "  " + pyStr(v)
				}
				p("  - %s: restricted=%s backend=%s domains=%s%s", name,
					pyStr(get(item, "restricted")), pyStr(get(item, "backend")), domains, reason)
			}
		}
	}
	return b.String()
}

// norm round-trips a section through JSON so FormatText only handles the
// decoded shapes (map[string]any, []any, string, bool, int64, float64, nil).
func norm(v any) map[string]any {
	b, err := fsutil.MarshalJSON(v)
	if err != nil {
		return map[string]any{}
	}
	d, err := fsutil.DecodeJSON(b)
	if err != nil {
		return map[string]any{}
	}
	return obj(d)
}

func obj(v any) map[string]any {
	o, _ := v.(map[string]any)
	return o
}

func get(o map[string]any, k string) any {
	v := o[k]
	return v
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

func joinList(v any) string {
	var parts []string
	for _, e := range list(v) {
		parts = append(parts, pyStr(e))
	}
	return strings.Join(parts, ", ")
}

func deref(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedStrings(s []string) []string {
	sort.Strings(s)
	return s
}

// truthy is Python truthiness for decoded JSON values.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int64:
		return x != 0
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// pyStr formats a value the way Python's %s does.
func pyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		return x
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	case float64:
		return fsutil.FloatRepr(x)
	}
	return fmt.Sprint(v)
}
