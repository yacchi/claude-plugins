package config

import (
	"strings"
	"testing"
)

func TestTierResolvesBothForms(t *testing.T) {
	r := &Resolved{Values: map[string]any{"tiers": map[string]any{
		"light": "haiku",
		"deep":  map[string]any{"model": "opus", "effort": "high"},
	}}}
	if m, e := Tier(r, "light"); m != "haiku" || e != "" {
		t.Fatalf("light: %q %q", m, e)
	}
	if m, e := Tier(r, "deep"); m != "opus" || e != "high" {
		t.Fatalf("deep: %q %q", m, e)
	}
	if m, e := Tier(r, "review"); m != "" || e != "" {
		t.Fatalf("missing: %q %q", m, e)
	}
}

func TestValidateTiersRejectsBadObjects(t *testing.T) {
	cases := map[string]map[string]any{
		"unknown key": {"light": map[string]any{"model": "haiku", "temperature": 1}},
		"bad effort":  {"light": map[string]any{"model": "haiku", "effort": "turbo"}},
		"bad type":    {"light": 3},
	}
	for name, tiers := range cases {
		err := validateTiers(map[string]any{"tiers": tiers})
		if err == nil || !strings.Contains(err.Error(), "tiers.light") {
			t.Fatalf("%s: err=%v", name, err)
		}
	}
}

func TestValidateTiersRejectsBadPlanner(t *testing.T) {
	cases := map[string]any{
		"unlisted model":  map[string]any{"model": "haiku", "effort": "max"},
		"unlisted string": "gpt-6.1-sol",
		"no model":        map[string]any{"effort": "max"},
		"bad effort":      map[string]any{"model": "opus", "effort": "ultra"},
	}
	for name, planner := range cases {
		err := validateTiers(map[string]any{"tiers": map[string]any{"planner": planner}})
		if err == nil || !strings.Contains(err.Error(), "tiers.planner") {
			t.Fatalf("%s: err=%v", name, err)
		}
	}
}

func TestDefaultsCarryPlanner(t *testing.T) {
	if m, e := Tier(&Resolved{Values: Defaults()}, "planner"); m != "opus" || e != "xhigh" {
		t.Fatalf("planner: %q %q", m, e)
	}
}

func TestValidateTiersAcceptsDefaults(t *testing.T) {
	if err := validateTiers(Defaults()); err != nil {
		t.Fatalf("defaults rejected: %v", err)
	}
}
