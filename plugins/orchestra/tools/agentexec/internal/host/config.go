package host

import (
	"encoding/json"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
)

// Config is a host's config section merged over its defaults: `models` (and `warm`) merge key by key,
// a null value keeps the default, `enabled` is normalized.
type Config struct {
	values  map[string]any
	Enabled Tri
}

// MergeConfig merges section `name` of cfg (nil: the defaults) over
// config.Defaults()[name].
func MergeConfig(cfg *config.Resolved, name string) Config {
	defaults := config.ObjectAt(config.Defaults(), name)
	out := map[string]any{}
	if defaults != nil {
		out = maps.Clone(defaults)
	}
	var section map[string]any
	if cfg != nil {
		section = cfg.Section(name)
	}
	if section != nil {
		for _, k := range slices.Sorted(maps.Keys(section)) {
			v := section[k]
			sub, isObj := v.(map[string]any)
			base := config.ObjectAt(out, k)
			switch {
			case (k == "models" || k == "warm") && isObj:
				if base == nil {
					base = map[string]any{}
				}
				merged := maps.Clone(base)
				for _, sk := range slices.Sorted(maps.Keys(sub)) {
					sv := sub[sk]
					merged[sk] = config.CloneValue(sv)
				}
				out[k] = merged
			case v != nil:
				out[k] = config.CloneValue(v)
			}
		}
	}
	if config.ObjectAt(out, "models") == nil {
		out["models"] = map[string]any{}
	}
	en := out["enabled"]
	return Config{values: out, Enabled: NormalizeEnabled(en)}
}

// Get is the raw merged value of key.
func (c Config) Get(key string) any {
	if c.values == nil {
		return nil
	}
	v := c.values[key]
	return v
}

// Str is the text of a scalar key ("" for null or a container).
func (c Config) Str(key string) string { return scalarText(c.Get(key)) }

// Truthy reports whether key holds a set value (see Truthy).
func (c Config) Truthy(key string) bool { return Truthy(c.Get(key)) }

// Seconds is the numeric value of key as a duration (def when not numeric).
func (c Config) Seconds(key string, def float64) time.Duration {
	return secondsDuration(ConfigFloat(c.Get(key), def))
}

// Float is the numeric value of key (def when not numeric).
func (c Config) Float(key string, def float64) float64 { return ConfigFloat(c.Get(key), def) }

// Model is models[cls], "" when unset.
func (c Config) Model(cls string) string {
	m := config.ObjectAt(c.values, "models")
	if m == nil {
		return ""
	}
	v := m[cls]
	if !Truthy(v) {
		return ""
	}
	return scalarText(v)
}

// Sub is the merged sub-object key (nil when absent).
func (c Config) Sub(key string) map[string]any {
	if c.values == nil {
		return nil
	}
	return config.ObjectAt(c.values, key)
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
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "true", "yes", "on", "required":
			return TriTrue
		case "false", "no", "off", "never":
			return TriFalse
		}
	}
	return TriAuto
}

// Truthy reports whether a decoded JSON value is set: non-null, non-zero,
// non-empty.
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
	}
	return true
}

// ConfigFloat is the number in a config scalar (a number or a numeric
// string); def for anything else, including NaN and infinities.
func ConfigFloat(v any, def float64) float64 {
	switch x := v.(type) {
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case float64:
		if isFinite(x) {
			return x
		}
	case json.Number:
		if f, err := x.Float64(); err == nil && isFinite(f) {
			return f
		}
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil && isFinite(f) {
			return f
		}
	}
	return def
}

func isFinite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// scalarText is the text of a string or number; "" for null and containers.
func scalarText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
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

// Settings JSON of a hosted worker, compact: the herdr start line is capped
// (maxStartLine). disableRemoteControl stops the worker registering a Remote
// Control session, which no CLI flag can.
const (
	remoteControlOffSettings = `{"disableRemoteControl":true}`
	isolateSettings          = `{"autoMemoryEnabled":false,"disableClaudeAiConnectors":true,"disableRemoteControl":true}`
)

// IsolationFlags are the Claude flags a hosted worker is started with. Every
// mode adds --settings with disableRemoteControl. `isolate` also adds
// --strict-mcp-config (no user MCP servers), --setting-sources project,local
// (no user hooks or enabled plugins), --plugin-dir <orchestra plugin root>
// and --no-chrome, and its settings also turn off the claude.ai connectors
// and auto memory; each flag unless tokens (the user's own args) already has
// it. `inherit` adds only the settings. A --settings in tokens is an error
// unless it sets disableRemoteControl itself: orchestra adds no second one.
func IsolationFlags(c Config, tokens []string) ([]string, error) {
	mode, err := UserConfigMode(c)
	if err != nil {
		return nil, err
	}
	has := func(flag string) bool {
		for _, t := range tokens {
			if t == flag || strings.HasPrefix(t, flag+"=") {
				return true
			}
		}
		return false
	}
	userVals := userSettings(tokens)
	for _, v := range userVals {
		if !disablesRemoteControl(v) {
			return nil, Need(KindError, `the worker's --settings must set "disableRemoteControl": true `+
				`(add it to that JSON; orchestra does not add a second --settings)`)
		}
	}
	flags := []string{}
	if mode == "inherit" {
		if len(userVals) == 0 {
			flags = append(flags, "--settings", remoteControlOffSettings)
		}
		return flags, nil
	}
	if !has("--strict-mcp-config") {
		flags = append(flags, "--strict-mcp-config")
	}
	if !has("--setting-sources") {
		flags = append(flags, "--setting-sources", "project,local")
	}
	if root := PluginRoot(); !has("--plugin-dir") && root != "" {
		flags = append(flags, "--plugin-dir", root)
	}
	if len(userVals) == 0 {
		flags = append(flags, "--settings", isolateSettings)
	}
	if !has("--no-chrome") {
		flags = append(flags, "--no-chrome")
	}
	return flags, nil
}

// IsolationEnv is the environment `isolate` adds to a hosted worker process
// (the bundled skills and the IDE auto-connect off); none for `inherit`.
func IsolationEnv(c Config) ([]string, error) {
	mode, err := UserConfigMode(c)
	if err != nil {
		return nil, err
	}
	if mode == "inherit" {
		return []string{}, nil
	}
	return []string{"CLAUDE_CODE_DISABLE_BUNDLED_SKILLS=1", "CLAUDE_CODE_AUTO_CONNECT_IDE=false"}, nil
}

// userSettings are the values of every --settings (`--settings v` or
// `--settings=v`) in tokens.
func userSettings(tokens []string) []string {
	var values []string
	for i, t := range tokens {
		if t == "--settings" {
			v := ""
			if i+1 < len(tokens) {
				v = tokens[i+1]
			}
			values = append(values, v)
		} else if v, ok := strings.CutPrefix(t, "--settings="); ok {
			values = append(values, v)
		}
	}
	return values
}

// disablesRemoteControl reports whether a --settings value is JSON that sets
// disableRemoteControl to true (a file path is not, so it is refused).
func disablesRemoteControl(v string) bool {
	var settings map[string]any
	if err := json.Unmarshal([]byte(v), &settings); err != nil {
		return false
	}
	return settings["disableRemoteControl"] == true
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
