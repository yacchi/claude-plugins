package telemetry

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"unicode/utf8"
)

// ws is the whitespace YAML allows around the key and value.
const ws = ` \t`

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

// splitLinesKeepEnds splits s after every "\n", so "\r\n" endings stay on
// their line and joining the pieces restores s exactly.
func splitLinesKeepEnds(s string) []string {
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
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
		if strings.TrimSpace(raw) == "" {
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
		if strings.TrimSpace(raw) == "" {
			continue
		}
		if m := leadingWSRE.FindStringSubmatch(raw); m != nil {
			childIndent = m[1]
		}
		break
	}
	insert := childIndent + "enabled: " + b + "\n"
	// An anchor that is the last line and has no terminator would get the
	// child glued onto it ; end the anchor line first.
	if anchor := lines[telemetryIdx]; telemetryIdx == len(lines)-1 && !strings.HasSuffix(anchor, "\n") {
		insert = "\n" + insert
	}
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
// edited. Line endings of an existing file are preserved.
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
		s := string(data)
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

// AbsPath makes p absolute from the kernel's cwd (never $PWD) and cleans it.
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
