package procx

import (
	"testing"
)

// A script reaches the shell byte for byte: backslashes (Windows paths),
// both quote kinds, newlines and a trailing backslash pair included.
func TestShellPassesTheScriptUnchanged(t *testing.T) {
	for _, want := range []string{
		`C:\Users\x`,
		`a\b "c\"d" 'e\f' \`,
		"line1\nline2\\",
		`plain words`,
	} {
		script := "cat <<'END_OF_INPUT'\n" + want + "\nEND_OF_INPUT"
		out, err := Shell(nil, script).Output()
		if err != nil {
			t.Fatalf("%q: %v", want, err)
		}
		if got := string(out); got != want+"\n" {
			t.Errorf("script %q printed %q, want %q", want, got, want+"\n")
		}
	}
}

// A script without blanks reaches the command line unquoted, and the MSYS
// runtime brace-expands and globs such an argument before sh sees it. The
// shell itself never brace-expands an assignment, so x keeps its braces only
// when the script arrived intact.
func TestShellUnquotedScriptIsNotExpanded(t *testing.T) {
	out, err := Shell(nil, "x=a{b,c};echo${IFS}$x").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); got != "a{b,c}\n" {
		t.Fatalf("printed %q, want %q", got, "a{b,c}\n")
	}
}
