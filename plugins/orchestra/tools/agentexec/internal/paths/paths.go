// Package paths resolves the machine-wide locations agent-exec reads and
// writes, with the environment overrides tests and sandboxed runs use.
package paths

import (
	"os"
	"path/filepath"
	"strings"
)

// Home is $HOME (os.path.expanduser("~")).
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

func absExpanded(p string) string {
	abs, err := filepath.Abs(expandUser(p))
	if err != nil {
		return expandUser(p)
	}
	return abs
}

// OrchestraHome is ~/.claude/orchestra.
func OrchestraHome() string {
	return filepath.Join(Home(), ".claude", "orchestra")
}

// StateDir is the machine-local state directory: $XDG_STATE_HOME/claude-orchestra
// when XDG_STATE_HOME is absolute, else ~/.local/state/claude-orchestra,
// symlinks resolved as far as the path exists.
func StateDir() string {
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" && filepath.IsAbs(xdg) {
		return RealPath(filepath.Join(xdg, "claude-orchestra"))
	}
	return RealPath(filepath.Join(RealPath(Home()), ".local", "state", "claude-orchestra"))
}

// AliveDir holds the per-session heartbeat files: $ORCHESTRA_ALIVE_DIR, else
// ~/.claude/orchestra/alive. (The Python default followed a relocated
// ledger dir; callers holding a config may do the same.)
func AliveDir() string {
	if v := os.Getenv("ORCHESTRA_ALIVE_DIR"); v != "" {
		return expandUser(v)
	}
	return filepath.Join(OrchestraHome(), "alive")
}

// UIStatePath is the dashboard's ui.json: $ORCHESTRA_UI_STATE, else
// ~/.claude/orchestra/ui.json.
func UIStatePath() string {
	if v := os.Getenv("ORCHESTRA_UI_STATE"); v != "" {
		return absExpanded(v)
	}
	return filepath.Join(OrchestraHome(), "ui.json")
}

// WaveRegistryPath is $ORCHESTRA_WAVE_REGISTRY, else
// ~/.claude/orchestra/waves.jsonl.
func WaveRegistryPath() string {
	if v := os.Getenv("ORCHESTRA_WAVE_REGISTRY"); v != "" {
		return absExpanded(v)
	}
	return filepath.Join(OrchestraHome(), "waves.jsonl")
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

// RealPath is Python's os.path.realpath: an absolute path with every
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
