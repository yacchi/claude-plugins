package herdr

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
)

const gvsCfg = `{"isolation": {"pnpm_global_virtual_store": true}}`

// unsetPnpmStoreEnv clears the store variables of the process, whatever their
// letter case, so a developer's own store choice cannot change the pin the
// test expects. t restores them afterwards.
func unsetPnpmStoreEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.EqualFold(name, config.PnpmStorePinName) || strings.EqualFold(name, "npm_config_store_dir") {
			t.Setenv(name, "")
			_ = os.Unsetenv(name)
		}
	}
}

// pnpmStorePinEntry is the store pin of a linked tree in this process: pnpm's
// default store under the HOME the test set.
func pnpmStorePinEntry() string {
	return config.PnpmStorePinName + "=" + config.PnpmStoreDir(runtime.GOOS, os.Getenv("HOME"), os.Getenv)
}

// commitLockfile commits a pnpm-lock.yaml, so the linked worktrees of the
// repository carry one.
func (e *env) commitLockfile() {
	_ = os.WriteFile(filepath.Join(e.repo, "pnpm-lock.yaml"), []byte("lockfileVersion: '9.0'\n"), 0o644)
	gitRun(e.t, e.repo, "add", "pnpm-lock.yaml")
	gitRun(e.t, e.repo, "commit", "-q", "-m", "lock")
}

// linkedTree is a linked worktree of the repository under e.tmp.
func (e *env) linkedTree(name string) string {
	tree := filepath.Join(e.tmp, name)
	gitRun(e.t, e.repo, "worktree", "add", "-q", "-b", name, tree)
	return tree
}

// workspaceEnvOf opens an agent on tree and returns the --env values of the
// workspace create it issued. The agent may fail after that; only the
// workspace is under test.
func (e *env) workspaceEnvOf(x *Executor, tree string) []string {
	e.t.Helper()
	s := &host.Session{Pkg: "a", Name: "herdr", Worktree: tree,
		Extra: map[string]any{"repo": e.repo, "state_dir": e.state}}
	_, _ = x.openAgent(context.Background(), s, false)
	creates := e.callsOf("workspace create")
	if len(creates) == 0 {
		e.t.Fatal("no workspace create was issued")
	}
	var envs []string
	argv := creates[0].Argv
	for i, a := range argv {
		if a == "--env" && i+1 < len(argv) {
			envs = append(envs, argv[i+1])
		}
	}
	return envs
}

// A workspace gets the pnpm global-virtual-store switch only when its tree
// runs under it: a linked worktree of a pnpm repository, not the user's main
// checkout and not a classic node_modules. The isolation switches are on
// every workspace. A linked worktree also gets the store pin, before the switch.
func TestWorkspaceEnvCarriesThePnpmGVSSwitchOnlyForAGVSTree(t *testing.T) {
	for _, c := range []struct {
		label string
		tree  func(e *env) string
		pin   bool
		gvs   bool
	}{
		{"linked worktree", func(e *env) string { return e.linkedTree("gvs") }, true, true},
		{"main checkout", func(e *env) string { return e.repo }, false, false},
		{"classic node_modules", func(e *env) string {
			tree := e.linkedTree("classic")
			nm := filepath.Join(tree, "node_modules")
			_ = os.MkdirAll(nm, 0o755)
			_ = os.WriteFile(filepath.Join(nm, ".modules.yaml"), []byte("virtualStoreDir: .pnpm\n"), 0o644)
			return tree
		}, true, false},
	} {
		t.Run(c.label, func(t *testing.T) {
			unsetPnpmStoreEnv(t)
			e := newEnv(t)
			e.commitLockfile()
			e.scenario(map[string]any{}, true)
			iso, err := host.IsolationEnv(e.executor(gvsCfg).Config())
			if err != nil || len(iso) == 0 {
				t.Fatalf("isolation env = %v, %v; want the worker switches", iso, err)
			}
			want := slices.Clone(iso)
			if c.pin {
				want = append(want, pnpmStorePinEntry())
			}
			if c.gvs {
				want = append(want, config.PnpmGVSEnv)
			}
			got := e.workspaceEnvOf(e.executor(gvsCfg), c.tree(e))
			if !slices.Equal(got, want) {
				t.Errorf("workspace --env = %v, want %v", got, want)
			}
		})
	}
}

// A user's own store choice in the process env is kept: a linked tree gets no
// second pin beside pnpm_config_store_dir, and an npm-named store becomes the
// pin's value, as config.PnpmStorePin reads it.
func TestWorkspaceEnvPinsTheStoreUnlessTheUserChoseOne(t *testing.T) {
	t.Run("pnpm name set", func(t *testing.T) {
		unsetPnpmStoreEnv(t)
		t.Setenv(config.PnpmStorePinName, "/mine")
		e := newEnv(t)
		got := e.workspaceEnvOf(e.executor("{}"), e.linkedTree("own"))
		for _, kv := range got {
			if strings.HasPrefix(kv, config.PnpmStorePinName+"=") {
				t.Errorf("workspace --env = %v, want no store pin", got)
			}
		}
	})
	t.Run("npm name set", func(t *testing.T) {
		unsetPnpmStoreEnv(t)
		t.Setenv("npm_config_store_dir", "/mine")
		e := newEnv(t)
		got := e.workspaceEnvOf(e.executor("{}"), e.linkedTree("npm"))
		if !slices.Contains(got, config.PnpmStorePinName+"=/mine") {
			t.Errorf("workspace --env = %v, want the store pinned to /mine", got)
		}
	})
}

// The server's env carries neither the pnpm switch nor the worker's
// isolation switches: each workspace gets its own, so a setting change
// reaches the next package start even on a running server.
func TestServerEnvCarriesNoWorkspaceEnv(t *testing.T) {
	e := newEnv(t)
	e.commitLockfile()
	e.scenario(map[string]any{}, true)
	iso, err := host.IsolationEnv(e.executor(gvsCfg).Config())
	if err != nil || len(iso) == 0 {
		t.Fatalf("isolation env = %v, %v; want the worker switches", iso, err)
	}
	e.mustStart(e.executor(gvsCfg), "a")
	if len(e.spawnEnvs) == 0 {
		t.Fatal("no server spawned")
	}
	for _, kv := range append(slices.Clone(iso), config.PnpmGVSEnv) {
		if slices.Contains(e.spawnEnvs[0], kv) {
			t.Errorf("server env has %q, want it on the workspaces only", kv)
		}
	}
}

// A workspace on a linked worktree with a mise config carries mise's trust of
// the tree, appended to the value the agent-exec process already has.
func TestWorkspaceEnvCarriesTheMiseTrustOfItsTree(t *testing.T) {
	t.Setenv("MISE_TRUSTED_CONFIG_PATHS", "/keep")
	was := config.MiseShowSeam
	config.MiseShowSeam = func(dir string) ([]byte, error) { return []byte(dir + "/mise.toml: trusted\n"), nil }
	t.Cleanup(func() { config.MiseShowSeam = was })

	e := newEnv(t)
	e.scenario(map[string]any{}, true)
	tree := e.linkedTree("mise")
	_ = os.WriteFile(filepath.Join(tree, "mise.toml"), []byte("[env]\n"), 0o644)
	want := "MISE_TRUSTED_CONFIG_PATHS=/keep:" + tree
	if got := e.workspaceEnvOf(e.executor("{}"), tree); !slices.Contains(got, want) {
		t.Errorf("workspace --env = %v, want %q", got, want)
	}
}
