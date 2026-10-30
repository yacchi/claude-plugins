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

// The env reaches linked worktrees holding a pnpm project, whatever layout
// an existing node_modules has (the carry rebuilds it), and never a main
// checkout.
func TestPnpmGVSTree(t *testing.T) {
	cases := []struct {
		name   string
		on     bool
		gitDir bool   // .git is a directory: a main checkout
		lock   string // pnpm-lock.yaml path below the root ("" = none)
		want   bool
	}{
		{name: "root project", on: true, lock: "pnpm-lock.yaml", want: true},
		{name: "per-package project", on: true, lock: "packages/a/pnpm-lock.yaml", want: true},
		{name: "classic node_modules", on: true, lock: "pnpm-lock.yaml", want: true},
		{name: "lockfile too deep", on: true, lock: "a/b/c/pnpm-lock.yaml"},
		{name: "lockfile in node_modules", on: true, lock: "node_modules/x/pnpm-lock.yaml"},
		{name: "lockfile in a dot dir", on: true, lock: ".cache/pnpm-lock.yaml"},
		{name: "main checkout", on: true, gitDir: true, lock: "pnpm-lock.yaml"},
		{name: "no lockfile", on: true},
		{name: "setting off", lock: "pnpm-lock.yaml"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tree := t.TempDir()
			if c.gitDir {
				writeTestFile(t, filepath.Join(tree, ".git", "HEAD"), "ref: refs/heads/main\n")
			} else {
				writeTestFile(t, filepath.Join(tree, ".git"), "gitdir: /repo/.git/worktrees/t\n")
			}
			if c.lock != "" {
				writeTestFile(t, filepath.Join(tree, c.lock), "lockfileVersion: '9.0'\n")
			}
			writeTestFile(t, filepath.Join(tree, "node_modules", ".modules.yaml"), "virtualStoreDir: .pnpm\n")
			if got := PnpmGVSTree(c.on, tree); got != c.want {
				t.Errorf("PnpmGVSTree = %v, want %v", got, c.want)
			}
		})
	}
}

// Projects are every directory holding its own lockfile, root included, in
// sorted order; a member without one, and anything behind a symlink, is not.
func TestPnpmProjects(t *testing.T) {
	tree := t.TempDir()
	outside := t.TempDir()
	for _, f := range []string{
		"pnpm-lock.yaml", "packages/b/pnpm-lock.yaml", "packages/a/pnpm-lock.yaml",
		"packages/member/package.json", "apps/web/pnpm-lock.yaml",
	} {
		writeTestFile(t, filepath.Join(tree, f), "")
	}
	writeTestFile(t, filepath.Join(outside, "pnpm-lock.yaml"), "")
	if err := os.Symlink(outside, filepath.Join(tree, "linked")); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(PnpmProjects(tree), ",")
	if want := ".,apps/web,packages/a,packages/b"; got != want {
		t.Errorf("PnpmProjects = %q, want %q", got, want)
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
	on, reason := PnpmGVSStatus(gvsConfig(t.TempDir(), "auto"))
	if on {
		t.Error("auto is on in a test binary without a probe seam")
	}
	if reason != "auto: not probed in tests" {
		t.Errorf("reason = %q, want %q", reason, "auto: not probed in tests")
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

// A bool setting is its own reason, whatever pnpm is installed.
func TestPnpmGVSStatusReasonForBoolSettings(t *testing.T) {
	cases := []struct {
		setting any
		on      bool
		reason  string
	}{
		{setting: true, on: true, reason: "set to true"},
		{setting: false, reason: "set to false"},
	}
	for _, c := range cases {
		on, reason := PnpmGVSStatus(gvsConfig(t.TempDir(), c.setting))
		if on != c.on || reason != c.reason {
			t.Errorf("setting %v: got (%v, %q), want (%v, %q)", c.setting, on, reason, c.on, c.reason)
		}
	}
}

// "auto" follows the pnpm that runs in the config root: 11 and later turn the
// global virtual store on, older, unparsable or missing pnpm leave it off.
func TestPnpmGVSAutoFollowsThePnpmVersion(t *testing.T) {
	cases := []struct {
		name   string
		body   string // fake pnpm body ("" = no pnpm on PATH)
		want   bool
		reason string // "" = not checked (the missing case has an exec-specific error)
	}{
		{name: "pnpm 12", body: "echo 12.0.0", want: true, reason: "auto: pnpm 12.0.0 (11 or later)"},
		{name: "pnpm 11", body: "echo 11.0.9", want: true, reason: "auto: pnpm 11.0.9 (11 or later)"},
		{name: "pnpm 12 prerelease", body: "echo 12.0.0-beta.1", want: true,
			reason: "auto: pnpm 12.0.0-beta.1 (11 or later)"},
		{name: "pnpm 10.12.1", body: "echo 10.12.1", reason: "auto: pnpm 10.12.1 is older than 11"},
		{name: "garbage", body: "echo nonsense", reason: "auto: pnpm version unknown (nonsense)"},
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
			on, reason := PnpmGVSStatus(gvsConfig(t.TempDir(), "auto"))
			if on != c.want {
				t.Errorf("PnpmGVS = %v, want %v", on, c.want)
			}
			if c.reason != "" && reason != c.reason {
				t.Errorf("reason = %q, want %q", reason, c.reason)
			}
			if c.body == "" && !strings.HasPrefix(reason, "auto: pnpm version unknown (") {
				t.Errorf("missing pnpm: reason = %q, want a version-unknown reason", reason)
			}
		})
	}
}

// The probe runs in the config root, not in the process's directory (the
// package directory, which holds no marker).
func TestPnpmGVSAutoProbesInTheConfigRoot(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "marker"), "")
	fakePnpm(t, "if [ -e marker ]; then echo 11.0.0; else echo 10.0.0; fi")
	if !PnpmGVS(gvsConfig(root, "auto")) {
		t.Errorf("PnpmGVS in %s = false, want true", root)
	}
}

// Every lookup of one root reuses the first probe, reason included.
func TestPnpmGVSAutoProbesOncePerRoot(t *testing.T) {
	log := filepath.Join(t.TempDir(), "probes")
	fakePnpm(t, "echo probe >> '"+log+"'\necho 10.12.1")
	root := t.TempDir()
	var first string
	for i := range 3 {
		on, reason := PnpmGVSStatus(gvsConfig(root, "auto"))
		if on {
			t.Fatalf("lookup %d: on for pnpm 10.12.1", i)
		}
		if i == 0 {
			first = reason
		} else if reason != first {
			t.Errorf("lookup %d reason = %q, want the first %q", i, reason, first)
		}
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
