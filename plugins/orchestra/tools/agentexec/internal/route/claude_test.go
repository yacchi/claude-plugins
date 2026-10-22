package route

import (
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
)

func TestClaudeWinnerCarriesTierModelEffortAndAgentType(t *testing.T) {
	cases := map[string][3]any{
		"light":    {"haiku", "medium", "orchestra:orchestra-light"},
		"standard": {"haiku", "high", "orchestra:orchestra-light"},
		"deep":     {"opus", "medium", "orchestra:orchestra-deep"},
		"review":   {"sonnet", "medium", "orchestra:orchestra-review"},
	}
	for cls, want := range cases {
		cfg := usageConfig(cls, "claude")
		r := Resolve(cfg, piReady, cls, "default", nil, nil, nil)
		if r.Executor != "claude" || r.Dispatch != "claude" {
			t.Fatalf("%s: executor=%q dispatch=%v", cls, r.Executor, r.Dispatch)
		}
		if r.Model != want[0] || r.Effort != want[1] || r.AgentType != want[2] {
			t.Fatalf("%s: model=%v effort=%v agent_type=%v", cls, r.Model, r.Effort, r.AgentType)
		}
	}
}

func TestClaudeIndependentReviewHasNoAgentType(t *testing.T) {
	cfg := usageConfig("independent-review", "claude")
	r := Resolve(cfg, piReady, "independent-review", "default", nil, nil, nil)
	if r.Executor != "claude" || r.AgentType != nil {
		t.Fatalf("executor=%q agent_type=%v", r.Executor, r.AgentType)
	}
}

func TestClaudeStringTierHasNullEffort(t *testing.T) {
	cfg := usageConfig("light", "claude")
	config.ObjectAt(cfg.Values, "tiers")["light"] = "haiku"
	r := Resolve(cfg, piReady, "light", "default", nil, nil, nil)
	obj := r.Object()
	if r.Model != "haiku" || r.Effort != nil || obj["effort"] != nil {
		t.Fatalf("model=%v effort=%v", r.Model, r.Effort)
	}
	if obj["agent_type"] != "orchestra:orchestra-light" {
		t.Fatalf("agent_type=%v", obj["agent_type"])
	}
}
