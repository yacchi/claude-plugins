package config

import "testing"

func TestFoldHostIsASCIIOnly(t *testing.T) {
	cases := map[string]string{
		"GitHub.COM":   "github.com",
		"GİTHUB.com":   "gİthub.com",
		"\u212Aagi.io": "\u212Aagi.io", // Kelvin sign must not become "k"
		"ΑΣ":           "ΑΣ",
	}
	for in, want := range cases {
		if got := FoldHost(in); got != want {
			t.Errorf("FoldHost(%q) = %q, want %q", in, got, want)
		}
	}
	if FoldHost("GİTHUB.com") == FoldHost("github.com") {
		t.Error("non-ASCII host matches the ASCII allowlist entry")
	}
}
