package tools

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
)

func piHome(t *testing.T) string {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv(PiAgentDirEnv, "")
	return home
}

func cfgWithPi(key string, v any) *config.Resolved {
	r := &config.Resolved{Values: config.Defaults()}
	r.Values.Object("external_executors").Object("pi").Set(key, v)
	return r
}

func TestPiAgentDirModes(t *testing.T) {
	home := piHome(t)
	managed := filepath.Join(home, ".local", "state", "claude-orchestra", "pi-agent")
	for _, c := range []struct {
		v          any
		mode, want string
	}{
		{"managed", PiAgentManaged, managed},
		{"", PiAgentManaged, managed},
		{nil, PiAgentManaged, managed},
		{true, PiAgentManaged, managed},
		{"user", PiAgentUser, ""},
		{"~/pi-dir", PiAgentPath, filepath.Join(home, "pi-dir")},
		{"/abs/pi", PiAgentPath, "/abs/pi"},
	} {
		mode, dir := PiAgentDir(cfgWithPi("agent_dir", c.v))
		if mode != c.mode || dir != c.want {
			t.Errorf("%v: (%s, %s), want (%s, %s)", c.v, mode, dir, c.mode, c.want)
		}
	}
	if mode, _ := PiAgentDir(nil); mode != PiAgentManaged {
		t.Errorf("nil cfg mode %s", mode)
	}
}

func TestPiContextFiles(t *testing.T) {
	if !PiContextFiles(nil) || !PiContextFiles(cfgWithPi("context_files", "no")) {
		t.Fatal("context files off without an explicit false")
	}
	if PiContextFiles(cfgWithPi("context_files", false)) {
		t.Fatal("explicit false ignored")
	}
}

func TestEnsurePiAgentDirLinksExistingLogin(t *testing.T) {
	home := piHome(t)
	userAuth := filepath.Join(home, ".pi", "agent", "auth.json")
	if err := os.MkdirAll(filepath.Dir(userAuth), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userAuth, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir, err := EnsurePiAgentDir(nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(dir)
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("dir %v %v", st, err)
	}
	if target, err := os.Readlink(filepath.Join(dir, "auth.json")); err != nil || target != userAuth {
		t.Fatalf("auth link %q %v", target, err)
	}
	// An existing auth.json is never replaced.
	other := filepath.Join(home, "other")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(PiAgentDirEnv, other)
	if _, err := EnsurePiAgentDir(nil); err != nil {
		t.Fatal(err)
	}
	if target, _ := os.Readlink(filepath.Join(dir, "auth.json")); target != userAuth {
		t.Fatalf("relinked to %q", target)
	}
}

func TestEnsurePiAgentDirPrefersOverrideDir(t *testing.T) {
	home := piHome(t)
	other := filepath.Join(home, "other")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(PiAgentDirEnv, other)
	dir, err := EnsurePiAgentDir(nil)
	if err != nil {
		t.Fatal(err)
	}
	if target, _ := os.Readlink(filepath.Join(dir, "auth.json")); target != filepath.Join(other, "auth.json") {
		t.Fatalf("link %q", target)
	}
}

func TestPiEnvOverrides(t *testing.T) {
	home := piHome(t)
	if got := PiEnvOverrides(cfgWithPi("agent_dir", "user")); len(got) != 0 {
		t.Fatalf("user mode %v", got)
	}
	got := PiEnvOverrides(cfgWithPi("agent_dir", "~/explicit"))
	if got[PiAgentDirEnv] != filepath.Join(home, "explicit") {
		t.Fatalf("path mode %v", got)
	}
	if _, err := os.Stat(filepath.Join(home, "explicit")); !os.IsNotExist(err) {
		t.Fatal("explicit dir was created")
	}
	got = PiEnvOverrides(nil)
	if got[PiAgentDirEnv] != filepath.Join(home, ".local", "state", "claude-orchestra", "pi-agent") {
		t.Fatalf("managed %v", got)
	}
}
