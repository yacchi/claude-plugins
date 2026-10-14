// Package tools resolves (and, see status.go/install.go, reports and
// installs) the executor binaries orchestra pins under its state dir, so a
// user's personal install cannot change worker behavior.
package tools

import (
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// KnownBinaries maps every executor agent-exec drives to its binary name.
var KnownBinaries = map[string]string{"pi": "pi"}

// InstallHint is the remedy printed next to a missing or stale tool.
const InstallHint = "run: agent-exec tools install "

// Resolution says where a tool runs from. Empty strings stand for Python's
// None. Source is "managed" or "system"; Reason is managed-not-installed |
// managed-stale | not-on-path | not-installed.
type Resolution struct {
	Name    string
	Path    string
	Source  string
	Version string
	Wanted  string
	Reason  string
	Hint    string
}

// ToolConfig is the normalized `tools.<name>` entry. A tool absent from the
// config is a plain system binary.
type ToolConfig struct {
	Source  string // auto | managed | system
	Spec    string
	Version string
	Bin     string
}

// Root is <state dir>/tools.
func Root() string { return filepath.Join(paths.StateDir(), "tools") }

// ManifestPath is <state dir>/tools/manifest.json.
func ManifestPath() string { return filepath.Join(Root(), "manifest.json") }

// LoadManifest returns the installed-tool records; a missing or corrupt
// manifest is empty.
func LoadManifest() map[string]map[string]any {
	out := map[string]map[string]any{}
	raw, err := os.ReadFile(ManifestPath())
	if err != nil {
		return out
	}
	v, err := fsutil.DecodeJSON(raw)
	if err != nil {
		return out
	}
	root, _ := v.(map[string]any)
	tools := config.ObjectAt(root, "tools")
	for _, k := range slices.Sorted(maps.Keys(tools)) {
		if rec := config.ObjectAt(tools, k); rec != nil {
			out[k] = rec
		}
	}
	return out
}

func section(cfg *config.Resolved) map[string]any {
	if cfg == nil {
		return config.ObjectAt(config.Defaults(), "tools")
	}
	return cfg.Section("tools")
}

// Names lists the tools configured under `tools:` (entries that are objects).
func Names(cfg *config.Resolved) []string {
	sec := section(cfg)
	var out []string
	for _, k := range config.OrderedKeys(sec, config.ToolOrder) {
		if config.ObjectAt(sec, k) != nil {
			out = append(out, k)
		}
	}
	return out
}

// ConfigFor normalizes the config entry of name; defaultBin is used when the
// entry names no binary (the tool name itself when empty).
func ConfigFor(name string, cfg *config.Resolved, defaultBin string) ToolConfig {
	if defaultBin == "" {
		defaultBin = name
	}
	entry := config.ObjectAt(section(cfg), name)
	if entry == nil {
		return ToolConfig{Source: "system", Bin: defaultBin}
	}
	tc := ToolConfig{Source: "auto", Bin: defaultBin}
	if s, _ := entry["source"]; s == "managed" || s == "system" || s == "auto" {
		tc.Source = s.(string)
	}
	if s, ok := entry["spec"]; ok {
		tc.Spec, _ = s.(string)
	}
	if v, ok := entry["version"]; ok && v != nil && v != "" {
		tc.Version = pyStr(v)
	}
	if b, _ := entry["bin"]; b != nil {
		if s, ok := b.(string); ok && s != "" {
			tc.Bin = s
		}
	}
	return tc
}

// pyStr is Python's str() for the scalar types a YAML version can load as.
func pyStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		if x {
			return "True"
		}
		return "False"
	}
	b, err := fsutil.MarshalJSON(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func executable(p string) bool {
	if p == "" || !filepath.IsAbs(p) {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0
}

func recordState(rec map[string]any, tc ToolConfig) string {
	if rec == nil {
		return "none"
	}
	spec := rec["spec"]
	version := rec["version"]
	if spec != nilIfEmpty(tc.Spec) || version != nilIfEmpty(tc.Version) {
		return "stale"
	}
	if p, _ := rec["path"]; !executable(asString(p)) {
		return "stale"
	}
	return "valid"
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

// Resolve decides where name runs from under cfg (nil means the defaults):
// a valid managed install, else (source auto) a stale but runnable one with
// a hint, else the binary on PATH, else nothing with a reason.
func Resolve(name string, cfg *config.Resolved) Resolution {
	tc := ConfigFor(name, cfg, KnownBinaries[name])
	rec := LoadManifest()[name]
	state := recordState(rec, tc)
	hint := InstallHint + name
	hasSpec := tc.Spec != "" && tc.Version != ""
	out := Resolution{Name: name, Wanted: tc.Version}
	managed := func(reason string, withHint bool) Resolution {
		out.Path, out.Source = asString(mustGet(rec, "path")), "managed"
		out.Version, out.Reason = asString(mustGet(rec, "version")), reason
		if withHint {
			out.Hint = hint
		}
		return out
	}
	system := func() bool {
		found, err := exec.LookPath(tc.Bin)
		if err != nil {
			return false
		}
		if abs, err := filepath.Abs(found); err == nil {
			found = abs
		}
		out.Path, out.Source = found, "system"
		return true
	}
	switch tc.Source {
	case "managed":
		if state == "valid" {
			return managed("", false)
		}
		out.Reason, out.Hint = "managed-stale", hint
		if rec == nil {
			out.Reason = "managed-not-installed"
		}
		return out
	case "system":
		if !system() {
			out.Reason = "not-on-path"
		}
		return out
	}
	if state == "valid" {
		return managed("", false)
	}
	if state == "stale" && executable(asString(mustGet(rec, "path"))) {
		return managed("managed-stale", true)
	}
	if system() {
		if hasSpec {
			out.Hint = hint
		}
		return out
	}
	out.Reason = "not-installed"
	if hasSpec {
		out.Hint = hint
	}
	return out
}

func mustGet(o map[string]any, k string) any {
	v := o[k]
	return v
}

// MarkAvailability sets external_executors.<name>.available for every
// enabled `dispatch: cli` executor, as Python's resolve_config did: whether
// the tool resolves to a path, or null for an executor agent-exec has no
// built-in binary for. It runs after config.Resolve because tool
// resolution sits above config in the package graph.
func MarkAvailability(cfg *config.Resolved) {
	ext := cfg.Section("external_executors")
	for _, name := range slices.Sorted(maps.Keys(ext)) {
		ecfg := config.ObjectAt(ext, name)
		if ecfg == nil {
			continue
		}
		enabled := ecfg["enabled"]
		dispatch := ecfg["dispatch"]
		if enabled != true || dispatch != "cli" {
			continue
		}
		if _, known := KnownBinaries[name]; !known {
			ecfg["available"] = nil
			continue
		}
		ecfg["available"] = Resolve(name, cfg).Path != ""
	}
}
