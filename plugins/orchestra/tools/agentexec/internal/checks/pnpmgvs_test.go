package checks

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
)

// Every command a check runs in a tree on pnpm's global virtual store
// (ensure, the check, the retry) carries the switch: one without it would
// rebuild the classic layout. The user's checkout never gets it.
func TestCommandsInAPnpmGVSTreeCarryTheSwitch(t *testing.T) {
	was := config.PnpmGVSOn
	config.PnpmGVSOn = func() bool { return true }
	t.Cleanup(func() { config.PnpmGVSOn = was })

	const hasSwitch = `test "$pnpm_config_enable_global_virtual_store" = true`
	linked := t.TempDir()
	write(t, linked, ".git", "gitdir: /repo/.git/worktrees/t\n")
	write(t, linked, "pnpm-lock.yaml", "")
	write(t, linked, "pkg/a.test.ts", "")
	main := t.TempDir()
	write(t, main, ".git/HEAD", "ref: refs/heads/main\n")
	write(t, main, "pnpm-lock.yaml", "")

	o := runOpts(t)
	o.Ensure = []Ensure{{Exists: "installed", Run: hasSwitch + " && : > installed"}}
	items := []Item{{Name: "sub", Cwd: "pkg", Run: hasSwitch}}
	if got := statuses(Run(linked, items, o)); !reflect.DeepEqual(got, []string{"sub=pass"}) {
		t.Errorf("linked tree: %v", got)
	}
	if status, _ := RerunFailedAlone(" FAIL  pkg/a.test.ts\n", hasSwitch, linked, filepath.Join(t.TempDir(), "s"), 1, 0); status != "pass" {
		t.Errorf("retry in the linked tree: %s", status)
	}
	if got := statuses(Run(main, []Item{{Name: "main", Run: "! " + hasSwitch}}, runOpts(t))); !reflect.DeepEqual(got, []string{"main=pass"}) {
		t.Errorf("main checkout got the switch: %v", got)
	}
}

// Every command a check runs in a linked worktree that mise trusts (ensure,
// the check) sees the tree on MISE_TRUSTED_CONFIG_PATHS, so mise reads the
// tree's config. The user's checkout never gets it.
func TestCommandsInAMiseTrustedTreeSeeTheTree(t *testing.T) {
	wasOn, wasShow := config.PnpmGVSOn, config.MiseShowSeam
	config.PnpmGVSOn = func() bool { return false }
	t.Cleanup(func() { config.PnpmGVSOn = wasOn })
	t.Setenv("MISE_TRUSTED_CONFIG_PATHS", "")

	main := t.TempDir()
	write(t, main, ".git/HEAD", "ref: refs/heads/main\n")
	config.MiseShowSeam = func(dir string) ([]byte, error) { return []byte(dir + "/mise.toml: trusted\n"), nil }
	t.Cleanup(func() { config.MiseShowSeam = wasShow })
	linked := t.TempDir()
	write(t, linked, ".git", "gitdir: "+filepath.Join(main, ".git", "worktrees", "t")+"\n")
	write(t, linked, "mise.toml", "[env]\n")
	write(t, linked, "pkg/a.test.ts", "")

	has := `test "$MISE_TRUSTED_CONFIG_PATHS" = "` + linked + `"`
	o := runOpts(t)
	o.Ensure = []Ensure{{Exists: "installed", Run: has + " && : > installed"}}
	items := []Item{{Name: "sub", Cwd: "pkg", Run: has}}
	if got := statuses(Run(linked, items, o)); !reflect.DeepEqual(got, []string{"sub=pass"}) {
		t.Errorf("linked tree: %v", got)
	}
	if got := statuses(Run(main, []Item{{Name: "main", Run: "! " + has}}, runOpts(t))); !reflect.DeepEqual(got, []string{"main=pass"}) {
		t.Errorf("main checkout got the tree: %v", got)
	}
}
