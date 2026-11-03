// Package paths resolves the machine-wide locations agent-exec reads and
// writes, with the environment overrides tests and sandboxed runs use.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Home is $HOME.
func Home() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "/"
	}
	return home
}

func expandUser(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		return Home() + p[1:]
	}
	return p
}

// StateDir is the machine-local state directory: $XDG_STATE_HOME/claude-orchestra
// when XDG_STATE_HOME is absolute, else ~/.local/state/claude-orchestra,
// symlinks resolved as far as the path exists. Every runtime file of
// orchestra lives under it.
func StateDir() string {
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" && filepath.IsAbs(xdg) {
		return RealPath(filepath.Join(xdg, "claude-orchestra"))
	}
	return RealPath(filepath.Join(RealPath(Home()), ".local", "state", "claude-orchestra"))
}

// PlansDir is StateDir()/plans: one directory per plan id. Plans, specs,
// answers and wave state live here, not in /tmp, because a reboot wipes
// temporary directories and with them a run's whole plan.
func PlansDir() string {
	return filepath.Join(StateDir(), "plans")
}

// PlanDir is PlansDir()/id.
func PlanDir(id string) string {
	return filepath.Join(PlansDir(), id)
}

// TempRootsSeam replaces the list of temporary roots TempRoot compares
// against (a test seam).
var TempRootsSeam func() []string

// tempRoots is the list TempRoot compares against. Inside a test binary the
// default list is empty, so tests that do not set the seam refuse nothing.
func tempRoots() []string {
	if TempRootsSeam != nil {
		return TempRootsSeam()
	}
	if testing.Testing() {
		return nil
	}
	return []string{"/tmp", "/private/tmp", os.TempDir()}
}

// TempRoot is the temporary root p lies under: "/tmp", "/private/tmp" or
// os.TempDir() ($TMPDIR), returned as that root is written in the list; ""
// when none. A relative p is made absolute first; both sides are compared
// with RealPath (so /tmp and /var/folders read as /private/...), at a path
// separator boundary (p equal to the root counts; /tmpfoo is not under /tmp).
func TempRoot(p string) string {
	abs := RealPath(p)
	for _, root := range tempRoots() {
		if root == "" {
			continue
		}
		r := RealPath(root)
		if r == "/" {
			continue
		}
		if abs == r || strings.HasPrefix(abs, r+"/") {
			return root
		}
	}
	return ""
}

// RefuseTemp is nil unless p lies under a temporary root; then an error
// naming the flag, the path, the root and the plan directory to use instead.
func RefuseTemp(flag, p string) error {
	root := TempRoot(p)
	if root == "" {
		return nil
	}
	return fmt.Errorf("%s %s is under %s, which a reboot wipes; keep plans, specs, answers and wave state in the plan directory %s/<plan id>/ (agent-exec plan dir --id <plan id> prints it)", flag, p, root, PlansDir())
}

// LedgerDir is the default run ledger: $StateDir/runs. Its siblings tokens/,
// sessions/ and check-slots/ follow it wherever ledger.dir points.
func LedgerDir() string {
	return filepath.Join(StateDir(), "runs")
}

// TelemetryDir is the default telemetry directory: $StateDir/telemetry.
func TelemetryDir() string {
	return filepath.Join(StateDir(), "telemetry")
}

// CooldownFile is the executor-state file's name, in StateDir by default.
const CooldownFile = "executor-state.json"

// CooldownPath is the default cooldown state file: $StateDir/executor-state.json.
func CooldownPath() string {
	return filepath.Join(StateDir(), CooldownFile)
}

// AliveDir holds the per-session heartbeat files: $ORCHESTRA_ALIVE_DIR, else
// $StateDir/alive. (Callers holding a config may follow a relocated ledger
// dir instead.)
func AliveDir() string {
	if v := os.Getenv("ORCHESTRA_ALIVE_DIR"); v != "" {
		return expandUser(v)
	}
	return filepath.Join(StateDir(), "alive")
}

// ClaudeConfigDir is $CLAUDE_CONFIG_DIR, else ~/.claude.
func ClaudeConfigDir() string {
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		return expandUser(v)
	}
	return filepath.Join(Home(), ".claude")
}

// SessionID is the calling Claude Code session ($CLAUDE_CODE_SESSION_ID).
func SessionID() (string, bool) {
	v := os.Getenv("CLAUDE_CODE_SESSION_ID")
	return v, v != ""
}

// RealPath is an absolute path with every
// existing symlink resolved; components that do not exist are kept as given.
func RealPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r
	}
	dir, base := filepath.Split(abs)
	dir = filepath.Clean(dir)
	if dir == abs || base == "" {
		return abs
	}
	return filepath.Join(RealPath(dir), base)
}
