package paths

import (
	"path/filepath"
	"testing"
)

// The default runtime locations all sit under StateDir, which follows
// XDG_STATE_HOME only when it is absolute. hooks/lib/paths.test.ts pins the
// same resolution table for the TS side.
func TestRuntimeDefaultsLiveUnderStateDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ORCHESTRA_ALIVE_DIR", "")

	xdg := filepath.Join(home, "xdg")
	t.Setenv("XDG_STATE_HOME", xdg)
	state := RealPath(filepath.Join(xdg, "claude-orchestra"))
	if got := StateDir(); got != state {
		t.Fatalf("StateDir() = %q, want %q", got, state)
	}
	want := map[string]string{
		"LedgerDir":    filepath.Join(state, "runs"),
		"TelemetryDir": filepath.Join(state, "telemetry"),
		"CooldownPath": filepath.Join(state, "executor-state.json"),
		"AliveDir":     filepath.Join(state, "alive"),
	}
	got := map[string]string{
		"LedgerDir":    LedgerDir(),
		"TelemetryDir": TelemetryDir(),
		"CooldownPath": CooldownPath(),
		"AliveDir":     AliveDir(),
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s() = %q, want %q", name, got[name], w)
		}
	}

	t.Setenv("XDG_STATE_HOME", "relative/state")
	if got, want := StateDir(), RealPath(filepath.Join(home, ".local", "state", "claude-orchestra")); got != want {
		t.Errorf("relative XDG_STATE_HOME: StateDir() = %q, want %q", got, want)
	}

	t.Setenv("ORCHESTRA_ALIVE_DIR", "/override")
	if got := AliveDir(); got != "/override" {
		t.Errorf("ORCHESTRA_ALIVE_DIR override: AliveDir() = %q", got)
	}
}
