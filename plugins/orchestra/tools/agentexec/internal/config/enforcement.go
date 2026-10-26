package config

import (
	"maps"
	"slices"
	"strings"
)

// ResumeMaxTokens is enforcement.resume_max_tokens: a worker session whose
// last context reached this many tokens is not resumed; its correction goes
// to a fresh session with the full prompt instead (cache reads count against
// the plan limits). 0 means no limit: the key is "off", false, 0, negative,
// or not a number.
func ResumeMaxTokens(cfg *Resolved) int64 {
	n, ok := cfg.Int("enforcement", "resume_max_tokens")
	if !ok || n < 0 {
		return 0
	}
	return int64(n)
}

// ResumeMaxTokensFor is the resume limit for a worker running model: the
// enforcement.resume_max_tokens_by_model entry whose key is the longest
// case-insensitive substring of model, else ResumeMaxTokens. Some models
// price context past a size cliff, so their limit sits below it. A global
// "off" disables the limit for every model, and an empty model takes the
// common limit.
func ResumeMaxTokensFor(cfg *Resolved, model string) int64 {
	base := ResumeMaxTokens(cfg)
	if base == 0 || model == "" {
		return base
	}
	v, _ := cfg.Lookup("enforcement", "resume_max_tokens_by_model")
	byModel, _ := v.(map[string]any)
	m := strings.ToLower(model)
	best, limit := "", base
	for _, key := range slices.Sorted(maps.Keys(byModel)) {
		k := strings.ToLower(key)
		if k == "" || !strings.Contains(m, k) || len(k) <= len(best) {
			continue
		}
		if n, ok := resumeLimitValue(byModel[key]); ok {
			best, limit = k, n
		}
	}
	return limit
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
