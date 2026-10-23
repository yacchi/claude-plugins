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
