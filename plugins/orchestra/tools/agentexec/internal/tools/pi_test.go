package tools

import (
	"os"
	"path/filepath"
	"runtime"
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

// linksTo reports whether link is target: a symlink to it, or (where symlinks
// are not allowed, as on Windows) a hard link or copy of it.
func linksTo(link, target string) bool {
	if got, err := os.Readlink(link); err == nil {
		return got == target
	}
	a, errA := os.Stat(link)
	b, errB := os.Stat(target)
	return errA == nil && errB == nil && os.SameFile(a, b)
}

func cfgWithPi(key string, v any) *config.Resolved {
	r := &config.Resolved{Values: config.Defaults()}
	config.ObjectAt(config.ObjectAt(r.Values, "external_executors"), "pi")[key] = v
	return r
}

func TestPiAgentDirModes(t *testing.T) {
	home := piHome(t)
	managed := filepath.Join(home, ".local", "state", "claude-orchestra", "pi-agent")
	abs := filepath.Join(filepath.VolumeName(home)+string(filepath.Separator), "abs", "pi") // /abs/pi
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
		{abs, PiAgentPath, abs},
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
	if err != nil || (runtime.GOOS != "windows" && st.Mode().Perm() != 0o700) {
		t.Fatalf("dir %v %v", st, err)
	}
	if !linksTo(filepath.Join(dir, "auth.json"), userAuth) {
		t.Fatalf("auth.json is not linked to %s", userAuth)
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
	if !linksTo(filepath.Join(dir, "auth.json"), userAuth) {
		t.Fatalf("relinked away from %s", userAuth)
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
	if !linksTo(filepath.Join(dir, "auth.json"), filepath.Join(other, "auth.json")) {
		t.Fatalf("auth.json is not linked to the override dir's")
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
