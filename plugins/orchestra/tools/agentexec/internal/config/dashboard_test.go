package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A dashboard key in any layer fails resolution naming that layer's file.
func TestResolveRejectsRemovedDashboardKey(t *testing.T) {
	cases := map[string]struct {
		user    string
		project string
		// wantFile is "user" or "project" for the layer the error names, "" for no error.
		wantFile string
	}{
		"user artifact_url": {
			user:     "dashboard:\n  artifact_url: https://claude.ai/artifact/old\n",
			wantFile: "user",
		},
		"user artifact_urls": {
			user:     "dashboard:\n  artifact_urls: {}\n",
			wantFile: "user",
		},
		"project only": {
			project:  "dashboard: {}\n",
			wantFile: "project",
		},
		"no dashboard key": {
			user: "tiers: {light: sonnet}\n",
		},
	}
	for title, tc := range cases {
		t.Run(title, func(t *testing.T) {
			home := t.TempDir()
			root := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("ORCHESTRA_CONFIG_ROOT", root)
			want := ""
			if tc.user != "" {
				p := filepath.Join(home, ".claude", "orchestra.yaml")
				writeConfigFile(t, p, tc.user)
				if tc.wantFile == "user" {
					want = p
				}
			}
			if tc.project != "" {
				p := filepath.Join(root, ".claude", "orchestra.yaml")
				writeConfigFile(t, p, tc.project)
				if tc.wantFile == "project" {
					want = p
				}
			}
			cfg, err := Resolve()
			if want == "" {
				if err != nil {
					t.Fatalf("Resolve() error = %v, want none", err)
				}
				if _, ok := cfg.Values["dashboard"]; ok {
					t.Error("resolved values carry a dashboard key")
				}
				return
			}
			if err == nil {
				t.Fatal("Resolve() succeeded, want the removed dashboard key error")
			}
			msg := err.Error()
			if !strings.HasPrefix(msg, want+": ") ||
				!strings.Contains(msg, "the dashboard key was removed") ||
				!strings.Contains(msg, "~/.claude/orchestra/status.json") {
				t.Errorf("error = %q, want it to name %s and the status file", msg, want)
			}
		})
	}
}

func writeConfigFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
