//go:build darwin

package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Real enforcement under the generated SBPL: guards rule precedence and
// signal confinement, which comparing the profile text cannot.

func needSeatbelt(t *testing.T) {
	t.Helper()
	if p := probeSeatbelt(); !p.OK {
		t.Skipf("needs a working sandbox-exec: %s", p.Detail)
	}
}

func canWrite(t *testing.T, p *Policy, cwd, path string) bool {
	t.Helper()
	argv := WrapArgv([]string{"/bin/sh", "-c", `echo x >> "$1"`, "sh", path}, cwd, Spec{Backend: "seatbelt", Policy: p})
	return exec.Command(argv[0], argv[1:]...).Run() == nil
}

func TestSeatbeltEnforcesWriteReadAndSignalRules(t *testing.T) {
	needSeatbelt(t)
	base := tempReal(t)
	tree, outside, secret := filepath.Join(base, "tree"), filepath.Join(base, "out"), filepath.Join(base, "secret")
	for _, d := range []string{tree, outside, secret} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(secret, "k"), []byte("k"), 0o644); err != nil {
		t.Fatal(err)
	}
	sleeper := exec.Command("sleep", "60")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	defer sleeper.Process.Kill()
	script := "echo x > " + tree + "/f && echo w-in;" +
		"echo x > " + outside + "/f 2>/dev/null || echo w-out-denied;" +
		"cat " + secret + "/k 2>/dev/null || echo r-denied;" +
		"kill " + strconv.Itoa(sleeper.Process.Pid) + " 2>/dev/null || echo kill-denied;" +
		"sleep 30 & kill $! && echo kill-own"
	p := &Policy{Writable: []string{tree, "/dev"}, DenyRead: []string{secret}}
	argv := WrapArgv([]string{"/bin/sh", "-c", script}, tree, Spec{Backend: "seatbelt", Policy: p})
	out, _ := exec.Command(argv[0], argv[1:]...).Output()
	if got := strings.Fields(string(out)); strings.Join(got, " ") != "w-in w-out-denied r-denied kill-denied kill-own" {
		t.Errorf("enforcement: %v", got)
	}
}

func TestSeatbeltBlocksTamperAndPiConfigWrites(t *testing.T) {
	needSeatbelt(t)
	h := newHome(t)
	for _, d := range []string{h.h(".pi/agent"), h.h(".claude/orchestra")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(h.h(".pi/agent/settings.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	none := []Grant{}
	p := buildPolicy(h.tree, "pi", Config{AllowWrite: []string{"~/.claude"}},
		policyOpts{home: h.home, env: map[string]string{}, platform: "darwin", learned: &none})
	if !canWrite(t, p, h.tree, h.h(".pi/agent/settings.json.lock")) {
		t.Error("pi lock file not writable")
	}
	if canWrite(t, p, h.tree, h.h(".pi/agent/settings.json")) {
		t.Error("pi settings writable")
	}
	argv := WrapArgv([]string{"/bin/mkdir", "-p", h.h(".pi/agent/extensions")}, h.tree, Spec{Backend: "seatbelt", Policy: p})
	if exec.Command(argv[0], argv[1:]...).Run() == nil {
		t.Error("pi extensions dir creatable")
	}
	if canWrite(t, p, h.tree, LearnedPath()) {
		t.Error("grant store writable")
	}
	if !canWrite(t, p, h.tree, h.h(".claude/orchestra/other.json")) {
		t.Error("allow_write ~/.claude not writable")
	}
	if canWrite(t, p, h.tree, h.h(".local/share/uv/tools/x")) {
		t.Error("uv tools writable")
	}
}

func TestSeatbeltHostWritesOnlyTheInlinePluginDir(t *testing.T) {
	needSeatbelt(t)
	base := tempReal(t)
	home := filepath.Join(base, "home")
	plugins := filepath.Join(home, ".claude", "plugins")
	if err := os.MkdirAll(filepath.Join(plugins, "data", "other-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")
	repo, _ := repoWithWorktree(t, base)
	none := []Grant{}
	p := hostPolicy(HostInput{Repo: repo, WorktreeBase: repo + "-orchestra", HerdrHome: filepath.Join(home, "hx")},
		Config{}, policyOpts{home: home, env: map[string]string{"TMPDIR": "/nonexistent-tmp"}, platform: "darwin", learned: &none})
	checks := map[string]bool{
		filepath.Join(plugins, "data", "orchestra-inline", "x"): true,
		filepath.Join(plugins, "data", "other-plugin", "x"):     false,
		filepath.Join(plugins, "installed_plugins.json"):        false,
		filepath.Join(home, ".claude", "history-x.jsonl"):       true,
		filepath.Join(home, ".claude.json.lock"):                true,
		filepath.Join(home, ".claude", "settings.json"):         false,
	}
	for path, want := range checks {
		if got := canWrite(t, p, base, path); got != want {
			t.Errorf("write %s: %v, want %v", path, got, want)
		}
	}
}

func TestSeatbeltWritesManagedPiSessionsButNotState(t *testing.T) {
	needSeatbelt(t)
	h := newHome(t)
	state := h.h(".local/state/claude-orchestra")
	agent := filepath.Join(state, "pi-agent")
	for _, d := range []string{filepath.Join(agent, "sessions"), filepath.Join(state, "tools")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p := buildPolicy(h.tree, "pi", Config{}, policyOpts{home: h.home,
		env: map[string]string{"PI_CODING_AGENT_DIR": agent}, platform: "darwin"})
	if !canWrite(t, p, h.tree, filepath.Join(agent, "sessions", "s.jsonl")) {
		t.Error("sessions not writable")
	}
	for _, path := range []string{filepath.Join(state, "sandbox-learned.json"), filepath.Join(state, "tools", "x"),
		filepath.Join(agent, "settings.json")} {
		if canWrite(t, p, h.tree, path) {
			t.Errorf("%s writable", path)
		}
	}
}

// A Keychain lookup needs the Security framework's MDS cache writable: Claude
// Code in a herdr pane otherwise starts "Not logged in".
func TestSeatbeltKeychainLookupNeedsTheMDSCache(t *testing.T) {
	needSeatbelt(t)
	cache := policyOpts{}.darwinUserCache()
	if cache == "" {
		t.Skip("no DARWIN_USER_CACHE_DIR")
	}
	base := tempReal(t)
	lookup := func(writable ...string) string {
		p := &Policy{Writable: append([]string{base, "/dev"}, writable...)}
		argv := WrapArgv([]string{"/usr/bin/security", "find-generic-password", "-s", "orchestra-sandbox-probe-absent"},
			base, Spec{Backend: "seatbelt", Policy: p})
		out, _ := exec.Command(argv[0], argv[1:]...).CombinedOutput()
		return string(out)
	}
	const mdsError = "Module Directory Service"
	if !strings.Contains(lookup(), mdsError) {
		t.Skip("the Keychain lookup works without the MDS cache on this machine")
	}
	if out := lookup(realPath(filepath.Join(cache, "mds"))); strings.Contains(out, mdsError) {
		t.Errorf("lookup with the MDS cache writable: %s", out)
	}
}
