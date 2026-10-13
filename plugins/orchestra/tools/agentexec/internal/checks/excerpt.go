// Package checks runs the config-declared lint/test commands of `agent-exec
// check` over a tree's changed files. It knows nothing about git: the caller
// resolves the tree, the changed-file list and any baseline worktree.
package checks

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Python's \s and \w are Unicode-aware for str patterns; RE2's are ASCII.
const (
	pyS    = `[\t-\r\x1c-\x1f \x{85}\p{Z}]`
	pyNotS = `[^\t-\r\x1c-\x1f \x{85}\p{Z}]`
	wordCS = `\pL\pN_` // the inside of a character class
)

var (
	ansiRE   = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)
	markupRE = regexp.MustCompile(`^` + pyS + `*(?:</?[A-Za-z][` + wordCS + `:-]*(?:` + pyS + `|/?>|$)|/?>$|[` +
		wordCS + `:-]+="[^"]*"` + pyS + `*/?>?$)`)

	failedFileRE = []*regexp.Regexp{
		regexp.MustCompile(`^` + pyS + `*FAIL` + pyS + `+(.+?)(?:` + pyS + `+>` + pyS + `+|` + pyS + `+|$)`),
		regexp.MustCompile(`^` + pyS + `*FAILED` + pyS + `+(` + pyNotS + `+?)(?:::` + pyNotS + `+)(?:` + pyS + `|$)`),
	}
)

const (
	markupRun    = 4
	contextAfter = 3
	tailLines    = 30
)

// anchor is one alternative of the failure-anchor pattern. RE2 has no \b that
// understands Unicode words, so the boundaries are checked by hand.
type anchor struct {
	re          *regexp.Regexp
	left, right bool
}

var anchors = []anchor{
	{regexp.MustCompile(`FAIL(?:ED)?`), true, true},
	{regexp.MustCompile(`[×✖✗]`), false, false},
	{regexp.MustCompile(`error TS\d+`), true, false},
	{regexp.MustCompile(`error\[`), true, false},
	{regexp.MustCompile(`Error:`), true, false},
	{regexp.MustCompile(`AssertionError`), true, true},
	{regexp.MustCompile(`panicked at`), false, false},
	{regexp.MustCompile(`not ok`), true, true},
	{regexp.MustCompile(`^Traceback`), false, false},
	{regexp.MustCompile(`^` + pyS + `*Expected`), false, true},
	{regexp.MustCompile(`^` + pyS + `*Received`), false, true},
	{regexp.MustCompile(`Timed out|timed out`), false, false},
}

func isWord(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) }

func (a anchor) matches(line string) bool {
	for off := 0; off <= len(line); {
		loc := a.re.FindStringIndex(line[off:])
		if loc == nil {
			return false
		}
		start, end := off+loc[0], off+loc[1]
		ok := true
		if a.left && start > 0 {
			r, _ := utf8.DecodeLastRuneInString(line[:start])
			ok = !isWord(r)
		}
		if ok && a.right && end < len(line) {
			r, _ := utf8.DecodeRuneInString(line[end:])
			ok = !isWord(r)
		}
		if ok {
			return true
		}
		_, w := utf8.DecodeRuneInString(line[start:])
		if w == 0 {
			return false
		}
		off = start + w
	}
	return false
}

func isAnchor(line string) bool {
	for _, a := range anchors {
		if a.matches(line) {
			return true
		}
	}
	return false
}

// pySplitLines is Python's str.splitlines().
func pySplitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); {
		r, w := utf8.DecodeRuneInString(s[i:])
		switch r {
		case '\n', '\v', '\f', '\x1c', '\x1d', '\x1e', '\u0085', ' ', ' ':
			out = append(out, s[start:i])
			i += w
			start = i
		case '\r':
			out = append(out, s[start:i])
			i += w
			if i < len(s) && s[i] == '\n' {
				i++
			}
			start = i
		default:
			i += w
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func collapseMarkup(lines []string) []string {
	var out, run []string
	flush := func() {
		if len(run) < markupRun {
			out = append(out, run...)
		} else {
			out = append(out, "  … ("+strconv.Itoa(len(run))+" markup lines omitted)")
		}
		run = nil
	}
	for _, line := range lines {
		if markupRE.MatchString(line) {
			run = append(run, line)
			continue
		}
		flush()
		out = append(out, line)
	}
	flush()
	return out
}

// clip shortens text to limit code points the way Python's _clip does,
// including measuring the marker with the full length rather than the
// omitted count.
func clip(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	marker := func(n int) string { return "\n… (" + strconv.Itoa(n) + " chars omitted) …\n" }
	keep := max(limit-utf8.RuneCountInString(marker(len(runes))), 0)
	head := keep * 2 / 3
	tail := keep - head
	out := string(runes[:head]) + marker(len(runes)-keep)
	if tail > 0 {
		out += string(runes[len(runes)-tail:])
	}
	return out
}

// FailureExcerpt is the part of a failed command's output worth handing to a
// fixer: failure anchors with a few lines of context first, then the tail,
// at most 4000 characters.
func FailureExcerpt(text string) string { return excerptLimit(text, 4000) }

func excerptLimit(text string, limit int) string {
	if text == "" {
		return ""
	}
	lines := collapseMarkup(pySplitLines(ansiRE.ReplaceAllString(text, "")))
	var picked []int
	seen := map[int]bool{}
	budget := limit * 2 / 3
	used := 0
	for i, line := range lines {
		if !isAnchor(line) {
			continue
		}
		for j := i; j < min(i+1+contextAfter, len(lines)); j++ {
			if seen[j] {
				continue
			}
			seen[j] = true
			picked = append(picked, j)
			used += utf8.RuneCountInString(lines[j]) + 1
		}
		if used >= budget {
			break
		}
	}
	var tail []string
	for j := max(len(lines)-tailLines, 0); j < len(lines); j++ {
		if !seen[j] {
			tail = append(tail, lines[j])
		}
	}
	var body string
	if len(picked) > 0 {
		parts := make([]string, len(picked))
		for k, j := range picked {
			parts[k] = lines[j]
		}
		body = strings.Join(parts, "\n")
		if len(tail) > 0 {
			body += "\n--- tail ---\n" + strings.Join(tail, "\n")
		}
	} else {
		body = strings.Join(lines[max(len(lines)-tailLines*2, 0):], "\n")
	}
	return clip(body, limit)
}

// ParseFailedFiles returns the ordered, unique test files named by common
// test runners ("FAIL path", "FAILED path::test").
func ParseFailedFiles(output string) []string {
	var found []string
	seen := map[string]bool{}
	for _, line := range pySplitLines(ansiRE.ReplaceAllString(output, "")) {
		var m []string
		for _, re := range failedFileRE {
			if m = re.FindStringSubmatch(line); m != nil {
				break
			}
		}
		if m == nil {
			continue
		}
		path := strings.TrimSpace(m[1])
		if !seen[path] {
			seen[path] = true
			found = append(found, path)
		}
	}
	return found
}
