package host

import (
	"errors"
	"strings"
)

// ShellSplit is Python's shlex.split (POSIX mode, no comments): words
// split on whitespace, with single quotes, double quotes (where only \ " $
// ` and newline may be escaped) and backslash escapes.
func ShellSplit(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inWord := false
	r := []rune(s)
	for i := 0; i < len(r); i++ {
		c := r[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		case c == '\\':
			inWord = true
			if i+1 >= len(r) {
				return nil, errors.New("no escaped character")
			}
			i++
			cur.WriteRune(r[i])
		case c == '\'':
			inWord = true
			j := i + 1
			for j < len(r) && r[j] != '\'' {
				j++
			}
			if j >= len(r) {
				return nil, errors.New("no closing quotation")
			}
			cur.WriteString(string(r[i+1 : j]))
			i = j
		case c == '"':
			inWord = true
			j := i + 1
			for ; j < len(r) && r[j] != '"'; j++ {
				if r[j] == '\\' && j+1 < len(r) && strings.ContainsRune("\\\"$`\n", r[j+1]) {
					j++
				}
				cur.WriteRune(r[j])
			}
			if j >= len(r) {
				return nil, errors.New("no closing quotation")
			}
			i = j
		default:
			inWord = true
			cur.WriteRune(c)
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}
