package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// cfgDashSetup isolates config resolution: a temporary HOME whose user layer
// holds userYAML, a temporary CLAUDE_CONFIG_DIR whose .claude.json holds
// claudeJSON (skipped when empty), and an empty project root.
func cfgDashSetup(t *testing.T, claudeJSON, userYAML string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("ORCHESTRA_CONFIG_ROOT", t.TempDir())
	cfgDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
	if claudeJSON != "" {
		if err := os.WriteFile(filepath.Join(cfgDir, ".claude.json"), []byte(claudeJSON), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "orchestra.yaml"), []byte(userYAML), 0o600); err != nil {
		t.Fatal(err)
	}
}

// cfgDashDashboard runs `agent-exec config --json` and returns its dashboard
// object.
func cfgDashDashboard(t *testing.T) map[string]any {
	t.Helper()
	out, code := hookCmdRun(t, "", "config", "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var doc struct {
		Dashboard map[string]any `json:"dashboard"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	return doc.Dashboard
}

const cfgDashUserYAML = `dashboard:
  artifact_urls:
    "0d1e0000-0000-4000-8000-00000000000a": "https://claude.ai/artifact/abc"
`

func TestConfigDashboardResolvesSignedInOrgURL(t *testing.T) {
	cfgDashSetup(t, `{"oauthAccount": {"organizationUuid": "0d1e0000-0000-4000-8000-00000000000a", "organizationName": "Org A"}}`, cfgDashUserYAML)
	dash := cfgDashDashboard(t)
	if got := dash["artifact_url"]; got != "https://claude.ai/artifact/abc" {
		t.Errorf("artifact_url = %v; want the organization's URL", got)
	}
	if got := dash["org"]; got != "0d1e0000-0000-4000-8000-00000000000a" {
		t.Errorf("org = %v", got)
	}
	if got := dash["org_name"]; got != "Org A" {
		t.Errorf("org_name = %v", got)
	}
}

func TestConfigDashboardOtherOrgGetsNoURL(t *testing.T) {
	cfgDashSetup(t, `{"oauthAccount": {"organizationUuid": "9f9f0000-0000-4000-8000-00000000000b", "organizationName": "Org B"}}`, cfgDashUserYAML)
	dash := cfgDashDashboard(t)
	if got := dash["artifact_url"]; got != "" {
		t.Errorf("artifact_url = %v; want empty for another organization", got)
	}
}

func TestConfigDashboardNoProfileGivesEmptyOrg(t *testing.T) {
	cfgDashSetup(t, "", cfgDashUserYAML)
	dash := cfgDashDashboard(t)
	if dash["org"] != "" || dash["artifact_url"] != "" {
		t.Errorf("org = %v, artifact_url = %v; want both empty without a profile", dash["org"], dash["artifact_url"])
	}
}

// The removed global dashboard.artifact_url does not stop config: it is
// ignored, never resolved as the organization's URL, and reported in warnings.
func TestConfigDashboardLegacyArtifactURLWarns(t *testing.T) {
	cfgDashSetup(t, `{"oauthAccount": {"organizationUuid": "0d1e0000-0000-4000-8000-00000000000a", "organizationName": "Org A"}}`,
		"dashboard:\n  artifact_url: https://claude.ai/artifact/old\n")
	out, code := hookCmdRun(t, "", "config", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var doc struct {
		Dashboard map[string]any   `json:"dashboard"`
		Warnings  []map[string]any `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if got := doc.Dashboard["artifact_url"]; got != "" {
		t.Errorf("artifact_url = %v; want empty, the legacy key is ignored", got)
	}
	if len(doc.Warnings) != 1 || doc.Warnings[0]["type"] != "dashboard_artifact_url" {
		t.Errorf("warnings = %v; want one dashboard_artifact_url warning", doc.Warnings)
	}
}
