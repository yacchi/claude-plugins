package isolate

import (
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
)

// When `git gtr` (git-worktree-runner) is installed, worktrees are created
// through it, so the user's own gtr.copy.include patterns and postCreate
// hooks apply to orchestra's worktrees too. orchestra never writes gtr
// config: what it needs is lent to one subprocess through GIT_CONFIG_COUNT.

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

// createViaGtr returns the new worktree's path, or "" when gtr refused.
func createViaGtr(root, branch string, pairs [][2]string) string {
	env := gitx.ConfigEnv(pairs, nil)
	if _, err := gitx.RunEnv(root, env, "gtr", "new", branch, "--from-current", "--yes", "--no-fetch"); err != nil {
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
	return path
}
