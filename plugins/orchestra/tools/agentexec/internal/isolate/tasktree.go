package isolate

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// TaskTree reports whether dir lies in an orchestra TASK worktree: a linked
// worktree (its git dir differs from the common dir) whose git dir holds the
// baseline or the adoption marker, and whose role is not RoleIntegration.
// top is that worktree's top-level directory. A dir that does not exist is
// replaced by its nearest existing parent first. Anything git cannot answer
// is "not a task tree".
//
// The SessionStart hook uses this to tell a hosted worker session from the
// instructor's, so the main checkout must never qualify, even when a stray
// marker sits in its .git dir: the git-dir check comes before the marker.
func TaskTree(dir string) (top string, ok bool) {
	existing, ok := taskTreeExistingDir(dir)
	if !ok {
		return "", false
	}
	out, err := gitx.Run(existing, "rev-parse", "--path-format=absolute", "--git-dir", "--git-common-dir", "--show-toplevel")
	if err != nil {
		return "", false
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 3 {
		return "", false
	}
	gitDir, commonDir, top := lines[0], lines[1], lines[2]
	if paths.RealPath(gitDir) == paths.RealPath(commonDir) {
		return "", false
	}
	if readMarker(top, baselineFile) == "" {
		if _, adopted := readAdoption(top); !adopted {
			return "", false
		}
	}
	if ReadRole(top) == RoleIntegration {
		return "", false
	}
	return top, true
}

// taskTreeExistingDir is the nearest existing directory at or above dir.
// It reports false when none exists below the filesystem root.
func taskTreeExistingDir(dir string) (string, bool) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", false
	}
	for {
		if info, err := os.Stat(abs); err == nil && info.IsDir() {
			return abs, true
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", false
		}
		abs = parent
	}
}
