package pilogin

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
)

var (
	labelSplitRE  = regexp.MustCompile(`\s+[•·]\s+`)
	labelSuffixRE = regexp.MustCompile(`(?i)\s*\((legacy|default)\)`)
	highlightRE   = regexp.MustCompile(`^\s*[→❯]\s+(\S.*?)\s*$`)
	codeRE        = regexp.MustCompile(`Enter code:\s*([A-Za-z0-9][A-Za-z0-9-]*)`)
)

// stopWords end a wrapped URL.
var stopWords = []string{"cmd+click"}

// splitLines splits on \n, \r\n and \r, dropping the final empty piece (a
// terminal screen carries no other separators).
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// ScreenText is the pane text of a `pane read`: its raw stdout, or the text
// of a JSON payload when herdr wraps it.
func ScreenText(out string, payload *jsonx.Object) string {
	if result := payload.Object("result"); result != nil {
		if text, ok := result.Get("text"); ok {
			if s, isStr := text.(string); isStr {
				return s
			}
		}
	}
	return out
}

// Tail is the last lines of a screen, right-trimmed and stripped.
func Tail(text string, lines int) string {
	all := splitLines(text)
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	for i, l := range all {
		all[i] = strings.TrimRightFunc(l, unicode.IsSpace)
	}
	return strings.TrimSpace(strings.Join(all, "\n"))
}

func normLabel(label string) string {
	if loc := labelSplitRE.FindStringIndex(label); loc != nil {
		label = label[:loc[0]]
	}
	label = labelSuffixRE.ReplaceAllString(label, "")
	return strings.TrimSpace(label)
}

// Highlighted is the label of the selected entry (the line marked with an
// arrow) and whether there is one. Annotations like `• not configured` and
// `(legacy)` are dropped.
func Highlighted(screen string) (string, bool) {
	for _, line := range splitLines(screen) {
		if m := highlightRE.FindStringSubmatch(line); m != nil {
			return normLabel(m[1]), true
		}
	}
	return "", false
}

// ExtractURL is the login URL: from the line starting with https:// through
// the hard-wrapped continuation lines (each with a leading space), joined
// with no separator, stopping before the `Cmd+click` line, a blank line, or
// any line with whitespace inside.
func ExtractURL(screen string) (string, bool) {
	lines := splitLines(screen)
	for i, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), "https://") {
			continue
		}
		parts := []string{strings.TrimSpace(line)}
	next:
		for _, nxt := range lines[i+1:] {
			piece := strings.TrimSpace(nxt)
			if piece == "" || strings.IndexFunc(piece, unicode.IsSpace) >= 0 {
				break
			}
			for _, w := range stopWords {
				if strings.Contains(strings.ToLower(piece), w) {
					break next
				}
			}
			parts = append(parts, piece)
		}
		return strings.Join(parts, ""), true
	}
	return "", false
}

// ExtractCode is the device code after `Enter code:`.
func ExtractCode(screen string) (string, bool) {
	if m := codeRE.FindStringSubmatch(screen); m != nil {
		return m[1], true
	}
	return "", false
}

// WantsPaste is whether pi asks for the redirect URL or code to be pasted.
func WantsPaste(screen string) bool {
	lower := strings.ToLower(screen)
	return strings.Contains(lower, "paste") &&
		(strings.Contains(lower, "redirect url") || strings.Contains(lower, "authorization code"))
}
