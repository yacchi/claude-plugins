package route

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/cooldown"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/quota"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/readiness"
)

// usageConfig is the defaults with cls routed over the given candidates (the
// default priority belongs to another change and is not relied on).
func usageConfig(cls string, cands ...string) *config.Resolved {
	values := config.Defaults()
	list := make([]any, len(cands))
	for i, c := range cands {
		list[i] = c
	}
	config.ObjectAt(values, "priority")[cls] = map[string]any{"default": list}
	return &config.Resolved{Values: values}
}

var piReady = map[string]readiness.ExecReady{"pi": {OK: true}}

func fiveHour(pct float64) quota.Entry {
	return quota.Entry{FiveHour: &quota.Window{UsedPercent: pct}, Source: quota.SourceFixture}
}

func TestUsageDemotionIsSoftWhenEveryCandidateIsOver(t *testing.T) {
	cfg := usageConfig("light", "claude", "pi")
	usage := map[string]quota.Entry{"claude": fiveHour(95), "openai-codex": fiveHour(99)}
	r := Resolve(cfg, piReady, "light", "default", nil, nil, usage)
	if r.Executor != "claude" || !r.UsageBypassed || r.CooldownBypassed {
		t.Fatalf("executor=%q usage_bypassed=%v cooldown_bypassed=%v", r.Executor, r.UsageBypassed, r.CooldownBypassed)
	}
	if len(r.UsageApplied) != 2 || r.UsageApplied[0].Executor != "claude" || r.UsageApplied[1].Executor != "pi" {
		t.Fatalf("usage_applied lists what would have been demoted: %+v", r.UsageApplied)
	}
	if len(r.Skipped) != 0 {
		t.Fatalf("the bypass walk skips nothing: %+v", r.Skipped)
	}
}

func TestUsageDemotesClaudeToPiOnFiveHourPercent(t *testing.T) {
	cfg := usageConfig("standard", "claude", "pi")
	usage := map[string]quota.Entry{"claude": fiveHour(83), "openai-codex": fiveHour(10)}
	r := Resolve(cfg, piReady, "standard", "default", nil, nil, usage)
	if r.Executor != "pi" || r.UsageBypassed {
		t.Fatalf("executor=%q bypassed=%v", r.Executor, r.UsageBypassed)
	}
	if len(r.Skipped) != 1 || r.Skipped[0].Reason != "usage:claude:five_hour=83%" {
		t.Fatalf("skipped %+v", r.Skipped)
	}
	got := r.Object()["usage_applied"].([]any)
	if len(got) != 1 {
		t.Fatalf("usage_applied %v", got)
	}
	u := got[0].(map[string]any)
	if u["executor"] != "claude" || u["group"] != "claude" || u["window"] != "five_hour" ||
		u["used_percent"] != 83.0 || u["threshold"] != 80.0 {
		t.Fatalf("usage_applied entry %v", u)
	}
}

func TestUsageSevenDayJustBelowThresholdIsNotDemoted(t *testing.T) {
	cfg := usageConfig("standard", "claude", "pi")
	usage := map[string]quota.Entry{"claude": {SevenDay: &quota.Window{UsedPercent: 89.9}}}
	r := Resolve(cfg, piReady, "standard", "default", nil, nil, usage)
	if r.Executor != "claude" || len(r.UsageApplied) != 0 {
		t.Fatalf("89.9%% < 90%% must not demote: executor=%q applied=%+v", r.Executor, r.UsageApplied)
	}
	usage["claude"] = quota.Entry{SevenDay: &quota.Window{UsedPercent: 90}}
	r = Resolve(cfg, piReady, "standard", "default", nil, nil, usage)
	if r.Executor != "pi" || r.Skipped[0].Reason != "usage:claude:seven_day=90%" {
		t.Fatalf("at the threshold demotes: executor=%q skipped=%+v", r.Executor, r.Skipped)
	}
}

func TestUsageFailedOrMissingGroupNeverDemotes(t *testing.T) {
	cfg := usageConfig("standard", "claude", "pi")
	failed := fiveHour(99)
	failed.Error = quota.ErrNetwork
	for name, usage := range map[string]map[string]quota.Entry{
		"error":   {"claude": failed},
		"missing": {"openai-codex": fiveHour(99)},
		"nil":     nil,
	} {
		r := Resolve(cfg, piReady, "standard", "default", nil, nil, usage)
		if r.Executor != "claude" || len(r.UsageApplied) != 0 {
			t.Errorf("%s: executor=%q applied=%+v", name, r.Executor, r.UsageApplied)
		}
	}
}

func TestUsageDoesNotDemotePiOnAnotherProvider(t *testing.T) {
	cfg := usageConfig("standard", "pi", "claude")
	policy := config.ObjectAt(config.ObjectAt(config.ObjectAt(cfg.Values["external_executors"].(map[string]any),
		"pi"), "class_policy"), "standard")
	policy["model"] = "github-copilot/gpt-5"
	usage := map[string]quota.Entry{"openai-codex": fiveHour(99)}
	r := Resolve(cfg, piReady, "standard", "default", nil, nil, usage)
	if r.Executor != "pi" || len(r.UsageApplied) != 0 {
		t.Fatalf("executor=%q applied=%+v", r.Executor, r.UsageApplied)
	}
	if got := UsageGroups(cfg, "standard", "default"); strings.Join(got, ",") != "github-copilot,claude" {
		t.Fatalf("usage groups %v", got)
	}
}

func TestUsageAndCooldownBypassTogether(t *testing.T) {
	cfg := usageConfig("standard", "claude", "pi")
	cd := map[string]cooldown.Entry{"pi": {Reason: "rate-limit", Until: 4102444800}}
	usage := map[string]quota.Entry{"claude": fiveHour(85)}
	r := Resolve(cfg, piReady, "standard", "default", nil, cd, usage)
	if r.Executor != "claude" || !r.UsageBypassed || !r.CooldownBypassed {
		t.Fatalf("executor=%q usage_bypassed=%v cooldown_bypassed=%v", r.Executor, r.UsageBypassed, r.CooldownBypassed)
	}
	text := FormatText(r)
	for _, want := range []string{
		"usage: claude (claude five_hour=85% >= 80%) resets -\n",
		"usage: BYPASSED",
		"cooldown: BYPASSED",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
}

func TestUsageAppliedIsEmptyListNotNull(t *testing.T) {
	r := Resolve(usageConfig("standard", "claude"), nil, "standard", "default", nil, nil, nil)
	b, err := json.Marshal(r.Object())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"usage_applied":[]`) || !strings.Contains(string(b), `"usage_bypassed":false`) {
		t.Fatalf("route json %s", b)
	}
}
