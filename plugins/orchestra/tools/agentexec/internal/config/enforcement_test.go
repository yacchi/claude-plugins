package config

import "testing"

func TestResumeMaxTokensDefaultAndLayer(t *testing.T) {
	if got := ResumeMaxTokens(&Resolved{Values: Defaults()}); got != 150000 {
		t.Errorf("default = %d, want 150000", got)
	}
	over := map[string]any{"enforcement": map[string]any{"resume_max_tokens": int64(50000)}}
	cfg := &Resolved{Values: DeepMerge(Defaults(), over)}
	if got := ResumeMaxTokens(cfg); got != 50000 {
		t.Errorf("layered = %d, want 50000", got)
	}
}

func TestResumeMaxTokensOffReadsAsZero(t *testing.T) {
	for _, v := range []any{"off", false, int64(0), int64(-5), "abc"} {
		cfg := &Resolved{Values: map[string]any{"enforcement": map[string]any{"resume_max_tokens": v}}}
		if got := ResumeMaxTokens(cfg); got != 0 {
			t.Errorf("resume_max_tokens=%v: got %d, want 0", v, got)
		}
	}
}

func TestResumeMaxTokensForByModel(t *testing.T) {
	cfg := &Resolved{Values: Defaults()}
	for model, want := range map[string]int64{
		"claude-haiku-5-5":          90000,
		"claude-sonnet-5-5":         150000,
		"openai-codex/gpt-5.6-luna": 150000,
		"":                          150000,
	} {
		if got := ResumeMaxTokensFor(cfg, model); got != want {
			t.Errorf("model %q = %d, want %d", model, got, want)
		}
	}
}

func TestResumeMaxTokensForLongestMatchCaseInsensitive(t *testing.T) {
	byModel := map[string]any{"haiku": int64(90000), "haiku-5": int64(40000)}
	cfg := &Resolved{Values: DeepMerge(Defaults(), map[string]any{
		"enforcement": map[string]any{"resume_max_tokens_by_model": byModel},
	})}
	if got := ResumeMaxTokensFor(cfg, "Claude-HAIKU-5-5"); got != 40000 {
		t.Errorf("longest match = %d, want 40000", got)
	}
}

func TestResumeMaxTokensForOffDisables(t *testing.T) {
	byModel := map[string]any{"haiku": "off"}
	cfg := &Resolved{Values: DeepMerge(Defaults(), map[string]any{
		"enforcement": map[string]any{"resume_max_tokens_by_model": byModel},
	})}
	if got := ResumeMaxTokensFor(cfg, "claude-haiku-5-5"); got != 0 {
		t.Errorf("haiku off = %d, want 0", got)
	}
}

func TestResumeMaxTokensForGlobalOffWins(t *testing.T) {
	cfg := &Resolved{Values: DeepMerge(Defaults(), map[string]any{
		"enforcement": map[string]any{"resume_max_tokens": "off"},
	})}
	if got := ResumeMaxTokensFor(cfg, "claude-haiku-5-5"); got != 0 {
		t.Errorf("global off, haiku = %d, want 0", got)
	}
}
