package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const cfgDashOrg = "0b1c2d3e-4f50-4617-8899-aabbccddeeff"

// cfgDashWriteProfile writes a .claude.json with oauthAccount org into dir.
func cfgDashWriteProfile(t *testing.T, dir, org, name string) {
	t.Helper()
	body := `{"oauthAccount":{"organizationUuid":"` + org + `","organizationName":"` + name + `"}}`
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Any dashboard.artifact_url, empty or not, is refused with the migration text.
func TestDashboardArtifactURLKeyIsRefused(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	for _, v := range []string{"", "https://claude.ai/artifact/abc-123"} {
		values := resolveLayers(obj("dashboard", obj("artifact_url", v)))
		err := validateDashboard(values)
		if err == nil {
			t.Fatalf("artifact_url %q accepted, want an error", v)
		}
		if !strings.HasPrefix(err.Error(), "dashboard.artifact_url was removed: ") ||
			!strings.HasSuffix(err.Error(), "(this session's organization: unknown), or delete the key to publish a new dashboard") {
			t.Errorf("error = %q", err)
		}
	}
	if err := validateDashboard(Defaults()); err != nil {
		t.Errorf("defaults: %v", err)
	}
}

func TestDashboardArtifactURLErrorNamesCurrentOrg(t *testing.T) {
	dir := t.TempDir()
	cfgDashWriteProfile(t, dir, cfgDashOrg, "Acme")
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	err := validateDashboard(resolveLayers(obj("dashboard", obj("artifact_url", ""))))
	if err == nil || !strings.Contains(err.Error(), "(this session's organization: "+cfgDashOrg+" Acme)") {
		t.Errorf("error = %v, want the current organization named", err)
	}
}

func TestDashboardArtifactURLsValidation(t *testing.T) {
	good := obj(cfgDashOrg, "https://claude.ai/artifact/abc-123", "0b1c2d3e-4f50-4617-8899-000000000001", "")
	for _, values := range []map[string]any{
		resolveLayers(),
		resolveLayers(obj("dashboard", obj("artifact_urls", good))),
	} {
		if err := validateDashboard(values); err != nil {
			t.Errorf("valid artifact_urls rejected: %v", err)
		}
	}
	bad := map[string]any{
		"non-uuid key":   obj("not-a-uuid", "https://claude.ai/artifact/abc"),
		"non-claude url": obj(cfgDashOrg, "https://evil.example/artifact/abc"),
		"chat url":       obj(cfgDashOrg, "https://claude.ai/chat/abc"),
		"non-string url": obj(cfgDashOrg, int64(1)),
	}
	for name, urls := range bad {
		values := resolveLayers(obj("dashboard", obj("artifact_urls", urls)))
		if err := validateDashboard(values); err == nil {
			t.Errorf("%s accepted, want an error", name)
		}
	}
	values := Defaults()
	ObjectAt(values, "dashboard")["artifact_urls"] = "https://claude.ai/artifact/abc"
	if err := validateDashboard(values); err == nil {
		t.Error("non-map artifact_urls accepted, want an error")
	}
}

func TestCurrentOrgReadsProfileFile(t *testing.T) {
	cases := map[string]struct {
		useCfgDir bool
		setup     func(t *testing.T, cfgDir, home string)
		want      string
	}{
		"CLAUDE_CONFIG_DIR wins over home": {
			useCfgDir: true,
			setup: func(t *testing.T, cfgDir, home string) {
				cfgDashWriteProfile(t, cfgDir, cfgDashOrg, "Acme")
				cfgDashWriteProfile(t, home, "ffffffff-0000-4000-8000-000000000000", "Other")
			},
			want: cfgDashOrg + " Acme",
		},
		"CLAUDE_CONFIG_DIR without file does not fall back to home": {
			useCfgDir: true,
			setup: func(t *testing.T, cfgDir, home string) {
				cfgDashWriteProfile(t, home, cfgDashOrg, "Acme")
			},
			want: "",
		},
		"home .claude.json": {
			setup: func(t *testing.T, cfgDir, home string) {
				cfgDashWriteProfile(t, home, cfgDashOrg, "Acme")
			},
			want: cfgDashOrg + " Acme",
		},
		"home .claude/.claude.json fallback": {
			setup: func(t *testing.T, cfgDir, home string) {
				cfgDashWriteProfile(t, filepath.Join(home, ".claude"), cfgDashOrg, "Acme")
			},
			want: cfgDashOrg + " Acme",
		},
		"missing oauthAccount": {
			setup: func(t *testing.T, cfgDir, home string) {
				if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"numStartups":1}`), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "",
		},
	}
	for title, tc := range cases {
		t.Run(title, func(t *testing.T) {
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			home := t.TempDir()
			cfgDirEnv := ""
			if tc.useCfgDir {
				cfgDirEnv = cfgDir
			}
			t.Setenv("CLAUDE_CONFIG_DIR", cfgDirEnv)
			t.Setenv("HOME", home)
			tc.setup(t, cfgDir, home)
			uuid, name := CurrentOrg()
			if got := strings.TrimSpace(uuid + " " + name); got != tc.want {
				t.Errorf("CurrentOrg() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDashboardURLPicksOrganization(t *testing.T) {
	const url = "https://claude.ai/artifact/abc-123"
	values := resolveLayers(obj("dashboard", obj("artifact_urls", obj(cfgDashOrg, url))))
	if got := DashboardURL(values, cfgDashOrg); got != url {
		t.Errorf("DashboardURL(own org) = %q, want %q", got, url)
	}
	if got := DashboardURL(values, "0b1c2d3e-4f50-4617-8899-000000000001"); got != "" {
		t.Errorf("DashboardURL(other org) = %q, want \"\"", got)
	}
	if got := DashboardURL(values, ""); got != "" {
		t.Errorf("DashboardURL(\"\") = %q, want \"\"", got)
	}
}
