package config

import "testing"

// dashboard.artifact_url is empty or a claude.ai Artifact URL; anything else
// is a config error.
func TestDashboardArtifactURLAcceptsOnlyEmptyOrClaudeArtifact(t *testing.T) {
	for _, good := range []string{"", "https://claude.ai/artifact/abc-123", "https://claude.ai/public/artifacts/abc-123"} {
		values := resolveLayers(obj("dashboard", obj("artifact_url", good)))
		if got := ObjectAt(values, "dashboard")["artifact_url"]; got != good {
			t.Errorf("resolved artifact_url = %#v, want %q", got, good)
		}
		if err := validateDashboard(values); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
	for _, bad := range []any{
		"http://claude.ai/artifact/abc", "https://evil.example/artifact/abc", "https://claude.ai.evil.example/artifact/x",
		"https://claude.ai/chat/abc", "https://user@claude.ai/artifact/abc", "claude.ai/artifact/abc", nil, int64(1),
	} {
		values := Defaults()
		ObjectAt(values, "dashboard")["artifact_url"] = bad
		if err := validateDashboard(values); err == nil {
			t.Errorf("%#v accepted, want an error", bad)
		}
	}
}

func TestDefaultsCarryEmptyDashboardURL(t *testing.T) {
	values := resolveLayers()
	if got, ok := ObjectAt(values, "dashboard")["artifact_url"]; !ok || got != "" {
		t.Errorf("default artifact_url = %#v (present %v), want \"\"", got, ok)
	}
	if err := validateDashboard(values); err != nil {
		t.Error(err)
	}
}
