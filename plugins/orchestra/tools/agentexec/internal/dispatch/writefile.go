package dispatch

import (
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
)

// NoExpect is WriteFile's expectChars when no count was given (any other
// value, negative ones included, must equal the count).
const NoExpect = math.MinInt

// WriteFile writes all of in, byte for byte, atomically to path (missing
// parent directories are created) and reports the absolute path and the
// content's code-point count without one trailing newline. With an
// expected count that differs nothing is written: `mismatch`, exit 1.
//
// A failure (input that is not UTF-8, an uncreatable directory, a failed
// write) is exit 2 with {"status": "error", "path", "error"}: a diagnostic
// for stderr, not a result to print.
func WriteFile(path string, expectChars int, in io.Reader) (map[string]any, int) {
	absPath := abs(path)
	fail := func(msg string) (map[string]any, int) {
		return map[string]any{"status": "error", "path": absPath, "error": msg}, 2
	}
	data, err := io.ReadAll(in)
	if err != nil {
		return fail(err.Error())
	}
	if !utf8.Valid(data) {
		return fail("stdin is not valid UTF-8")
	}
	chars := utf8.RuneCountInString(strings.TrimSuffix(string(data), "\n"))
	if expectChars != NoExpect && chars != expectChars {
		return map[string]any{
			"status":   "mismatch",
			"path":     absPath,
			"chars":    int64(chars),
			"expected": int64(expectChars),
		}, 1
	}
	if err := os.MkdirAll(filepath.Dir(absPath), 0o777); err != nil {
		return fail(err.Error())
	}
	if err := fsutil.AtomicWriteFile(absPath, data, 0o600); err != nil {
		return fail(err.Error())
	}
	return map[string]any{"status": "ok", "path": absPath, "chars": int64(chars)}, 0
}
