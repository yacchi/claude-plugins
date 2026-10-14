// Package telemetry is the opt-in, anonymized crash-dump-style telemetry.
// Enforcement is by allowlist in code: Sanitize only ever copies a fixed set
// of keys whose values pass an exact enum/type check into a brand new object.
// No code path copies caller-supplied free text (prompts, task text, paths,
// task ids, code, error messages) into a record.
package telemetry

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
)

const (
	maxLines   = 10000
	defaultDir = "~/.claude/orchestra/telemetry"
)

var (
	events    = []string{"run_summary", "dispatch"}
	lanes     = []string{"express", "orchestrated"}
	executors = []string{"claude", "pi"}
	classes   = []string{"light", "standard", "deep", "review", "independent-review"}
	statuses  = []string{"ok", "unavailable", "needs-permission", "runaway"}
	reasons   = []string{
		"quota", "rate-limit", "credits", "auth", "nonzero-exit", "error",
		"sandbox", "idle", "tool-idle", "repeat", "wall", "runaway",
	}
	usageKeys = []string{
		"input_tokens", "output_tokens", "cached_input_tokens",
		"api_duration_ms", "session_duration_ms",
	}
	countKeys = []string{
		"task_count", "pass", "fail", "exhausted", "fallbacks",
		"sandbox_denials", "sandbox_granted",
	}

	versionRE  = regexp.MustCompile(`^\p{Nd}+\.\p{Nd}+\.\p{Nd}+\n?$`)
	roundKeyRE = regexp.MustCompile(`^[1-9][0-9]*\n?$`)
)

// ErrRejected is Sanitize's error for a record without a valid event.
var ErrRejected = errors.New("telemetry: record rejected")

func oneOf(v any, allowed []string) (string, bool) {
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	for _, a := range allowed {
		if a == s {
			return s, true
		}
	}
	return "", false
}

func nonNegInt(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, x >= 0
	case int:
		return int64(x), x >= 0
	}
	return 0, false
}

type entry struct {
	key string
	val any
}

// entries is the key/value pairs of a JSON object value, sorted by key; ok is false when v is not an object.
func entries(v any) ([]entry, bool) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]entry, 0, len(keys))
		for _, k := range keys {
			out = append(out, entry{k, x[k]})
		}
		return out, true
	}
	return nil, false
}

func contains(list []string, s string) bool {
	for _, a := range list {
		if a == s {
			return true
		}
	}
	return false
}

// countDict keeps the allowed keys holding non-negative integers; ok is
// false when value is not an object.
func countDict(value any, allowed []string) (map[string]any, bool) {
	es, ok := entries(value)
	if !ok {
		return nil, false
	}
	out := map[string]any{}
	for _, e := range es {
		if n, isInt := nonNegInt(e.val); isInt && contains(allowed, e.key) {
			out[e.key] = n
		}
	}
	return out, true
}

// Sanitize turns a caller-supplied record into a NEW object holding only
// allowlisted keys with valid values. An error means the record must be
// rejected outright (missing or invalid event). Nested objects may be
// map[string]any; integers must be int64 or int, as fsutil.DecodeJSON
// produces.
func Sanitize(raw map[string]any) (map[string]any, error) {
	event, ok := oneOf(raw["event"], events)
	if !ok {
		return nil, ErrRejected
	}
	out := map[string]any{}
	out["event"] = event
	out["schema_version"] = int64(2)
	out["ts"] = fsutil.NowISO()
	out["os"] = strings.ToLower(runtime.GOOS)

	if v, ok := raw["orchestra_version"].(string); ok && versionRE.MatchString(v) {
		out["orchestra_version"] = v
	}
	for _, f := range []struct {
		key     string
		allowed []string
	}{{"lane", lanes}, {"executor", executors}, {"cls", classes}, {"status", statuses}, {"reason", reasons}} {
		if s, ok := oneOf(raw[f.key], f.allowed); ok {
			out[f.key] = s
		}
	}
	if b, ok := raw["resumed"].(bool); ok {
		out["resumed"] = b
	}
	for _, key := range countKeys {
		if n, ok := nonNegInt(raw[key]); ok {
			out[key] = n
		}
	}
	if v, present := raw["classes"]; present {
		if d, ok := countDict(v, classes); ok {
			out["classes"] = d
		}
	}
	if v, present := raw["executors_used"]; present {
		if d, ok := countDict(v, executors); ok {
			out["executors_used"] = d
		}
	}
	if es, ok := entries(raw["rounds"]); ok {
		d := map[string]any{}
		for _, e := range es {
			if n, isInt := nonNegInt(e.val); isInt && roundKeyRE.MatchString(e.key) {
				d[e.key] = n
			}
		}
		out["rounds"] = d
	}
	if es, ok := entries(raw["external_enabled"]); ok {
		d := map[string]any{}
		for _, e := range es {
			if b, isBool := e.val.(bool); isBool && e.key == "pi" {
				d[e.key] = b
			}
		}
		out["external_enabled"] = d
	}
	if d, ok := countDict(raw["usage"], usageKeys); ok && len(d) > 0 {
		out["usage"] = d
	}
	return out, nil
}

// Dir is the telemetry directory: telemetry.dir, else the default.
func Dir(cfg *config.Resolved) string {
	if d, ok := cfg.String("telemetry", "dir"); ok && d != "" {
		return fsutil.ExpandUser(d)
	}
	return fsutil.ExpandUser(defaultDir)
}

// RecordsPath is the records file inside Dir.
func RecordsPath(cfg *config.Resolved) string {
	return filepath.Join(Dir(cfg), "records.jsonl")
}

// Enabled is true only when telemetry.enabled is the boolean true.
func Enabled(cfg *config.Resolved) bool {
	b, _ := cfg.Bool("telemetry", "enabled")
	return b
}

// Append sanitizes rec and appends it as one line to the records file,
// subject to a hard line cap. It never fails the caller: a disabled
// telemetry, a rejected record or any I/O error means nothing is written.
func Append(rec map[string]any, cfg *config.Resolved) {
	if !Enabled(cfg) || rec == nil {
		return
	}
	raw := map[string]any{}
	for _, k := range slices.Sorted(maps.Keys(rec)) {
		raw[k] = rec[k]
	}
	clean, err := Sanitize(raw)
	if err != nil {
		return
	}
	dir := Dir(cfg)
	if os.MkdirAll(dir, 0o777) != nil {
		return
	}
	line, err := fsutil.MarshalJSON(clean)
	if err != nil {
		return
	}
	path := filepath.Join(dir, "records.jsonl")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
	if err != nil {
		return
	}
	_, werr := f.Write(append(line, '\n'))
	f.Close()
	if werr == nil {
		enforceCap(path)
	}
}

// enforceCap keeps only the newest maxLines lines of path.
func enforceCap(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.SplitAfter(string(data), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > maxLines {
		_ = os.WriteFile(path, []byte(strings.Join(lines[len(lines)-maxLines:], "")), 0o666)
	}
}

// BuildDispatchRecord builds the (pre-sanitize) record of one dispatch from
// the executor's result: only status/reason, the sandbox counts and numeric
// usage are used, never the answer or any other free text.
func BuildDispatchRecord(executor string, result map[string]any, resumed bool, cls string) map[string]any {
	status := result["status"]
	reason := result["reason"]
	rec := map[string]any{}
	rec["event"] = "dispatch"
	rec["executor"] = executor
	rec["status"] = status
	rec["reason"] = reason
	rec["resumed"] = resumed
	if sb := config.ObjectAt(result, "sandbox"); sb != nil {
		if d, has := sb["denials"]; has {
			dl, _ := d.([]any)
			g := sb["granted"]
			gl, _ := g.([]any)
			rec["sandbox_denials"] = int64(len(dl))
			rec["sandbox_granted"] = int64(len(gl))
		}
	}
	if cls != "" {
		rec["cls"] = cls
	}
	if usage := config.ObjectAt(result, "usage"); usage != nil {
		flat := map[string]any{}
		for _, key := range []string{"api_duration_ms", "session_duration_ms"} {
			if v, ok := usage[key]; ok {
				if n, isInt := nonNegInt(v); isInt {
					flat[key] = n
				}
			}
		}
		if tokens := config.ObjectAt(usage, "tokens"); tokens != nil {
			for _, key := range []string{"input_tokens", "output_tokens", "cached_input_tokens"} {
				if v, ok := tokens[key]; ok {
					if n, isInt := nonNegInt(v); isInt {
						flat[key] = n
					}
				}
			}
		}
		if len(flat) > 0 {
			rec["usage"] = flat
		}
	}
	return rec
}

// ReadRecords returns every parseable JSON value of the records file, one
// per non-blank line (anything malformed is skipped); a missing file is
// empty.
func ReadRecords(path string) []any {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	var out []any
	for _, line := range strings.Split(text, "\n") {
		line = pyStrip(line)
		if line == "" {
			continue
		}
		v, err := fsutil.DecodeJSON([]byte(line))
		if err != nil {
			continue
		}
		out = append(out, v)
	}
	return out
}

// Summary is `telemetry show`'s text: the record total and the counts by
// event, status and reason.
func Summary(records []any) string {
	byEvent, byStatus, byReason := map[string]any{}, map[string]any{}, map[string]any{}
	for _, r := range records {
		obj, ok := r.(map[string]any)
		if !ok {
			continue
		}
		for _, c := range []struct {
			key string
			agg map[string]any
		}{{"event", byEvent}, {"status", byStatus}, {"reason", byReason}} {
			v := obj[c.key]
			if v == nil {
				continue
			}
			key, ok := v.(string)
			if !ok {
				b, _ := fsutil.MarshalJSON(v)
				key = string(b)
			}
			n := c.agg[key]
			count, _ := n.(int64)
			c.agg[key] = count + 1
		}
	}
	dump := func(o map[string]any) string {
		b, _ := fsutil.MarshalJSON(o)
		return string(b)
	}
	return fmt.Sprintf("total records: %d\nby event: %s\nby status: %s\nby reason: %s\n",
		len(records), dump(byEvent), dump(byStatus), dump(byReason))
}
