package checks

import (
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

var globCache sync.Map // pattern -> *regexp.Regexp

// globRegex translates one `paths` glob to an anchored regex. `**` as a
// whole segment means "zero or more whole directories"; `*` and `?` inside
// any other segment never cross `/`.
func globRegex(pattern string) *regexp.Regexp {
	if re, ok := globCache.Load(pattern); ok {
		return re.(*regexp.Regexp)
	}
	segments := strings.Split(pattern, "/")
	var b strings.Builder
	b.WriteString("^")
	hasParts := false
	prevDoubleStar := false
	for i, seg := range segments {
		if seg == "**" {
			if hasParts && !prevDoubleStar {
				b.WriteString("/")
			}
			if i == len(segments)-1 {
				b.WriteString(`(?:[^/]+/)*[^/]*`)
			} else {
				b.WriteString(`(?:[^/]+/)*`)
			}
			hasParts = true
			prevDoubleStar = true
			continue
		}
		if hasParts && !prevDoubleStar {
			b.WriteString("/")
		}
		for _, ch := range seg {
			switch ch {
			case '*':
				b.WriteString("[^/]*")
			case '?':
				b.WriteString("[^/]")
			default:
				b.WriteString(regexp.QuoteMeta(string(ch)))
			}
		}
		hasParts = true
		prevDoubleStar = false
	}
	b.WriteString(`$`)
	re := regexp.MustCompile(b.String())
	globCache.Store(pattern, re)
	return re
}

// GlobMatch reports whether the POSIX-relative path matches one `paths` glob.
func GlobMatch(pattern, path string) bool { return globRegex(pattern).MatchString(path) }

// MatchFiles returns the changed files that satisfy any pattern. An empty
// pattern list matches every file ("omitted = always runs").
func MatchFiles(patterns, files []string) []string {
	if len(patterns) == 0 {
		return append([]string(nil), files...)
	}
	var out []string
	for _, f := range files {
		for _, p := range patterns {
			if GlobMatch(p, f) {
				out = append(out, f)
				break
			}
		}
	}
	return out
}

// filesForCommand is the matched files that still exist, re-expressed
// relative to cwdDir; one outside cwdDir is dropped.
func filesForCommand(tree, cwdDir string, matched []string) []string {
	var out []string
	absCwd, err := filepath.Abs(cwdDir)
	if err != nil {
		return nil
	}
	for _, rel := range matched {
		p := rel
		if !filepath.IsAbs(rel) {
			p = filepath.Join(tree, rel)
		}
		if !isFile(p) {
			continue
		}
		absP, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		r, err := filepath.Rel(absCwd, absP)
		if err != nil || r == ".." || strings.HasPrefix(r, "../") {
			continue
		}
		out = append(out, r)
	}
	return out
}

// shellQuote is Python's shlex.quote.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("_@%+=:,./-", c) >= 0) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func quoteFiles(files []string) string {
	q := make([]string, len(files))
	for i, f := range files {
		q[i] = shellQuote(f)
	}
	return strings.Join(q, " ")
}
