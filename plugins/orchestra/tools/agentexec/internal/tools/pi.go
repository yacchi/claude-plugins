package tools

import (
	"os"
	"path/filepath"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// PiAgentDirEnv points pi at its agent directory (auth, sessions, config).
const PiAgentDirEnv = "PI_CODING_AGENT_DIR"

// Modes of external_executors.pi.agent_dir.
const (
	PiAgentManaged = "managed" // <state dir>/pi-agent, created on use
	PiAgentUser    = "user"    // pi's own default (~/.pi/agent), untouched
	PiAgentPath    = "path"    // an explicit directory
)

func piSection(cfg *config.Resolved) *jsonx.Object {
	if cfg == nil {
		return config.Defaults().Object("external_executors").Object("pi")
	}
	return cfg.Section("external_executors").Object("pi")
}

// PiAgentDir is the mode of pi's agent dir and the directory ("" in user
// mode): agent_dir unset, empty or "managed" is the managed dir, "user" is
// pi's default, any other string an explicit path (~ expanded, absolute).
func PiAgentDir(cfg *config.Resolved) (mode, dir string) {
	managed := filepath.Join(paths.StateDir(), "pi-agent")
	v, _ := piSection(cfg).Get("agent_dir")
	switch s := v.(type) {
	case nil:
		return PiAgentManaged, managed
	case string:
		switch s {
		case "", PiAgentManaged:
			return PiAgentManaged, managed
		case PiAgentUser:
			return PiAgentUser, ""
		}
		p := fsutil.ExpandUser(s)
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		return PiAgentPath, p
	}
	return PiAgentManaged, managed
}

// PiContextFiles is whether pi may discover AGENTS.md/CLAUDE.md (false only
// for an explicit `context_files: false`).
func PiContextFiles(cfg *config.Resolved) bool {
	v, _ := piSection(cfg).Get("context_files")
	return v != false
}

// EnsurePiAgentDir returns pi's agent dir under cfg ("" in user mode). The
// managed dir is created (0700) and, only while it has no auth.json, that
// file is symlinked to the user's existing one ($PI_CODING_AGENT_DIR when it
// names another dir, else ~/.pi/agent) so a login carries over. An existing
// file or symlink is never overwritten.
func EnsurePiAgentDir(cfg *config.Resolved) (string, error) {
	mode, dir := PiAgentDir(cfg)
	if mode != PiAgentManaged {
		return dir, nil
	}
	return dir, ensureManaged(dir)
}

func ensureManaged(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	auth := filepath.Join(dir, "auth.json")
	if _, err := os.Lstat(auth); err == nil {
		return nil
	}
	source := filepath.Join(paths.Home(), ".pi", "agent")
	if override := os.Getenv(PiAgentDirEnv); override != "" &&
		paths.RealPath(override) != paths.RealPath(dir) {
		source = override
	}
	sourceAuth := filepath.Join(source, "auth.json")
	if st, err := os.Stat(sourceAuth); err != nil || !st.Mode().IsRegular() ||
		paths.RealPath(sourceAuth) == paths.RealPath(auth) {
		return nil
	}
	_ = os.Symlink(sourceAuth, auth) // best effort, as a login carry-over
	return nil
}

// PiEnvOverrides are the env additions of a pi child: PI_CODING_AGENT_DIR
// for a managed (ensured first) or explicit dir, nothing in user mode. A
// managed dir that cannot be created is still named (pi reports it).
func PiEnvOverrides(cfg *config.Resolved) map[string]string {
	dir, _ := EnsurePiAgentDir(cfg)
	if dir == "" {
		return map[string]string{}
	}
	return map[string]string{PiAgentDirEnv: dir}
}
