package isolate

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// When `git gtr` (git-worktree-runner) is installed, worktrees are created
// through it, so the user's own gtr.copy.include patterns and postCreate
// hooks apply to orchestra's worktrees too. orchestra never writes gtr
// config: what it needs is lent to one subprocess through GIT_CONFIG_COUNT.
// That includes the tree's directory (gtr.worktrees.dir), so a gtr tree
// lands where a git-backend tree would.

// gtrFallbackCopyInclude is only lent when the user configured no copy
// patterns of their own.
var gtrFallbackCopyInclude = []string{".env", ".env.*", "*.local"}

func gtrAvailable() bool {
	_, err := gitx.Run("", "gtr", "version")
	return err == nil
}

func gtrConfigPairs(root string) [][2]string {
	if out, err := gitx.Run(root, "config", "--get-all", "gtr.copy.include"); err == nil && strings.TrimSpace(out) != "" {
		return nil
	}
	pairs := make([][2]string, 0, len(gtrFallbackCopyInclude))
	for _, p := range gtrFallbackCopyInclude {
		pairs = append(pairs, [2]string{"gtr.copy.include", p})
	}
	return pairs
}

// createViaGtr returns the new worktree's path, or "" when gtr refused or
// put the tree anywhere but directly in dir. dir is lent to gtr as
// gtr.worktrees.dir for this one subprocess, next to the other lent pairs;
// a repository's own `git config --local gtr.worktrees.dir` still wins
// inside gtr, so the result is checked, and a tree gtr put elsewhere is
// discarded at once (worktree and branch), letting Create fall back to git.
func createViaGtr(root, branch, dir string, pairs [][2]string) string {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	lent := append([][2]string{{"gtr.worktrees.dir", dir}}, pairs...)
	env := gitx.ConfigEnv(lent, nil)
	release := HoldHeavy("git gtr new")
	_, err := gitx.RunEnv(root, env, "gtr", "new", branch, "--from-current", "--yes", "--no-fetch")
	release()
	if err != nil {
		return ""
	}
	out, err := gitx.RunEnv(root, env, "gtr", "go", branch)
	if err != nil {
		return ""
	}
	path := strings.TrimSpace(out)
	if path == "" || !isDir(path) {
		return ""
	}
	if paths.RealPath(filepath.Dir(path)) != paths.RealPath(dir) {
		_, _ = discard(root, path, branch)
		return ""
	}
	return path
}
