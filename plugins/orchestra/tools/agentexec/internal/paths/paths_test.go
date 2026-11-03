package paths

import (
	"os"
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

// planPathsSeam makes TempRoot compare against roots for the rest of the test.
func planPathsSeam(t *testing.T, roots ...string) {
	t.Helper()
	TempRootsSeam = func() []string { return roots }
	t.Cleanup(func() { TempRootsSeam = nil })
}

func TestPlanPathsLayout(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/nonexistent-xdg")
	if got, want := PlansDir(), StateDir()+"/plans"; got != want {
		t.Errorf("PlansDir() = %q, want %q", got, want)
	}
	if got, want := PlanDir("abc"), StateDir()+"/plans/abc"; got != want {
		t.Errorf("PlanDir(abc) = %q, want %q", got, want)
	}
}

func TestPlanPathsRefusesTempPaths(t *testing.T) {
	planPathsSeam(t, "/tmp", "/private/tmp", os.TempDir())
	for _, p := range []string{
		"/tmp/x/plan.json",
		"/private/tmp/x",
		"/tmp",
		filepath.Join(t.TempDir(), "plan.json"),
	} {
		if TempRoot(p) == "" {
			t.Errorf("TempRoot(%q) = \"\", want a temporary root", p)
		}
		if err := RefuseTemp("--plan", p); err == nil {
			t.Errorf("RefuseTemp(%q) = nil, want an error", p)
		}
	}
}

func TestPlanPathsSymlinkedAlias(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(filepath.Join(root, "sub"), link); err != nil {
		t.Fatal(err)
	}
	planPathsSeam(t, root)
	if got := TempRoot(filepath.Join(link, "plan.json")); got != root {
		t.Errorf("TempRoot(alias) = %q, want %q", got, root)
	}
}

func TestPlanPathsNotRefusedOutsideRoots(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/nonexistent-xdg")
	t.Setenv("HOME", "/nonexistent-home")
	planPathsSeam(t, "/tmp", "/private/tmp")
	for _, p := range []string{
		"/tmpfoo/x",
		"/private/tmpfoo",
		StateDir() + "/plans/p/plan.json",
		filepath.Join(Home(), "work", "plan.json"),
	} {
		if got := TempRoot(p); got != "" {
			t.Errorf("TempRoot(%q) = %q, want \"\"", p, got)
		}
		if err := RefuseTemp("--plan", p); err != nil {
			t.Errorf("RefuseTemp(%q) = %v, want nil", p, err)
		}
	}
}

func TestPlanPathsRelativeIsMadeAbsolute(t *testing.T) {
	root := t.TempDir()
	planPathsSeam(t, root)
	t.Chdir(root)
	if got := TempRoot("plan.json"); got != root {
		t.Errorf("TempRoot(relative in root) = %q, want %q", got, root)
	}

	t.Chdir(t.TempDir())
	if got := TempRoot("plan.json"); got != "" {
		t.Errorf("TempRoot(relative outside root) = %q, want \"\"", got)
	}
}

func TestPlanPathsRefuseText(t *testing.T) {
	planPathsSeam(t, "/tmp")
	got := RefuseTemp("--plan", "/tmp/a/plan.json")
	if got == nil {
		t.Fatal("RefuseTemp returned nil, want an error")
	}
	want := "--plan /tmp/a/plan.json is under /tmp, which a reboot wipes; keep plans, specs, answers and wave state in the plan directory " + PlansDir() + "/<plan id>/ (agent-exec plan dir --id <plan id> prints it)"
	if got.Error() != want {
		t.Errorf("RefuseTemp text =\n%q\nwant\n%q", got.Error(), want)
	}
}

func TestPlanPathsNoSeamInTestBinary(t *testing.T) {
	if TempRootsSeam != nil {
		t.Fatal("TempRootsSeam is set before the test")
	}
	if got := TempRoot("/tmp/x"); got != "" {
		t.Errorf("TempRoot(/tmp/x) without a seam = %q, want \"\"", got)
	}
	if err := RefuseTemp("--plan", "/tmp/x"); err != nil {
		t.Errorf("RefuseTemp(/tmp/x) without a seam = %v, want nil", err)
	}
}
