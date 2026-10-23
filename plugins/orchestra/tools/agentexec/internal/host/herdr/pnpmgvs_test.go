package herdr

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
)

// The server's panes inherit its env, so the pnpm global-virtual-store
// switch is set on the server, per repository: only when the setting is on
// and the repository is a pnpm one.
func TestServerEnvCarriesThePnpmGVSSwitchPerRepository(t *testing.T) {
	for _, c := range []struct {
		label, cfg string
		lock, want bool
	}{
		{"on", `{"isolation": {"pnpm_global_virtual_store": true}}`, true, true},
		{"setting off", "", true, false},
		{"no lockfile", `{"isolation": {"pnpm_global_virtual_store": true}}`, false, false},
	} {
		t.Run(c.label, func(t *testing.T) {
			e := newEnv(t)
			e.scenario(map[string]any{}, true)
			if c.lock {
				_ = os.WriteFile(filepath.Join(e.repo, "pnpm-lock.yaml"), []byte("lockfileVersion: '9.0'\n"), 0o644)
			}
			t.Chdir(e.repo)
			e.mustStart(e.executor(c.cfg), "a")
			if len(e.spawnEnvs) == 0 {
				t.Fatal("no server spawned")
			}
			if got := slices.Contains(e.spawnEnvs[0], config.PnpmGVSEnv); got != c.want {
				t.Errorf("server env has the switch = %v, want %v", got, c.want)
			}
		})
	}
}
