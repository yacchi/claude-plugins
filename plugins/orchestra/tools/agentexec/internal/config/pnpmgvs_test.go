package config

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
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
		name     string
		on       bool
		gitDir   bool   // .git is a directory: a main checkout
		unmarked bool   // a linked worktree orchestra did not prepare
		lock     string // pnpm-lock.yaml path below the root ("" = none)
		want     bool
	}{
		{name: "root project", on: true, lock: "pnpm-lock.yaml", want: true},
		{name: "per-package project", on: true, lock: "packages/a/pnpm-lock.yaml", want: true},
		{name: "classic node_modules", on: true, lock: "pnpm-lock.yaml", want: true},
		{name: "lockfile too deep", on: true, lock: "a/b/c/pnpm-lock.yaml"},
		{name: "lockfile in node_modules", on: true, lock: "node_modules/x/pnpm-lock.yaml"},
		{name: "lockfile in a dot dir", on: true, lock: ".cache/pnpm-lock.yaml"},
		{name: "main checkout", on: true, gitDir: true, lock: "pnpm-lock.yaml"},
		{name: "user's own worktree", on: true, unmarked: true, lock: "pnpm-lock.yaml"},
		{name: "no lockfile", on: true},
		{name: "setting off", lock: "pnpm-lock.yaml"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var tree string
			if c.gitDir {
				tree = t.TempDir()
				writeTestFile(t, filepath.Join(tree, ".git", "HEAD"), "ref: refs/heads/main\n")
			} else {
				marker := AdoptedMarker
				if c.unmarked {
					marker = ""
				}
				_, tree = linkedTree(t, marker)
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

// linkedTree is a linked worktree of a fresh main checkout, as git lays it
// out: the worktree's own git dir, main/.git/worktrees/w, holds a HEAD and the
// marker (a file name, "" = none), and the tree's .git file names that dir.
func linkedTree(t *testing.T, marker string) (main, tree string) {
	t.Helper()
	main = t.TempDir()
	gitdir := filepath.Join(main, ".git", "worktrees", "w")
	writeTestFile(t, filepath.Join(gitdir, "HEAD"), "ref: refs/heads/main\n")
	if marker != "" {
		writeTestFile(t, filepath.Join(gitdir, marker), "x\n")
	}
	tree = t.TempDir()
	writeTestFile(t, filepath.Join(tree, ".git"), "gitdir: "+gitdir+"\n")
	return main, tree
}

// A linked worktree is an orchestra tree when its own git dir holds a baseline
// or adopted marker. A relative gitdir line resolves against the tree, and a
// marker in the common git dir, shared by every worktree, does not count.
func TestOrchestraTree(t *testing.T) {
	cases := []struct {
		name     string
		marker   string // file in the worktree's own git dir
		common   string // file in the main checkout's git dir
		relative bool   // the gitdir line is relative to the tree
		want     bool
	}{
		{name: "adopted", marker: AdoptedMarker, want: true},
		{name: "baseline", marker: BaselineMarker, want: true},
		{name: "relative gitdir", marker: AdoptedMarker, relative: true, want: true},
		{name: "no marker", want: false},
		{name: "marker in the common git dir", common: BaselineMarker, want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			main := t.TempDir()
			gitdir := filepath.Join(main, ".git", "worktrees", "w")
			writeTestFile(t, filepath.Join(gitdir, "HEAD"), "ref: refs/heads/main\n")
			if c.marker != "" {
				writeTestFile(t, filepath.Join(gitdir, c.marker), "x\n")
			}
			if c.common != "" {
				writeTestFile(t, filepath.Join(main, ".git", c.common), "x\n")
			}
			tree := t.TempDir()
			line := gitdir
			if c.relative {
				rel, err := filepath.Rel(tree, gitdir)
				if err != nil {
					t.Fatal(err)
				}
				line = rel
			}
			writeTestFile(t, filepath.Join(tree, ".git"), "gitdir: "+line+"\n")
			if got := OrchestraTree(tree); got != c.want {
				t.Errorf("OrchestraTree = %v, want %v", got, c.want)
			}
		})
	}
}

// A main checkout is never an orchestra tree, marker or not, and neither is a
// tree whose .git names no git dir, or a git dir that is missing. A marker in
// the tree itself is not looked for.
func TestOrchestraTreeRejectsOtherGitLayouts(t *testing.T) {
	t.Run("main checkout", func(t *testing.T) {
		tree := t.TempDir()
		writeTestFile(t, filepath.Join(tree, ".git", AdoptedMarker), "x\n")
		if OrchestraTree(tree) {
			t.Error("main checkout is an orchestra tree")
		}
	})
	cases := []struct {
		name       string
		dotGit     string
		treeMarker bool // the marker sits in the tree's root
	}{
		{name: "no gitdir line", dotGit: "# no gitdir here\n"},
		{name: "gitdir missing", dotGit: "gitdir: /nonexistent/orchestra/worktrees/w\n"},
		{name: "marker in the tree itself", dotGit: "gitdir: /nonexistent/orchestra/worktrees/w\n", treeMarker: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tree := t.TempDir()
			writeTestFile(t, filepath.Join(tree, ".git"), c.dotGit)
			if c.treeMarker {
				writeTestFile(t, filepath.Join(tree, AdoptedMarker), "x\n")
			}
			if OrchestraTree(tree) {
				t.Errorf("OrchestraTree = true for %q", c.dotGit)
			}
		})
	}
}

// A project is classic when its node_modules/.modules.yaml names
// <project>/node_modules/.pnpm as its virtual store. A relative value resolves
// against that node_modules, quotes are removed, and the global layout's links
// dir, a missing node_modules or a missing key is not classic.
func TestPnpmClassicLayout(t *testing.T) {
	const global = "../../../../../Library/pnpm/store/v11/links"
	cases := []struct {
		name  string
		files map[string]string // paths below the tree; TREE in a content is the tree's path
		want  bool
	}{
		{name: "relative to the project's node_modules", files: map[string]string{
			"packages/a/pnpm-lock.yaml":             "",
			"packages/a/node_modules/.modules.yaml": "virtualStoreDir: .pnpm\n",
		}, want: true},
		{name: "absolute", files: map[string]string{
			"pnpm-lock.yaml":             "",
			"node_modules/.modules.yaml": "virtualStoreDir: TREE/node_modules/.pnpm\n",
		}, want: true},
		{name: "double-quoted absolute", files: map[string]string{
			"pnpm-lock.yaml":             "",
			"node_modules/.modules.yaml": "virtualStoreDir: \"TREE/node_modules/.pnpm\"\n",
		}, want: true},
		{name: "single-quoted relative", files: map[string]string{
			"pnpm-lock.yaml":             "",
			"node_modules/.modules.yaml": "virtualStoreDir: '.pnpm'\n",
		}, want: true},
		{name: "global links dir", files: map[string]string{
			"pnpm-lock.yaml":             "",
			"node_modules/.modules.yaml": "virtualStoreDir: " + global + "\n",
		}},
		{name: "no node_modules", files: map[string]string{
			"pnpm-lock.yaml": "",
		}},
		{name: "no virtualStoreDir key", files: map[string]string{
			"pnpm-lock.yaml":             "",
			"node_modules/.modules.yaml": "storeDir: .pnpm\n",
		}},
		{name: "one classic project among several", files: map[string]string{
			"pnpm-lock.yaml":                        "",
			"node_modules/.modules.yaml":            "virtualStoreDir: " + global + "\n",
			"packages/a/pnpm-lock.yaml":             "",
			"packages/a/node_modules/.modules.yaml": "virtualStoreDir: .pnpm\n",
		}, want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tree := t.TempDir()
			for p, content := range c.files {
				writeTestFile(t, filepath.Join(tree, p), strings.ReplaceAll(content, "TREE", tree))
			}
			if got := PnpmClassicLayout(tree); got != c.want {
				t.Errorf("PnpmClassicLayout = %v, want %v", got, c.want)
			}
		})
	}
}

// The switch reaches a process through TreeEntries and PnpmGVSShellPrefix
// alike, and only in an orchestra tree that is not classic. A user's own
// worktree gets neither the switch nor the store pin; a classic orchestra tree
// gets no switch and no prefix, though TreeEntries still pins its store.
func TestGVSSwitchEntryPoints(t *testing.T) {
	cases := []struct {
		name    string
		marker  string // the worktree's marker ("" = the user's own worktree)
		classic bool   // node_modules keeps its own store
		want    bool   // the switch reaches the process
	}{
		{name: "orchestra tree", marker: AdoptedMarker, want: true},
		{name: "user's own worktree", want: false},
		{name: "classic orchestra tree", marker: BaselineMarker, classic: true, want: false},
	}
	env := []string{"HOME=/h"}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, tree := linkedTree(t, c.marker)
			writeTestFile(t, filepath.Join(tree, "pnpm-lock.yaml"), "lockfileVersion: 9\n")
			if c.classic {
				writeTestFile(t, filepath.Join(tree, "node_modules", ".modules.yaml"), "virtualStoreDir: .pnpm\n")
			}
			entries := TreeEntries(true, tree, env)
			if got := slices.Contains(entries, PnpmGVSEnv); got != c.want {
				t.Errorf("TreeEntries carries the switch = %v, want %v", got, c.want)
			}
			if got, want := slices.ContainsFunc(entries, isPnpmStoreEntry), c.marker != ""; got != want {
				t.Errorf("TreeEntries carries the store pin = %v, want %v", got, want)
			}
			prefix := PnpmGVSShellPrefix(true, tree, env)
			if c.want && !strings.Contains(prefix, PnpmGVSEnv) {
				t.Errorf("shell prefix %q lacks the switch", prefix)
			}
			if !c.want && prefix != "" {
				t.Errorf("shell prefix = %q, want none", prefix)
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
