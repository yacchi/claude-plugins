package config

import (
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/procx"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The prefix sets the store pin and the GVS switch of a linked worktree that
// runs under the store, and a shell reads back the values, quoted where the
// store path needs it.
func TestPnpmGVSShellPrefix(t *testing.T) {
	main, linked := miseTrees(t)
	writeTestFile(t, filepath.Join(linked, "pnpm-lock.yaml"), "lockfileVersion: 9\n")
	writeTestFile(t, filepath.Join(main, "pnpm-lock.yaml"), "lockfileVersion: 9\n")
	env := []string{"HOME=" + absAt("/h"), "PNPM_HOME=" + absAt("/my store")}

	prefix := PnpmGVSShellPrefix(true, linked, env)
	cmd := exec.Command(procx.ShellPath(), "-c", prefix+`printf '%s|%s' "$pnpm_config_store_dir" "$pnpm_config_enable_global_virtual_store"`)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(out), filepath.Join(absAt("/my store"), "store")+"|true"; got != want {
		t.Errorf("shell saw %q, want %q (prefix %q)", got, want, prefix)
	}

	if p := PnpmGVSShellPrefix(true, main, env); p != "" {
		t.Errorf("main checkout got prefix %q", p)
	}
	if p := PnpmGVSShellPrefix(false, linked, env); p != "" {
		t.Errorf("setting off got prefix %q", p)
	}
}
