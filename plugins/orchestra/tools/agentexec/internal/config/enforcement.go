package config

import (
	"maps"
	"slices"
	"strings"
)

// ResumeMaxTokens is the resume limit for a worker whose model is unknown:
// ResumeMaxTokensFor with an empty model.
func ResumeMaxTokens(cfg *Resolved) int64 {
	return ResumeMaxTokensFor(cfg, "")
}

// resumeMaxTokensCap is enforcement.resume_max_tokens: the common context
// ceiling. 0 means no limit: the key is "off", false, 0, negative, or not a
// number.
func resumeMaxTokensCap(cfg *Resolved) int64 {
	n, ok := cfg.Int("enforcement", "resume_max_tokens")
	if !ok || n < 0 {
		return 0
	}
	return int64(n)
}

// resumeSafetyPercent is the share of the cap a session may reach before it
// is retired, so the limit trips before the context hits the cap itself.
const resumeSafetyPercent = 90

// ResumeMaxTokensFor is the resume limit for a worker running model: 90% of
// the cap, floored. A worker session whose last context reached it is not
// resumed; its correction goes to a fresh session (cache reads count against
// the plan limits, and some models price context past a size cliff). The cap
// is the value of the enforcement.resume_max_tokens_by_model entry whose key
// is the longest case-insensitive substring of model (not bounded by the
// common cap), else enforcement.resume_max_tokens. A matching "off" or 0
// gives 0 (no limit) for that model; a global off gives 0 for every model.
func ResumeMaxTokensFor(cfg *Resolved, model string) int64 {
	base := resumeMaxTokensCap(cfg)
	if base == 0 {
		return 0
	}
	limit := base
	if model != "" {
		v, _ := cfg.Lookup("enforcement", "resume_max_tokens_by_model")
		byModel, _ := v.(map[string]any)
		m := strings.ToLower(model)
		best := ""
		for _, key := range slices.Sorted(maps.Keys(byModel)) {
			k := strings.ToLower(key)
			if k == "" || !strings.Contains(m, k) || len(k) <= len(best) {
				continue
			}
			if n, ok := resumeLimitValue(byModel[key]); ok {
				best, limit = k, n
			}
		}
	}
	return limit * resumeSafetyPercent / 100
}

// resumeLimitValue reads one enforcement.resume_max_tokens_by_model value: a
// non-negative integer, or "off", which is 0 (no limit for that model). Any
// other value is not a limit and reports false.
func resumeLimitValue(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, x >= 0
	case int:
		return int64(x), x >= 0
	case string:
		return 0, strings.ToLower(strings.TrimSpace(x)) == "off"
	}
	return 0, false
}
