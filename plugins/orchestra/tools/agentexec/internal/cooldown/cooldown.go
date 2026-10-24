// Package cooldown keeps the executor-state file: which executors recently
// reported quota, rate-limit, credit or auth trouble and must be skipped by
// route until their cooldown expires. Every failure here is swallowed; a
// broken state file must never block routing.
package cooldown

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
)

const defaultPath = "~/.claude/orchestra/executor-state.json"

// Entry is one active cooldown.
type Entry struct {
	Reason string
	Until  float64
}

func seconds(t time.Time) float64 { return float64(t.UnixMicro()) / 1e6 }

// Enabled reports cooldown.enabled being exactly true.
func Enabled(cfg *config.Resolved) bool {
	b, _ := cfg.Bool("cooldown", "enabled")
	return b
}

// StatePath is the expanded absolute state file path.
func StatePath(cfg *config.Resolved) string {
	p, _ := cfg.String("cooldown", "path")
	if p == "" {
		p = defaultPath
	}
	a, err := filepath.Abs(fsutil.ExpandUser(p))
	if err != nil {
		return p
	}
	return a
}

func loadState(path string) map[string]any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{}
	}
	v, err := fsutil.DecodeJSON(raw)
	if err != nil {
		return map[string]any{}
	}
	if o, ok := v.(map[string]any); ok {
		return o
	}
	return map[string]any{}
}

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

func untilOf(entry any) (float64, bool) {
	o, ok := entry.(map[string]any)
	if !ok {
		return 0, false
	}
	u := o["until"]
	return number(u)
}

// Active returns the well-formed cooldowns still running at now.
func Active(cfg *config.Resolved, now time.Time) map[string]Entry {
	return active(loadState(StatePath(cfg)), seconds(now))
}

func active(state map[string]any, now float64) map[string]Entry {
	out := map[string]Entry{}
	for _, name := range slices.Sorted(maps.Keys(state)) {
		v := state[name]
		until, ok := untilOf(v)
		if !ok || !(until > now) {
			continue
		}
		reason := "unknown"
		if r, has := v.(map[string]any)["reason"]; has {
			if s, isStr := r.(string); isStr {
				reason = s
			}
		}
		out[name] = Entry{Reason: reason, Until: until}
	}
	return out
}

// Seconds is the configured positive cooldown for reason, else 0.
func Seconds(cfg *config.Resolved, reason string) int {
	if !Enabled(cfg) {
		return 0
	}
	n, ok := cfg.Int("cooldown", "seconds", reason)
	if !ok || n <= 0 {
		return 0
	}
	return n
}

func save(path string, state map[string]any, now float64) bool {
	pruned := map[string]any{}
	for _, name := range slices.Sorted(maps.Keys(state)) {
		v := state[name]
		if until, ok := untilOf(v); ok && until > now {
			pruned[name] = v
		}
	}
	data, err := fsutil.MarshalJSON(pruned)
	if err != nil {
		return false
	}
	if err := fsutil.EnsureDir(filepath.Dir(path), 0o755); err != nil {
		return false
	}
	return fsutil.AtomicWriteFile(path, data, 0o600) == nil
}

// RecordUnavailable starts a cooldown for executor unless the run being
// penalized evidently succeeded (exit 0 with a non-empty answer) or the
// reason is runaway. It never fails the caller.
func RecordUnavailable(cfg *config.Resolved, executor, reason string, now time.Time, exitCode *int, answer string) {
	if reason == "runaway" {
		return
	}
	if exitCode != nil && *exitCode == 0 && strings.TrimSpace(answer) != "" {
		return
	}
	secs := Seconds(cfg, reason)
	if secs <= 0 || executor == "" {
		return
	}
	path := StatePath(cfg)
	state := loadState(path)
	nowS := seconds(now)
	until := nowS + float64(secs)
	if v, ok := state[executor]; ok {
		if existing, has := untilOf(v); has && existing > until {
			return
		}
	}
	state[executor] = map[string]any{"reason": reason, "until": until}
	save(path, state, nowS)
}
