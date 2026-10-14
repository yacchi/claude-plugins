package config

// FoldHost lowercases ASCII letters only and leaves every other rune
// untouched. Every domain/host/allowlist comparison folds both sides with
// it. Unicode-aware lowering (strings.ToLower) would widen matches: "İ"
// would become "i̇" or "i" and the Kelvin sign "K" would become "k", so a
// non-ASCII host could match an ASCII allowlist entry it must not match.
func FoldHost(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// PyLower is a temporary alias for FoldHost, kept only so packages owned by
// other workers (host/*, cli) keep compiling until they are renamed.
func PyLower(s string) string { return FoldHost(s) }
