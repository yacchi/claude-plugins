package sandbox

import (
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
)

// Config is a well-formed `sandbox` section.
type Config struct {
	Mode       string
	AllowWrite []string
	DenyRead   []string
	Network    NetworkConfig
}

// NetworkConfig is a well-formed `sandbox.network`.
type NetworkConfig struct {
	Mode           string // allowlist | off
	Ecosystem      bool
	AllowedDomains []string
	DeniedDomains  []string
	QuietDomains   []string
}

// ConfigOf normalizes the sandbox section of cfg (nil: the defaults). The
// resolved config is normalized already; this re-applies the same rules so
// a hand-built section can never widen the sandbox.
func ConfigOf(cfg *config.Resolved) Config {
	var sec map[string]any
	if cfg == nil {
		sec = config.ObjectAt(config.Defaults(), "sandbox")
	} else {
		sec = cfg.Section("sandbox")
	}
	return normalizeConfig(sec)
}

// DenyRead is the effective deny_read of a CLI executor run under cfg:
// the default secret stores plus sandbox.deny_read, realpath'd.
func DenyRead(cfg *config.Resolved) []string {
	home := homeOf("")
	out := defaultDenyRead(home)
	for _, p := range ConfigOf(cfg).DenyRead {
		out = append(out, expand(p, home))
	}
	return dedupe(out)
}

func strList(v any) []string {
	list, _ := v.([]any)
	var out []string
	for _, e := range list {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func normalizeConfig(raw map[string]any) Config {
	c := Config{Mode: "auto"}
	switch m := mustGetNil(raw, "mode").(type) {
	case bool:
		if !m {
			c.Mode = "off" // an explicit `mode: false` means off
		}
	case string:
		if s := config.FoldHost(strings.TrimSpace(m)); contains(modes, s) {
			c.Mode = s
		}
	}
	c.AllowWrite = dedupe(strList(mustGetNil(raw, "allow_write")))
	c.DenyRead = dedupe(strList(mustGetNil(raw, "deny_read")))
	c.Network = normalizeNetwork(objOf(mustGetNil(raw, "network")))
	return c
}

func objOf(v any) map[string]any {
	o, _ := v.(map[string]any)
	return o
}

func mustGetNil(o map[string]any, k string) any {
	if o == nil {
		return nil
	}
	v := o[k]
	return v
}

func normalizeNetwork(raw map[string]any) NetworkConfig {
	n := NetworkConfig{Mode: "allowlist", Ecosystem: true}
	mode := ""
	switch m := mustGetNil(raw, "mode").(type) {
	case bool:
		if !m {
			mode = "off"
		}
	case string:
		mode = config.FoldHost(strings.TrimSpace(m))
	}
	if contains(networkModes, mode) {
		n.Mode = mode
	}
	switch e := mustGetNil(raw, "ecosystem").(type) {
	case bool:
		n.Ecosystem = e
	case string:
		switch config.FoldHost(strings.TrimSpace(e)) {
		case "off", "false", "no", "0":
			n.Ecosystem = false
		}
	}
	list := func(key string, def []string) []string {
		var values []string
		if raw != nil {
			if v, ok := raw[key]; ok {
				values = strList(v)
			} else {
				values = def
			}
		} else {
			values = def
		}
		var out []string
		for _, v := range values {
			if s := config.FoldHost(strings.TrimSpace(v)); s != "" {
				out = append(out, s)
			}
		}
		return dedupe(out)
	}
	n.AllowedDomains = list("allowed_domains", DefaultAllowedDomains)
	n.DeniedDomains = list("denied_domains", nil)
	n.QuietDomains = list("quiet_domains", DefaultQuietDomains)
	return n
}
