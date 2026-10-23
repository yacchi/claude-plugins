package isolate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
)

// fakePnpm puts a pnpm on PATH that logs each call (argv and the GVS
// switch) to the returned file, fails the calls whose argv contains one of
// failOn ("" = every call), and otherwise installs a global-virtual-store
// node_modules.
func fakePnpm(t *testing.T, failOn ...string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	fail := ""
	for _, f := range failOn {
		if f == "" {
			fail += "exit 1\n"
			continue
		}
		fail += "case \" $* \" in *\" " + f + " \"*) echo 'ERR_PNPM_NO_OFFLINE_TARBALL' >&2; exit 1;; esac\n"
	}
	script := "#!/bin/sh\n" +
		"echo \"$* gvs=$pnpm_config_enable_global_virtual_store\" >> '" + log + "'\n" +
		fail +
		"mkdir -p node_modules && echo 'virtualStoreDir: ../../store/links' > node_modules/.modules.yaml\n"
	if err := os.WriteFile(filepath.Join(dir, "pnpm"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func setPnpmGVS(t *testing.T, on bool) {
	t.Helper()
	was := config.PnpmGVSOn
	config.PnpmGVSOn = func() bool { return on }
	t.Cleanup(func() { config.PnpmGVSOn = was })
}

// pnpmRepo is newRepo with a committed pnpm-lock.yaml (lock: true) and an
// installed node_modules and .venv in the main tree.
func pnpmRepo(t *testing.T, lock bool) string {
	t.Helper()
	repo := newRepo(t)
	if lock {
		writeFile(t, filepath.Join(repo, "pnpm-lock.yaml"), "lockfileVersion: '9.0'\n")
		git(t, repo, "add", "-A")
		git(t, repo, "commit", "-q", "-m", "lock")
	}
	writeFile(t, filepath.Join(repo, "node_modules", "dep", "index.js"), "x\n")
	writeFile(t, filepath.Join(repo, ".venv", "bin", "python"), "x\n")
	return repo
}

func TestCarryInstallsNodeModulesUnderThePnpmGlobalVirtualStore(t *testing.T) {
	cases := []struct {
		name       string
		on, lock   bool
		failOn     []string
		method     string
		calls      []string
		cloned     bool // node_modules copied from the main tree
		noteSubstr string
	}{
		{name: "offline install", on: true, lock: true, method: carryPnpmGVS,
			calls: []string{"install --frozen-lockfile --offline gvs=true"}},
		{name: "offline fails, online retry", on: true, lock: true, failOn: []string{"--offline"},
			method: carryPnpmGVS,
			calls:  []string{"install --frozen-lockfile --offline gvs=true", "install --frozen-lockfile gvs=true"}},
		{name: "both fail: clone fallback", on: true, lock: true, failOn: []string{""}, cloned: true,
			calls:      []string{"install --frozen-lockfile --offline gvs=true", "install --frozen-lockfile gvs=true"},
			noteSubstr: "pnpm install (gvs) failed"},
		{name: "setting off", on: false, lock: true, cloned: true},
		{name: "no lockfile", on: true, lock: false, cloned: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo := pnpmRepo(t, c.lock)
			log := fakePnpm(t, c.failOn...)
			setPnpmGVS(t, c.on)
			res, err := Create(repo, "t1", CreateOptions{Backend: BackendGit, Carry: true})
			if err != nil || res.Status != "created" {
				t.Fatalf("create: %+v, %v", res, err)
			}
			if c.method != "" && res.CarryMethod != c.method {
				t.Errorf("carry_method = %q, want %q", res.CarryMethod, c.method)
			}
			if got := exists(filepath.Join(res.Path, "node_modules", "dep", "index.js")); got != c.cloned {
				t.Errorf("node_modules cloned = %v, want %v", got, c.cloned)
			}
			if !exists(filepath.Join(res.Path, ".venv", "bin", "python")) {
				t.Error(".venv was not carried")
			}
			var calls []string
			if b, err := os.ReadFile(log); err == nil {
				calls = strings.Split(strings.TrimSpace(string(b)), "\n")
			}
			if strings.Join(calls, "|") != strings.Join(c.calls, "|") {
				t.Errorf("pnpm calls = %q, want %q", calls, c.calls)
			}
			if !strings.Contains(res.CarryNote, c.noteSubstr) {
				t.Errorf("carry_note = %q, want it to contain %q", res.CarryNote, c.noteSubstr)
			}
		})
	}
}
