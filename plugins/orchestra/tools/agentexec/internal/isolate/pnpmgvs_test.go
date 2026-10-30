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
// failOn ("" = every call) or run in a directory holding .fail-pnpm, and
// otherwise installs a node_modules in its directory.
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
		"[ -e .fail-pnpm ] && { echo 'ERR_FAKE' >&2; exit 1; }\n" +
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

// linkedTree is a linked worktree of repo, as orchestra's trees are.
func linkedTree(t *testing.T, repo string) string {
	t.Helper()
	tree := filepath.Join(filepath.Dir(repo), "tree")
	git(t, repo, "worktree", "add", "-q", "-b", "t", tree)
	return tree
}

// perPackageRepo is a repository with one pnpm project per package (no root
// lockfile) and a cloned-looking node_modules in each, in the main tree.
func perPackageRepo(t *testing.T) string {
	t.Helper()
	repo := newRepo(t)
	for _, pkg := range []string{"a", "b"} {
		writeFile(t, filepath.Join(repo, "packages", pkg, "pnpm-lock.yaml"), "lockfileVersion: '9.0'\n")
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "locks")
	for _, pkg := range []string{"a", "b"} {
		writeFile(t, filepath.Join(repo, "packages", pkg, "node_modules", "dep", "index.js"), "x\n")
	}
	return repo
}

// Every per-package project is installed in its own directory, nothing of
// it cloned, and a symlinked node_modules already in the tree goes as a
// link without touching what it points at.
func TestCarryRebuildsEveryPnpmProjectUnderTheGlobalVirtualStore(t *testing.T) {
	repo := perPackageRepo(t)
	fakePnpm(t)
	setPnpmGVS(t, true)
	tree := linkedTree(t, repo)
	other := t.TempDir()
	writeFile(t, filepath.Join(other, "keep.js"), "x\n")
	if err := os.Symlink(other, filepath.Join(tree, "packages", "a", "node_modules")); err != nil {
		t.Fatal(err)
	}
	carried, method, present, note := CarryDependencies(repo, tree, nil)
	if method != carryPnpmGVS || note != "" {
		t.Errorf("method %q note %q, want %q and none", method, note, carryPnpmGVS)
	}
	if len(carried) != 0 || len(present) != 0 {
		t.Errorf("carried %q present %q, want none", carried, present)
	}
	if !exists(filepath.Join(other, "keep.js")) {
		t.Fatal("the symlinked node_modules was followed and its target deleted")
	}
	for _, pkg := range []string{"a", "b"} {
		nm := filepath.Join(tree, "packages", pkg, "node_modules")
		if isSymlink(nm) || !exists(filepath.Join(nm, ".modules.yaml")) || exists(filepath.Join(nm, "dep")) {
			t.Errorf("packages/%s/node_modules was not installed in place", pkg)
		}
	}
	if exists(filepath.Join(tree, "node_modules")) {
		t.Error("installed at the tree root, which is no pnpm project")
	}
}

// A project whose install fails gets its node_modules cloned and a note;
// the others are still installed, and the method is no longer pnpm-gvs.
func TestCarryClonesOnlyTheProjectWhoseInstallFailed(t *testing.T) {
	repo := perPackageRepo(t)
	fakePnpm(t)
	setPnpmGVS(t, true)
	tree := linkedTree(t, repo)
	writeFile(t, filepath.Join(tree, "packages", "a", ".fail-pnpm"), "")
	carried, method, _, note := CarryDependencies(repo, tree, nil)
	if method != carryClone {
		t.Errorf("method = %q, want %q", method, carryClone)
	}
	if strings.Join(carried, ",") != filepath.Join("packages", "a", "node_modules") {
		t.Errorf("carried = %q, want packages/a/node_modules only", carried)
	}
	if !strings.Contains(note, "pnpm install (gvs) failed in packages/a") || strings.Contains(note, "packages/b") {
		t.Errorf("note = %q, want packages/a's failure only", note)
	}
	if !exists(filepath.Join(tree, "packages", "a", "node_modules", "dep", "index.js")) {
		t.Error("packages/a/node_modules was not cloned after its failed install")
	}
	if !exists(filepath.Join(tree, "packages", "b", "node_modules", ".modules.yaml")) {
		t.Error("packages/b was not installed")
	}
}

// Without the global virtual store a cloned node_modules whose links dangle
// at the tree's depth is removed and its project reinstalled without the
// switch; one outside any project is only removed, with a note.
func TestCarryRepairsDanglingLinksWithoutTheGlobalVirtualStore(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, filepath.Join(repo, "app", "pnpm-lock.yaml"), "lockfileVersion: '9.0'\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "lock")
	// ../../../store resolves from repo/<dir>/node_modules but not from the
	// tree's copy, whose parent holds no store.
	writeFile(t, filepath.Join(filepath.Dir(repo), "store", "dep", "index.js"), "x\n")
	for _, dir := range []string{"app", "loose"} {
		writeFile(t, filepath.Join(repo, dir, "node_modules", "ok", "index.js"), "x\n")
		if err := os.MkdirAll(filepath.Join(repo, dir, "node_modules", "@scope"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../../../../store/dep", filepath.Join(repo, dir, "node_modules", "@scope", "dep")); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(repo, "fine", "node_modules", "ok", "index.js"), "x\n")
	log := fakePnpm(t)
	setPnpmGVS(t, false)
	parent := filepath.Join(filepath.Dir(repo), "trees")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(parent, "deeper")
	git(t, repo, "worktree", "add", "-q", "-b", "t", tree)
	carried, _, _, note := CarryDependencies(repo, tree, nil)
	want := strings.Join([]string{filepath.Join("app", "node_modules"), filepath.Join("fine", "node_modules")}, ",")
	if strings.Join(carried, ",") != want {
		t.Errorf("carried = %q, want %q", carried, want)
	}
	if !exists(filepath.Join(tree, "app", "node_modules", ".modules.yaml")) || exists(filepath.Join(tree, "app", "node_modules", "ok")) {
		t.Error("app/node_modules was not reinstalled")
	}
	if exists(filepath.Join(tree, "loose", "node_modules")) {
		t.Error("loose/node_modules with dangling links was kept")
	}
	if !exists(filepath.Join(tree, "fine", "node_modules", "ok", "index.js")) {
		t.Error("a node_modules without dangling links was removed")
	}
	if !strings.Contains(note, filepath.Join("loose", "node_modules")) {
		t.Errorf("note = %q, want it to name loose/node_modules", note)
	}
	b, _ := os.ReadFile(log)
	if got := strings.TrimSpace(string(b)); got != "install --frozen-lockfile --offline gvs=" {
		t.Errorf("pnpm calls = %q, want one offline install without the switch", got)
	}
}
