package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The env must reach linked worktrees only, and never one whose installed
// layout it would convert.
func TestPnpmGVSTree(t *testing.T) {
	const gvs = "virtualStoreDir: ../../../../store/v11/links\n"
	const classic = "virtualStoreDir: .pnpm\n"
	cases := []struct {
		name    string
		on      bool
		gitDir  bool   // .git is a directory: a main checkout
		lock    bool   // pnpm-lock.yaml at the root
		modules string // node_modules/.modules.yaml ("" = no node_modules)
		want    bool
	}{
		{name: "fresh linked tree", on: true, lock: true, want: true},
		{name: "gvs layout", on: true, lock: true, modules: gvs, want: true},
		{name: "absolute store dir", on: true, lock: true, modules: "virtualStoreDir: /store/v11/links\n", want: true},
		{name: "classic layout", on: true, lock: true, modules: classic},
		{name: "unreadable layout", on: true, lock: true, modules: "{{"},
		{name: "main checkout", on: true, gitDir: true, lock: true},
		{name: "no lockfile", on: true},
		{name: "setting off", lock: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tree := t.TempDir()
			if c.gitDir {
				writeTestFile(t, filepath.Join(tree, ".git", "HEAD"), "ref: refs/heads/main\n")
			} else {
				writeTestFile(t, filepath.Join(tree, ".git"), "gitdir: /repo/.git/worktrees/t\n")
			}
			if c.lock {
				writeTestFile(t, filepath.Join(tree, "pnpm-lock.yaml"), "lockfileVersion: '9.0'\n")
			}
			if c.modules != "" {
				writeTestFile(t, filepath.Join(tree, "node_modules", ".modules.yaml"), c.modules)
			}
			if got := PnpmGVSTree(c.on, tree); got != c.want {
				t.Errorf("PnpmGVSTree = %v, want %v", got, c.want)
			}
		})
	}
}

// pnpm drops the global virtual store under CI: the env is still set, and
// the warning comes once per process, not per spawn.
func TestWithPnpmGVSWarnsOnceUnderCI(t *testing.T) {
	var buf bytes.Buffer
	pnpmCIStderr, pnpmCIWarn = &buf, sync.Once{}
	t.Cleanup(func() { pnpmCIStderr, pnpmCIWarn = os.Stderr, sync.Once{} })

	if env := WithPnpmGVS([]string{"CI=false"}); env[len(env)-1] != PnpmGVSEnv || buf.Len() != 0 {
		t.Fatalf("CI=false: env %q, warned %q", env, buf.String())
	}
	for range 2 {
		env := WithPnpmGVS([]string{"CI=true"})
		if env[len(env)-1] != PnpmGVSEnv || env[0] != "CI=true" {
			t.Fatalf("CI=true: env %q", env)
		}
	}
	if n := strings.Count(buf.String(), "\n"); n != 1 || !strings.Contains(buf.String(), "CI=true") {
		t.Errorf("warnings = %q, want one line naming CI=true", buf.String())
	}
}

// probePnpm lets the auto probe run the real `pnpm --version` (the test
// binary's guard otherwise keeps it off) for the rest of t.
func probePnpm(t *testing.T) {
	t.Helper()
	pnpmVersionSeam = runPnpmVersion
	t.Cleanup(func() { pnpmVersionSeam = nil })
}

// fakePnpm puts a pnpm running body on PATH, in a directory of its own, and
// lets the probe run it.
func fakePnpm(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pnpm"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	probePnpm(t)
}

// Inside a test binary "auto" is off without a probe seam, whatever pnpm is
// on PATH, and the probe is not run at all.
func TestPnpmGVSAutoIsOffInTestsWithoutAProbe(t *testing.T) {
	log := filepath.Join(t.TempDir(), "probes")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pnpm"), []byte("#!/bin/sh\necho probe >> '"+log+"'\necho 12.0.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if PnpmGVS(gvsConfig(t.TempDir(), "auto")) {
		t.Error("auto is on in a test binary without a probe seam")
	}
	if _, err := os.Stat(log); err == nil {
		t.Error("pnpm ran without a probe seam")
	}
}

func gvsConfig(root string, setting any) *Resolved {
	values := Defaults()
	ObjectAt(values, "isolation")["pnpm_global_virtual_store"] = setting
	return &Resolved{Values: values, Root: root}
}

// "auto" follows the pnpm that runs in the config root: 12 and later turn the
// global virtual store on, older, unparsable or missing pnpm leave it off.
func TestPnpmGVSAutoFollowsThePnpmVersion(t *testing.T) {
	cases := []struct {
		name string
		body string // fake pnpm body ("" = no pnpm on PATH)
		want bool
	}{
		{name: "pnpm 12", body: "echo 12.0.0", want: true},
		{name: "pnpm 11", body: "echo 11.9.0"},
		{name: "pnpm 10.12.1", body: "echo 10.12.1"},
		{name: "garbage", body: "echo nonsense"},
		{name: "missing", body: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.body == "" {
				t.Setenv("PATH", t.TempDir())
				probePnpm(t)
			} else {
				fakePnpm(t, c.body)
			}
			if got := PnpmGVS(gvsConfig(t.TempDir(), "auto")); got != c.want {
				t.Errorf("PnpmGVS = %v, want %v", got, c.want)
			}
		})
	}
}

// The probe runs in the config root, not in the process's directory (the
// package directory, which holds no marker).
func TestPnpmGVSAutoProbesInTheConfigRoot(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "marker"), "")
	fakePnpm(t, "if [ -e marker ]; then echo 12.0.0; else echo 11.0.0; fi")
	if !PnpmGVS(gvsConfig(root, "auto")) {
		t.Errorf("PnpmGVS in %s = false, want true", root)
	}
}

// Every lookup of one root reuses the first probe.
func TestPnpmGVSAutoProbesOncePerRoot(t *testing.T) {
	log := filepath.Join(t.TempDir(), "probes")
	fakePnpm(t, "echo probe >> '"+log+"'\necho 12.0.0")
	root := t.TempDir()
	for range 3 {
		PnpmGVS(gvsConfig(root, "auto"))
	}
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(raw), "probe"); n != 1 {
		t.Errorf("pnpm ran %d times for one root, want 1", n)
	}
}

// The setting is true, false or "auto"; anything else is a config error.
func TestPnpmGVSSettingAcceptsOnlyBoolOrAuto(t *testing.T) {
	values := Defaults()
	if err := validatePnpmGVS(values); err != nil {
		t.Errorf("default: %v", err)
	}
	for _, bad := range []any{"yes", "true", "", nil, int64(1)} {
		ObjectAt(values, "isolation")["pnpm_global_virtual_store"] = bad
		if err := validatePnpmGVS(values); err == nil {
			t.Errorf("%#v accepted, want an error", bad)
		}
	}
}
