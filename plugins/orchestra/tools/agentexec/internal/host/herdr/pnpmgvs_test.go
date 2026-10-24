package herdr

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
)

const gvsCfg = `{"isolation": {"pnpm_global_virtual_store": true}}`

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
// every workspace.
func TestWorkspaceEnvCarriesThePnpmGVSSwitchOnlyForAGVSTree(t *testing.T) {
	for _, c := range []struct {
		label string
		tree  func(e *env) string
		want  bool
	}{
		{"linked worktree", func(e *env) string { return e.linkedTree("gvs") }, true},
		{"main checkout", func(e *env) string { return e.repo }, false},
		{"classic node_modules", func(e *env) string {
			tree := e.linkedTree("classic")
			nm := filepath.Join(tree, "node_modules")
			_ = os.MkdirAll(nm, 0o755)
			_ = os.WriteFile(filepath.Join(nm, ".modules.yaml"), []byte("virtualStoreDir: .pnpm\n"), 0o644)
			return tree
		}, false},
	} {
		t.Run(c.label, func(t *testing.T) {
			e := newEnv(t)
			e.commitLockfile()
			e.scenario(map[string]any{}, true)
			iso, err := host.IsolationEnv(e.executor(gvsCfg).Config())
			if err != nil || len(iso) == 0 {
				t.Fatalf("isolation env = %v, %v; want the worker switches", iso, err)
			}
			want := iso
			if c.want {
				want = append(slices.Clone(iso), config.PnpmGVSEnv)
			}
			got := e.workspaceEnvOf(e.executor(gvsCfg), c.tree(e))
			if !slices.Equal(got, want) {
				t.Errorf("workspace --env = %v, want %v", got, want)
			}
		})
	}
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
