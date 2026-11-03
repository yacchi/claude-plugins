package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

const waveWaitHopRefusal = "agent-exec: wave wait: --max-wait must be at most 170 seconds (Claude Code moves a foreground Bash call to the background after 180 s); use 150, the default"

// A missing state file means no runner record, so `wave wait` answers at
// once and nothing sleeps.
func TestWaveWaitHopMaxWait(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state.json")
	cases := []struct {
		maxWait string
		refused bool
	}{
		{"171", true},
		{"170.5", true},
		{"170", false},
		{"0", false},
	}
	for _, c := range cases {
		t.Run(c.maxWait, func(t *testing.T) {
			_, stderr, code := planDirRun(t, "wave", "wait", "--state", state, "--max-wait", c.maxWait)
			if !c.refused {
				if code == 2 || strings.Contains(stderr, "must be at most 170") {
					t.Fatalf("--max-wait %s: exit %d, stderr %q; want it accepted", c.maxWait, code, stderr)
				}
				return
			}
			if code != 2 {
				t.Fatalf("--max-wait %s: exit %d, want 2", c.maxWait, code)
			}
			if !strings.Contains(stderr, waveWaitHopRefusal) {
				t.Fatalf("--max-wait %s: stderr %q, want it to contain %q", c.maxWait, stderr, waveWaitHopRefusal)
			}
		})
	}
}
