package hookrun

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
)

// SessionEnvMarker is the first line of orchestra's block. Claude Code runs
// the SessionStart hook's CLAUDE_ENV_FILE as shell code in front of every
// Bash command, so the block carries the environment that used to be
// injected by rewriting each command; the marker keeps a resumed, cleared or
// compacted session from appending the block a second time.
const SessionEnvMarker = "# orchestra session env"

// SessionEnvScript is orchestra's block: SessionEnvMarker, then
// config.NodeCapShellPrefix(testSlots) on its own line when it is not "",
// then the tree-env line, then the line `true`; every line ends in "\n".
// The last line is `true` on purpose: the file is chained with `&&` to the
// command, so a failing line above must not skip the command.
func SessionEnvScript(exe string, testSlots int) string {
	var b strings.Builder
	b.WriteString(SessionEnvMarker + "\n")
	if p := config.NodeCapShellPrefix(testSlots); p != "" {
		b.WriteString(p + "\n")
	}
	b.WriteString("if [ -f .git ] && [ -z \"${pnpm_config_enable_global_virtual_store:-}\" ]; then eval \"$(" + sessionEnvQuote(exe) + " hook tree-env 2>/dev/null)\"; fi\n")
	b.WriteString("true\n")
	return b.String()
}

// sessionEnvQuote is s as one always-quoted POSIX shell word, so an exe path
// with spaces or quotes stays one word. config.shellQuote is unexported and
// the sandbox quoter is outside hookrun's package graph, hence this copy.
func sessionEnvQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// WriteSessionEnv appends SessionEnvScript(exe, testSlots) to path (created
// 0644 when missing) unless the file already contains SessionEnvMarker. An
// empty or relative path writes nothing and returns nil. The file is only
// ever appended to: other hooks write their own lines into it.
func WriteSessionEnv(path, exe string, testSlots int) error {
	if path == "" || !filepath.IsAbs(path) {
		return nil
	}
	have, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if bytes.Contains(have, []byte(SessionEnvMarker)) {
		return nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	var out strings.Builder
	if len(have) > 0 && !bytes.HasSuffix(have, []byte("\n")) {
		// Another hook's unterminated last line would otherwise swallow the marker.
		out.WriteString("\n")
	}
	out.WriteString(SessionEnvScript(exe, testSlots))
	_, werr := f.WriteString(out.String())
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}
