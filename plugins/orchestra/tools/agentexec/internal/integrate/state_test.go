package integrate

import (
	"os"
	"testing"
)

// TestMain points XDG_STATE_HOME at a fresh temp dir for the whole package:
// work trees go under the state dir (isolate.Home), and no test may create
// them, or any other state, in the developer's real
// ~/.local/state/claude-orchestra. A test that needs another state dir still
// sets its own with t.Setenv.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "agentexec-state-")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("XDG_STATE_HOME", dir); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
