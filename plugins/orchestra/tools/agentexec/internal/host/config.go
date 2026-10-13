package host

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
)

// Config is a host's config section merged over its defaults the way the
// Python hosts' merged_config did: `models` (and `warm`) merge key by key,
// a null value keeps the default, `enabled` is normalized.
type Config struct {
	values  *jsonx.Object
	Enabled Tri
}

// MergeConfig merges section `name` of cfg (nil: the defaults) over
// config.Defaults()[name].
func MergeConfig(cfg *config.Resolved, name string) Config {
	defaults := config.Defaults().Object(name)
	out := jsonx.NewObject()
	if defaults != nil {
		out = defaults.Clone()
	}
	var section *jsonx.Object
	if cfg != nil {
		section = cfg.Section(name)
	}
	if section != nil {
		for _, k := range section.Keys() {
			v, _ := section.Get(k)
			sub, isObj := v.(*jsonx.Object)
			base := out.Object(k)
			switch {
			case (k == "models" || k == "warm") && isObj:
				if base == nil {
					base = jsonx.NewObject()
				}
				merged := base.Clone()
				for _, sk := range sub.Keys() {
					sv, _ := sub.Get(sk)
					merged.Set(sk, jsonx.CloneValue(sv))
				}
				out.Set(k, merged)
			case v != nil:
				out.Set(k, jsonx.CloneValue(v))
			}
		}
	}
	if out.Object("models") == nil {
		out.Set("models", jsonx.NewObject())
	}
	en, _ := out.Get("enabled")
	return Config{values: out, Enabled: NormalizeEnabled(en)}
}

// Get is the raw merged value of key.
func (c Config) Get(key string) any {
	if c.values == nil {
		return nil
	}
	v, _ := c.values.Get(key)
	return v
}

// Str is str(value) of a scalar key ("" for null or a container).
func (c Config) Str(key string) string { return pyStr(c.Get(key)) }

// Truthy is Python's truthiness of key.
func (c Config) Truthy(key string) bool { return Truthy(c.Get(key)) }

// Seconds is float(value) of key as a duration (def when not numeric).
func (c Config) Seconds(key string, def float64) time.Duration {
	return secondsDuration(PyFloat(c.Get(key), def))
}

// Float is float(value) of key (def when not numeric).
func (c Config) Float(key string, def float64) float64 { return PyFloat(c.Get(key), def) }

// Model is models[cls], "" when unset.
func (c Config) Model(cls string) string {
	m := c.values.Object("models")
	if m == nil {
		return ""
	}
	v, _ := m.Get(cls)
	if !Truthy(v) {
		return ""
	}
	return pyStr(v)
}

// Sub is the merged sub-object key (nil when absent).
func (c Config) Sub(key string) *jsonx.Object {
	if c.values == nil {
		return nil
	}
	return c.values.Object(key)
}

// Strings is the string elements of the list under key.
func (c Config) Strings(key string) []string { return stringList(c.Get(key)) }

// Settings is the runner's view of the config.
func (c Config) Settings() Settings {
	return Settings{
		TaskTimeout:    c.Seconds("task_timeout", 3600),
		StartupTimeout: c.Seconds("startup_timeout", 90),
		KeepSessions:   c.Truthy("keep_sessions"),
		AddDirs:        c.Strings("add_dirs"),
	}
}

func secondsDuration(s float64) time.Duration {
	if math.IsNaN(s) || s < 0 {
		return 0
	}
	if s > float64(math.MaxInt64/int64(time.Second)) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(s * float64(time.Second))
}

// NormalizeEnabled maps `enabled` to a Tri: YAML booleans, and the words
// true/yes/on/required and false/no/off/never; anything else is auto.
func NormalizeEnabled(v any) Tri {
	switch x := v.(type) {
	case bool:
		if x {
			return TriTrue
		}
		return TriFalse
	case string:
		switch config.PyLower(strings.TrimSpace(x)) {
		case "true", "yes", "on", "required":
			return TriTrue
		case "false", "no", "off", "never":
			return TriFalse
		}
	}
	return TriAuto
}

// Truthy is Python's bool(v) for JSON values.
func Truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	case json.Number:
		f, err := x.Float64()
		return err != nil || f != 0
	case []any:
		return len(x) > 0
	case []string:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	case *jsonx.Object:
		return x != nil && x.Len() > 0
	}
	return true
}

// PyFloat is float(v) for config scalars (numbers and numeric strings);
// def otherwise.
func PyFloat(v any, def float64) float64 {
	switch x := v.(type) {
	case bool:
		if x {
			return 1
		}
		return 0
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case float64:
		return x
	case json.Number:
		if f, err := x.Float64(); err == nil {
			return f
		}
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil {
			return f
		}
	}
	return def
}

func pyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "True"
		}
		return "False"
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return jsonx.FormatFloat(x)
	case json.Number:
		return string(x)
	}
	return ""
}

func formatSeconds(s float64) string {
	if s == math.Trunc(s) && math.Abs(s) < 1e15 {
		return strconv.FormatInt(int64(s), 10)
	}
	return strconv.FormatFloat(s, 'f', -1, 64)
}

// --- hosted Claude ------------------------------------------------------------

// UserConfigMode is the host's `user_config`: isolate (default) or inherit.
func UserConfigMode(c Config) (string, error) {
	mode := c.Str("user_config")
	if mode == "" {
		mode = "isolate"
	}
	if mode != "isolate" && mode != "inherit" {
		return "", Need(KindError, "invalid user_config: "+mode+" (isolate | inherit)")
	}
	return mode, nil
}

// IsolationFlags are the Claude flags that keep a hosted worker off the
// user's own setup. `isolate` adds --strict-mcp-config (no user MCP
// servers), --setting-sources project,local (no user hooks or enabled
// plugins) and --plugin-dir <orchestra plugin root>, each unless tokens (the
// user's own args) already has it; `inherit` adds nothing.
func IsolationFlags(c Config, tokens []string) ([]string, error) {
	mode, err := UserConfigMode(c)
	if err != nil {
		return nil, err
	}
	if mode == "inherit" {
		return []string{}, nil
	}
	has := func(flag string) bool {
		for _, t := range tokens {
			if t == flag || strings.HasPrefix(t, flag+"=") {
				return true
			}
		}
		return false
	}
	flags := []string{}
	if !has("--strict-mcp-config") {
		flags = append(flags, "--strict-mcp-config")
	}
	if !has("--setting-sources") {
		flags = append(flags, "--setting-sources", "project,local")
	}
	if root := PluginRoot(); !has("--plugin-dir") && root != "" {
		flags = append(flags, "--plugin-dir", root)
	}
	return flags, nil
}

// PluginRoot is the orchestra plugin directory (the one holding
// .claude-plugin/plugin.json): the bootstrap's grandparent
// ($AGENT_EXEC_BOOTSTRAP is <root>/tools/agent-exec), else the nearest
// ancestor of this executable (a dev build under tools/agentexec). "" when
// neither is a plugin root.
func PluginRoot() string {
	isRoot := func(dir string) bool {
		st, err := os.Stat(filepath.Join(dir, ".claude-plugin", "plugin.json"))
		return err == nil && st.Mode().IsRegular()
	}
	if boot := os.Getenv("AGENT_EXEC_BOOTSTRAP"); boot != "" {
		if abs, err := filepath.Abs(boot); err == nil {
			if root := filepath.Dir(filepath.Dir(abs)); isRoot(root) {
				return root
			}
		}
	}
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		dir := filepath.Dir(exe)
		for i := 0; i < 6; i++ {
			if isRoot(dir) {
				return dir
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return ""
}
