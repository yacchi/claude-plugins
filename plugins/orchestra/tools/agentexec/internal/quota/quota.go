// Package quota reads how close the subscriptions behind the route
// candidates are to their rate limits (the 5-hour and 7-day windows of the
// Claude and ChatGPT plans), so route can demote a candidate whose plan is
// nearly spent. The endpoints are undocumented: every failure is fail-open
// (no data, never a demotion) and is reported as a fixed label, never with a
// credential in it. Credentials are only ever read, never refreshed or
// written.
package quota

import (
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
)

// Quota groups with a usage source. Any other group has no data and is
// never demoted.
const (
	GroupClaude = "claude"
	GroupCodex  = "openai-codex"
)

// Groups are the groups with a usage source, in report order.
var Groups = []string{GroupClaude, GroupCodex}

// Windows, in evaluation order.
const (
	FiveHour = "five_hour"
	SevenDay = "seven_day"
)

// WindowOrder is the order windows are checked and printed in.
var WindowOrder = []string{FiveHour, SevenDay}

// Environment switches.
const (
	// SnapshotEnv names a JSON file shaped like a snapshot that replaces
	// every live lookup (no network, keychain or cache).
	SnapshotEnv = "ORCHESTRA_USAGE_SNAPSHOT"
	// RoutingEnv set to "0" disables usage routing outright.
	RoutingEnv = "ORCHESTRA_USAGE_ROUTING"
)

// Sources of an Entry.
const (
	SourceAPI     = "api"
	SourceCache   = "cache"
	SourceFixture = "fixture"
)

// Error labels. An Entry's Error is always one of these.
const (
	ErrNoToken = "no-token"
	ErrExpired = "expired"
	ErrNetwork = "network"
	ErrParse   = "parse"
)

// ErrHTTP is the label of a non-200 answer.
func ErrHTTP(code int) string { return "http-" + strconv.Itoa(code) }

// Window is one rate-limit window: the percent used (0-100) and when it
// resets (epoch seconds, nil when unknown).
type Window struct {
	UsedPercent float64
	ResetsAt    *float64
}

// Entry is one group's usage: its windows (nil when unknown), where it came
// from, when it was fetched (epoch seconds) and the failure label, if any.
type Entry struct {
	FiveHour  *Window
	SevenDay  *Window
	Source    string
	FetchedAt float64
	Error     string
}

// Window returns the named window (nil when unknown).
func (e Entry) Window(name string) *Window {
	switch name {
	case FiveHour:
		return e.FiveHour
	case SevenDay:
		return e.SevenDay
	}
	return nil
}

func windowObject(w *Window) any {
	if w == nil {
		return nil
	}
	var resets any
	if w.ResetsAt != nil {
		resets = *w.ResetsAt
	}
	return map[string]any{"used_percent": w.UsedPercent, "resets_at": resets}
}

// Object renders the entry as JSON values (the cache, fixture and doctor
// shape).
func (e Entry) Object() map[string]any {
	var errLabel any
	if e.Error != "" {
		errLabel = e.Error
	}
	return map[string]any{
		FiveHour:     windowObject(e.FiveHour),
		SevenDay:     windowObject(e.SevenDay),
		"source":     e.Source,
		"fetched_at": e.FetchedAt,
		"error":      errLabel,
	}
}

// Number reads a JSON number (int64 or float64 as fsutil.DecodeJSON and the
// config loaders produce them).
func Number(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case int:
		return float64(x), true
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return 0, false
		}
		return x, true
	}
	return 0, false
}

func windowFrom(v any) *Window {
	o, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	pct, ok := Number(o["used_percent"])
	if !ok {
		return nil
	}
	w := &Window{UsedPercent: pct}
	if r, ok := Number(o["resets_at"]); ok {
		w.ResetsAt = &r
	}
	return w
}

// EntryFrom reads an entry rendered by Object; ok is false when v is not an
// object.
func EntryFrom(v any) (Entry, bool) {
	o, ok := v.(map[string]any)
	if !ok {
		return Entry{}, false
	}
	e := Entry{FiveHour: windowFrom(o[FiveHour]), SevenDay: windowFrom(o[SevenDay])}
	e.Source, _ = o["source"].(string)
	e.FetchedAt, _ = Number(o["fetched_at"])
	if s, isStr := o["error"].(string); isStr {
		e.Error = s
	}
	return e, true
}

// Enabled reports usage routing being on: never when ORCHESTRA_USAGE_ROUTING
// is "0", else usage_routing.enabled being exactly true.
func Enabled(cfg *config.Resolved) bool {
	if os.Getenv(RoutingEnv) == "0" {
		return false
	}
	b, _ := cfg.Bool("usage_routing", "enabled")
	return b
}

var defaultThresholds = map[string]float64{FiveHour: 80, SevenDay: 90}

// Thresholds is the percent per window at or above which a candidate is
// demoted; a missing or non-positive value falls back to the default.
func Thresholds(cfg *config.Resolved) map[string]float64 {
	out := map[string]float64{}
	for _, w := range WindowOrder {
		out[w] = defaultThresholds[w]
		v, _ := cfg.Lookup("usage_routing", "thresholds", w)
		if n, ok := Number(v); ok && n > 0 {
			out[w] = n
		}
	}
	return out
}

const defaultCacheSeconds = 120

// CacheSeconds is usage_routing.cache_seconds (non-negative integer), else
// the default. 0 means no cache.
func CacheSeconds(cfg *config.Resolved) int {
	n, ok := cfg.Int("usage_routing", "cache_seconds")
	if !ok || n < 0 {
		return defaultCacheSeconds
	}
	return n
}

// Group is the quota group the executor draws on for class cls: "claude"
// for claude, else the provider prefix of the class model
// ("openai-codex/gpt-5.6-luna" -> "openai-codex"); "" when unknown.
func Group(cfg *config.Resolved, executor, cls string) string {
	if executor == "claude" {
		return GroupClaude
	}
	ecfg := config.ObjectAt(cfg.Section("external_executors"), executor)
	policy := config.ObjectAt(config.ObjectAt(ecfg, "class_policy"), cls)
	model, _ := policy["model"].(string)
	if provider, _, found := strings.Cut(model, "/"); found {
		return provider
	}
	return ""
}

// FormatPercent renders a percent rounded to one decimal: 83 -> "83",
// 89.94 -> "89.9".
func FormatPercent(p float64) string {
	return strconv.FormatFloat(math.Round(p*10)/10, 'f', -1, 64)
}
