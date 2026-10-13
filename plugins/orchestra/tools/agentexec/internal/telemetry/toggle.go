package telemetry

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"unicode/utf8"
)

// ws is Python's \s for str patterns (str.isspace): RE2's own \s is ASCII
// only, and the line edit must treat the same characters as whitespace.
const ws = `\t\n\v\f\r\x1c-\x1f \x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`

var (
	topLevelRE     = regexp.MustCompile(`^telemetry:[` + ws + `]*(#.*)?$`)
	enabledChildRE = regexp.MustCompile(`^([` + ws + `]+enabled[` + ws + `]*:[` + ws + `]*)([^` + ws + `]+)(.*)$`)
	leadingWSRE    = regexp.MustCompile(`^([` + ws + `]+)`)
)

const (
	stubComment   = "# orchestra config\n# telemetry is opt-in and anonymized\n"
	blockTemplate = "telemetry:\n  enabled: %s  # opt-in, anonymized (see orchestra:run SKILL §10)\n"
)

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func block(enabled bool) string {
	return strings.Replace(blockTemplate, "%s", boolText(enabled), 1)
}

func isPySpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x1c, 0x1d, 0x1e, 0x1f, 0x85, 0xa0,
		0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// pyStrip is Python's str.strip().
func pyStrip(s string) string { return strings.TrimFunc(s, isPySpace) }

// splitLinesKeepEnds is Python's str.splitlines(keepends=True): it breaks at
// \n, \r, \r\n, \v, \f, \x1c-\x1e, \x85,   and  .
func splitLinesKeepEnds(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch r {
		case '\r':
			end := i + 1
			if end < len(s) && s[end] == '\n' {
				end++
			}
			lines = append(lines, s[start:end])
			start, i = end, end
		case '\n', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			lines = append(lines, s[start:i+size])
			start, i = i+size, i+size
		default:
			i += size
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// SetEnabledInText returns the content of an orchestra.yaml layer file (nil
// when the file does not exist yet) with telemetry.enabled surgically set.
// It is a LINE edit, not a YAML round trip: every other line, comment, key
// and line ending is preserved byte for byte. Only a zero-indent
// `telemetry:` key anchors the block; the same text under another key or in
// a comment is ignored.
func SetEnabledInText(text *string, enabled bool) string {
	b := boolText(enabled)
	if text == nil {
		return stubComment + "\n" + block(enabled)
	}
	lines := splitLinesKeepEnds(*text)

	telemetryIdx := -1
	for i, line := range lines {
		if topLevelRE.MatchString(strings.TrimRight(line, "\r\n")) {
			telemetryIdx = i
			break
		}
	}
	if telemetryIdx < 0 {
		out := *text
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		return out + "\n" + block(enabled)
	}

	// The block runs from just after the telemetry: line until the next
	// zero-indent, non-blank line (or EOF).
	endIdx := len(lines)
	for j := telemetryIdx + 1; j < len(lines); j++ {
		raw := strings.TrimRight(lines[j], "\r\n")
		if pyStrip(raw) == "" {
			continue
		}
		if raw[0] != ' ' && raw[0] != '\t' {
			endIdx = j
			break
		}
	}

	for j := telemetryIdx + 1; j < endIdx; j++ {
		if !enabledChildRE.MatchString(strings.TrimRight(lines[j], "\r\n")) {
			continue
		}
		line := lines[j]
		body, eol := line, ""
		switch {
		case strings.HasSuffix(line, "\r\n"):
			body, eol = line[:len(line)-2], "\r\n"
		case strings.HasSuffix(line, "\n"):
			body, eol = line[:len(line)-1], "\n"
		}
		m := enabledChildRE.FindStringSubmatch(body)
		lines[j] = m[1] + b + m[3] + eol
		return strings.Join(lines, "")
	}

	childIndent := "  "
	for j := telemetryIdx + 1; j < endIdx; j++ {
		raw := strings.TrimRight(lines[j], "\r\n")
		if pyStrip(raw) == "" {
			continue
		}
		if m := leadingWSRE.FindStringSubmatch(raw); m != nil {
			childIndent = m[1]
		}
		break
	}
	insert := childIndent + "enabled: " + b + "\n"
	lines = append(lines[:telemetryIdx+1], append([]string{insert}, lines[telemetryIdx+1:]...)...)
	return strings.Join(lines, "")
}

// ScopeTarget is the file `telemetry enable|disable --scope` edits: the
// .yaml variant, or an existing .yml only when the .yaml is absent. When
// neither exists it is the .yaml path (a new file is created there). The
// user scope lives under $HOME/.claude, project and local under ./.claude.
func ScopeTarget(scope string) (string, bool) {
	var dir, yamlName, ymlName string
	switch scope {
	case "user":
		dir = filepath.Join(expandHome("~"), ".claude")
		yamlName, ymlName = "orchestra.yaml", "orchestra.yml"
	case "project":
		dir = filepath.Join(".", ".claude")
		yamlName, ymlName = "orchestra.yaml", "orchestra.yml"
	case "local":
		dir = filepath.Join(".", ".claude")
		yamlName, ymlName = "orchestra.local.yaml", "orchestra.local.yml"
	default:
		return "", false
	}
	yamlPath, ymlPath := filepath.Join(dir, yamlName), filepath.Join(dir, ymlName)
	if isFile(yamlPath) {
		return yamlPath, true
	}
	if isFile(ymlPath) {
		return ymlPath, true
	}
	return yamlPath, true
}

func expandHome(p string) string {
	if h := os.Getenv("HOME"); h != "" {
		return h + p[1:]
	}
	if h, err := os.UserHomeDir(); err == nil {
		return h + p[1:]
	}
	return p
}

func isFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

// Toggle sets telemetry.enabled in the scope's config file, creating it (and
// its directory) from a template when absent, and returns the absolute path
// edited. Like Python's text-mode open, CRLF and lone CR line endings of an
// existing file are read as LF, so the file is rewritten with LF endings.
func Toggle(scope string, enabled bool) (string, error) {
	path, ok := ScopeTarget(scope)
	if !ok {
		return "", os.ErrInvalid
	}
	var text *string
	if isFile(path) {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		if !utf8.Valid(data) {
			return "", &os.PathError{Op: "read", Path: path, Err: os.ErrInvalid}
		}
		s := strings.ReplaceAll(string(data), "\r\n", "\n")
		s = strings.ReplaceAll(s, "\r", "\n")
		text = &s
	} else if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return "", err
	}
	out := SetEnabledInText(text, enabled)
	if err := os.WriteFile(path, []byte(out), 0o666); err != nil {
		return "", err
	}
	return AbsPath(path), nil
}

// AbsPath is os.path.abspath: the kernel's cwd (never $PWD) joined and
// cleaned.
func AbsPath(p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	wd, err := syscall.Getwd()
	if err != nil {
		return filepath.Clean(p)
	}
	return filepath.Join(wd, p)
}
