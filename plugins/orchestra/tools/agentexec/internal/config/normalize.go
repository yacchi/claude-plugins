package config

import (
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
)

var sandboxModes = []string{"auto", "required", "off"}

// union accumulates the lists that are UNIONED across layers instead of
// replaced: sandbox.allow_write/deny_read, the sandbox.network domain lists
// (seeded from the defaults, so a layer can add a domain but never drop
// api.anthropic.com) and each executor's network_domains.
type union struct {
	sandbox         map[string][]string
	network         map[string][]string
	executorDomains *jsonx.Object // name -> []string, first-seen order
}

func newUnion() *union {
	return &union{
		sandbox: map[string][]string{"allow_write": nil, "deny_read": nil},
		network: map[string][]string{
			"allowed_domains": append([]string(nil), DefaultAllowedDomains...),
			"denied_domains":  nil,
			"quiet_domains":   append([]string(nil), DefaultQuietDomains...),
		},
		executorDomains: jsonx.NewObject(),
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

func (u *union) add(layer *jsonx.Object) {
	if sb := layer.Object("sandbox"); sb != nil {
		for key := range u.sandbox {
			v, _ := sb.Get(key)
			u.sandbox[key] = appendNew(u.sandbox[key], v)
		}
		if nw := sb.Object("network"); nw != nil {
			for key := range u.network {
				v, _ := nw.Get(key)
				u.network[key] = appendNew(u.network[key], v)
			}
		}
	}
	ext := layer.Object("external_executors")
	for _, name := range ext.Keys() {
		ecfg := ext.Object(name)
		values, _ := ecfg.Get("network_domains")
		if _, ok := values.([]any); !ok {
			continue
		}
		cur, _ := u.executorDomains.Get(name)
		acc, _ := cur.([]string)
		if acc == nil {
			acc = []string{} // no executor ships default network_domains
		}
		u.executorDomains.Set(name, appendNew(acc, values))
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
func normalize(values *jsonx.Object, u *union) {
	values.Set("sandbox", normalizeSandbox(values.Object("sandbox"), u))
	if ext := values.Object("external_executors"); ext != nil {
		for _, name := range u.executorDomains.Keys() {
			if ecfg := ext.Object(name); ecfg != nil {
				d, _ := u.executorDomains.Get(name)
				ecfg.Set("network_domains", toAny(d.([]string)))
			}
		}
	}
	normalizeEnforcement(values)
}

func normalizeSandbox(raw *jsonx.Object, u *union) *jsonx.Object {
	mode := "auto"
	switch m, _ := raw.Get("mode"); x := m.(type) {
	case bool:
		if !x {
			mode = "off" // YAML 1.1 loads a bareword `off` as false
		}
	case string:
		if s := strings.ToLower(strings.TrimSpace(x)); contains(sandboxModes, s) {
			mode = s
		}
	}
	out := jsonx.NewObject().Set("mode", mode)
	for _, key := range []string{"allow_write", "deny_read"} {
		out.Set(key, dedupe(u.sandbox[key]))
	}
	out.Set("network", normalizeNetwork(raw.Object("network"), u))
	return out
}

func normalizeNetwork(raw *jsonx.Object, u *union) *jsonx.Object {
	mode := "allowlist"
	switch m, _ := raw.Get("mode"); x := m.(type) {
	case bool:
		if !x {
			mode = "off"
		}
	case string:
		if strings.ToLower(strings.TrimSpace(x)) == "off" {
			mode = "off"
		}
	}
	ecosystem := true
	switch e, ok := raw.Get("ecosystem"); x := e.(type) {
	case bool:
		ecosystem = !ok || x
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "off", "false", "no", "0":
			ecosystem = false
		}
	}
	out := jsonx.NewObject().Set("mode", mode).Set("ecosystem", ecosystem)
	for _, key := range []string{"allowed_domains", "denied_domains", "quiet_domains"} {
		var norm []string
		for _, v := range u.network[key] {
			if s := strings.ToLower(strings.TrimSpace(v)); s != "" {
				norm = append(norm, s)
			}
		}
		out.Set(key, dedupe(norm))
	}
	return out
}

func isOff(v any) bool {
	switch x := v.(type) {
	case bool:
		return !x
	case string:
		return strings.ToLower(strings.TrimSpace(x)) == "off"
	}
	return false
}

func normalizeEnforcement(values *jsonx.Object) {
	enf := values.Object("enforcement")
	if enf == nil {
		enf = jsonx.NewObject()
		values.Set("enforcement", enf)
	}
	// light_class fails safe to "off": an ambiguous value must never mean
	// "block". The other guards fail safe to ON: only an explicit off
	// (string, or the bareword YAML 1.1 loads as false) disables them.
	if v, _ := enf.Get("light_class"); v != "block" {
		enf.Set("light_class", "off")
	}
	pick := func(key, off, on string) {
		v, _ := enf.Get(key)
		if isOff(v) {
			enf.Set(key, off)
		} else {
			enf.Set(key, on)
		}
	}
	pick("worker_vcs", "off", "block")
	switch v, _ := enf.Get("turn_edits"); x := v.(type) {
	case int64:
		if x <= 0 {
			enf.Set("turn_edits", int64(8))
		}
	default:
		if isOff(x) {
			enf.Set("turn_edits", "off")
		} else {
			enf.Set("turn_edits", int64(8))
		}
	}
	pick("opus_generalist", "off", "nudge")
	pick("session_cleanup", "off", "on")
}
