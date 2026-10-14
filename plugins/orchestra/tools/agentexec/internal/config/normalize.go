package config

import (
	"maps"
	"slices"
	"strings"
)

var sandboxModes = []string{"auto", "required", "off"}

// union accumulates the lists that are UNIONED across layers instead of
// replaced: sandbox.allow_write/deny_read, the sandbox.network domain lists
// (seeded from the defaults, so a layer can add a domain but never drop
// api.anthropic.com) and each executor's network_domains.
type union struct {
	sandbox         map[string][]string
	network         map[string][]string
	executorDomains map[string]any // name -> []string, first-seen order
}

func newUnion() *union {
	return &union{
		sandbox: map[string][]string{"allow_write": nil, "deny_read": nil},
		network: map[string][]string{
			"allowed_domains": append([]string(nil), DefaultAllowedDomains...),
			"denied_domains":  nil,
			"quiet_domains":   append([]string(nil), DefaultQuietDomains...),
		},
		executorDomains: map[string]any{},
	}
}

func appendNew(acc []string, values any) []string {
	list, ok := values.([]any)
	if !ok {
		return acc
	}
	for _, v := range list {
		s, ok := v.(string)
		if ok && !contains(acc, s) {
			acc = append(acc, s)
		}
	}
	return acc
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func (u *union) add(layer map[string]any) {
	if sb := ObjectAt(layer, "sandbox"); sb != nil {
		for key := range u.sandbox {
			v := sb[key]
			u.sandbox[key] = appendNew(u.sandbox[key], v)
		}
		if nw := ObjectAt(sb, "network"); nw != nil {
			for key := range u.network {
				v := nw[key]
				u.network[key] = appendNew(u.network[key], v)
			}
		}
	}
	ext := ObjectAt(layer, "external_executors")
	for _, name := range slices.Sorted(maps.Keys(ext)) {
		ecfg := ObjectAt(ext, name)
		values := ecfg["network_domains"]
		if _, ok := values.([]any); !ok {
			continue
		}
		cur := u.executorDomains[name]
		acc, _ := cur.([]string)
		if acc == nil {
			acc = []string{} // no executor ships default network_domains
		}
		u.executorDomains[name] = appendNew(acc, values)
	}
}

func toAny(list []string) []any {
	out := make([]any, len(list))
	for i, s := range list {
		out[i] = s
	}
	return out
}

func dedupe(list []string) []any {
	var out []string
	for _, s := range list {
		if s != "" && !contains(out, s) {
			out = append(out, s)
		}
	}
	return toAny(out)
}

// normalize applies the post-merge rules of Python's resolve_config, in
// place: the sandbox section is rebuilt from the unions, executor
// network_domains are replaced by their unions, and the enforcement knobs
// are coerced to their known values (fail-safe per knob).
func normalize(values map[string]any, u *union) {
	values["sandbox"] = normalizeSandbox(ObjectAt(values, "sandbox"), u)
	if ext := ObjectAt(values, "external_executors"); ext != nil {
		for _, name := range slices.Sorted(maps.Keys(u.executorDomains)) {
			if ecfg := ObjectAt(ext, name); ecfg != nil {
				d := u.executorDomains[name]
				ecfg["network_domains"] = toAny(d.([]string))
			}
		}
	}
	normalizeEnforcement(values)
}

func normalizeSandbox(raw map[string]any, u *union) map[string]any {
	mode := "auto"
	switch m, _ := raw["mode"]; x := m.(type) {
	case bool:
		if !x {
			mode = "off" // an explicit `mode: false` means off
		}
	case string:
		if s := PyLower(strings.TrimSpace(x)); contains(sandboxModes, s) {
			mode = s
		}
	}
	out := map[string]any{"mode": mode}
	for _, key := range []string{"allow_write", "deny_read"} {
		out[key] = dedupe(u.sandbox[key])
	}
	out["network"] = normalizeNetwork(ObjectAt(raw, "network"), u)
	return out
}

func normalizeNetwork(raw map[string]any, u *union) map[string]any {
	mode := "allowlist"
	switch m, _ := raw["mode"]; x := m.(type) {
	case bool:
		if !x {
			mode = "off"
		}
	case string:
		if PyLower(strings.TrimSpace(x)) == "off" {
			mode = "off"
		}
	}
	ecosystem := true
	switch e, ok := raw["ecosystem"]; x := e.(type) {
	case bool:
		ecosystem = !ok || x
	case string:
		switch PyLower(strings.TrimSpace(x)) {
		case "off", "false", "no", "0":
			ecosystem = false
		}
	}
	out := map[string]any{"mode": mode, "ecosystem": ecosystem}
	for _, key := range []string{"allowed_domains", "denied_domains", "quiet_domains"} {
		var norm []string
		for _, v := range u.network[key] {
			if s := PyLower(strings.TrimSpace(v)); s != "" {
				norm = append(norm, s)
			}
		}
		out[key] = dedupe(norm)
	}
	return out
}

func isOff(v any) bool {
	switch x := v.(type) {
	case bool:
		return !x
	case string:
		return PyLower(strings.TrimSpace(x)) == "off"
	}
	return false
}

func normalizeEnforcement(values map[string]any) {
	enf := ObjectAt(values, "enforcement")
	if enf == nil {
		enf = map[string]any{}
		values["enforcement"] = enf
	}
	// light_class fails safe to "off": an ambiguous value must never mean
	// "block". The other guards fail safe to ON: only an explicit off
	// ("off", or false) disables them.
	if v, _ := enf["light_class"]; v != "block" {
		enf["light_class"] = "off"
	}
	pick := func(key, off, on string) {
		v := enf[key]
		if isOff(v) {
			enf[key] = off
		} else {
			enf[key] = on
		}
	}
	pick("worker_vcs", "off", "block")
	switch v, _ := enf["turn_edits"]; x := v.(type) {
	case int64:
		if x <= 0 {
			enf["turn_edits"] = int64(8)
		}
	default:
		if isOff(x) {
			enf["turn_edits"] = "off"
		} else {
			enf["turn_edits"] = int64(8)
		}
	}
	pick("opus_generalist", "off", "nudge")
	pick("session_cleanup", "off", "on")
}
