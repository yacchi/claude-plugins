package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// A *_calls key under enforcement.fanout fails resolution, naming the file and
// the *_requests key that replaces it.
func TestResolveRejectsRemovedFanoutCallKeys(t *testing.T) {
	cases := map[string]struct {
		user    string
		wantKey string
		wantNew string
	}{
		"scout":   {user: "enforcement:\n  fanout:\n    scout_calls: 10\n", wantKey: "scout_calls", wantNew: "scout_requests"},
		"checker": {user: "enforcement:\n  fanout:\n    checker_calls: 4\n", wantKey: "checker_calls", wantNew: "checker_requests"},
		"writer":  {user: "enforcement:\n  fanout:\n    writer_calls: 20\n", wantKey: "writer_calls", wantNew: "writer_requests"},
		"new key": {user: "enforcement:\n  fanout:\n    scout_requests: 10\n"},
		"off":     {user: "enforcement:\n  fanout: \"off\"\n"},
	}
	for title, tc := range cases {
		t.Run(title, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("ORCHESTRA_CONFIG_ROOT", t.TempDir())
			p := filepath.Join(home, ".claude", "orchestra.yaml")
			writeConfigFile(t, p, tc.user)
			_, err := Resolve()
			if tc.wantKey == "" {
				if err != nil {
					t.Fatalf("Resolve() error = %v, want none", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Resolve() succeeded, want the removed fanout key error")
			}
			msg := err.Error()
			if !strings.Contains(msg, p) || !strings.Contains(msg, "enforcement.fanout."+tc.wantKey+" was removed") || !strings.Contains(msg, "enforcement.fanout."+tc.wantNew) {
				t.Errorf("error = %q, want the file, the removed key and %s", msg, tc.wantNew)
			}
		})
	}
}
