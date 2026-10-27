package statusfile

import (
	"path/filepath"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// Project names the repository dir belongs to as owner/repo, from its origin
// remote. Without one it uses the main checkout's directory name, and failing
// that the directory's own name. The result is never empty.
func Project(dir string) string {
	if out, err := gitx.Run(dir, "remote", "get-url", "origin"); err == nil {
		if p := projectFromRemote(strings.TrimSpace(out)); p != "" {
			return p
		}
	}
	if out, err := gitx.Run(dir, "rev-parse", "--path-format=absolute", "--git-common-dir"); err == nil {
		if common := strings.TrimSpace(out); common != "" {
			return filepath.Base(filepath.Dir(common))
		}
	}
	return filepath.Base(paths.RealPath(dir))
}

// projectFromRemote returns the last two parts of a remote URL, such as
// "yacchi/claude-plugins" for git@github.com:yacchi/claude-plugins.git.
func projectFromRemote(url string) string {
	s := strings.TrimSpace(url)
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '/' || r == ':' })
	if len(parts) > 2 {
		parts = parts[len(parts)-2:]
	}
	return strings.Join(parts, "/")
}
