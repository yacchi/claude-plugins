package config

import (
	"strings"
	"unicode"
)

// PyLower is Python's str.lower(): the full Unicode lowercase mapping, not
// Go's simple one. The two differ where it matters for allowlists: U+0130
// (İ) lowers to "i̇" in Python but to plain "i" in Go, so with
// strings.ToLower `İ.com` would become the valid host `i.com` that Python
// refuses. Capital sigma lowers to final sigma (ς) at the end of a word,
// as Python does. Every domain/allowlist normalization must use this.
func PyLower(s string) string {
	rs := []rune(s)
	var b strings.Builder
	b.Grow(len(s))
	for i, r := range rs {
		switch {
		case r < 0x80:
			if 'A' <= r && r <= 'Z' {
				r += 'a' - 'A'
			}
			b.WriteRune(r)
		case r == 'İ':
			b.WriteString("i̇")
		case r == 'Σ':
			if finalSigma(rs, i) {
				b.WriteRune('ς')
			} else {
				b.WriteRune('σ')
			}
		default:
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// finalSigma is Unicode's Final_Sigma condition (as CPython applies it): a
// cased letter before (skipping case-ignorables) and none after.
func finalSigma(rs []rune, i int) bool {
	j := i - 1
	for j >= 0 && caseIgnorable(rs[j]) {
		j--
	}
	if j < 0 || !cased(rs[j]) {
		return false
	}
	k := i + 1
	for k < len(rs) && caseIgnorable(rs[k]) {
		k++
	}
	return k == len(rs) || !cased(rs[k])
}

func cased(r rune) bool {
	return unicode.IsUpper(r) || unicode.IsLower(r) || unicode.IsTitle(r)
}

func caseIgnorable(r rune) bool {
	switch r {
	case '\'', '.', ':', '^', '`', '·', '‘', '’', '․', '﹒', '＇', '．', '：':
		return true
	}
	return unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk)
}
