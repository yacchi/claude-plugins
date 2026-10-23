package integrate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
)

// --verify, --full and their bisect run in orchestra's own trees: under
// pnpm's global virtual store they must carry the switch, or the first pnpm
// they run rebuilds the classic layout.
func TestRunVerifyInAPnpmGVSTreeCarriesTheSwitch(t *testing.T) {
	was := config.PnpmGVSOn
	t.Cleanup(func() { config.PnpmGVSOn = was })

	const hasSwitch = `test "$pnpm_config_enable_global_virtual_store" = true`
	linked := t.TempDir()
	for name, body := range map[string]string{".git": "gitdir: /repo/.git/worktrees/t\n", "pnpm-lock.yaml": ""} {
		if err := os.WriteFile(filepath.Join(linked, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	config.PnpmGVSOn = func() bool { return true }
	if v := RunVerify(linked, hasSwitch, 0); v.Status != "pass" {
		t.Errorf("setting on: %s (%s)", v.Status, v.Excerpt)
	}
	config.PnpmGVSOn = func() bool { return false }
	if v := RunVerify(linked, "! "+hasSwitch, 0); v.Status != "pass" {
		t.Errorf("setting off got the switch: %s (%s)", v.Status, v.Excerpt)
	}
}
