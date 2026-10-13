package telemetry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

const (
	stub = "# orchestra config\n# telemetry is opt-in and anonymized\n"
	tmpl = "telemetry:\n  enabled: %s  # opt-in, anonymized (see orchestra:run SKILL §10)\n"
)

func edit(text string, enabled bool) string { return SetEnabledInText(&text, enabled) }

// TestSetEnabledInTextBytes pins the exact bytes of the line edit.
func TestSetEnabledInTextBytes(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		enabled bool
		want    string
	}{
		{"enabled value flips in place", "telemetry:\n  enabled: false\n", true, "telemetry:\n  enabled: true\n"},
		{"trailing comment kept", "telemetry:\n  enabled: false   # keep\n", true, "telemetry:\n  enabled: true   # keep\n"},
		{"space before colon", "telemetry:\n  enabled : false\n", true, "telemetry:\n  enabled : true\n"},
		{"quoted string value", "telemetry:\n  enabled: 'false'\n", true, "telemetry:\n  enabled: true\n"},
		{"yaml 1.1 word", "telemetry:\n  enabled: no\n", true, "telemetry:\n  enabled: true\n"},
		{"capitalized", "telemetry:\n  enabled: False\n", true, "telemetry:\n  enabled: true\n"},
		{"no space after colon", "telemetry:\n  enabled:true\n", false, "telemetry:\n  enabled:false\n"},
		{"tab indent", "telemetry:\n\tenabled: true\n", false, "telemetry:\n\tenabled: false\n"},
		{"odd indent", "telemetry:\n   enabled: false\n", true, "telemetry:\n   enabled: true\n"},
		{"no trailing newline", "telemetry:\n  enabled: false", true, "telemetry:\n  enabled: true"},
		{"CRLF preserved", "a: 1\r\ntelemetry:\r\n  enabled: false\r\nb: 2\r\n", true, "a: 1\r\ntelemetry:\r\n  enabled: true\r\nb: 2\r\n"},
		{"lone CR preserved", "telemetry:\r  enabled: false\rb: 2\r", true, "telemetry:\r  enabled: true\rb: 2\r"},
		{"anchor with comment", "telemetry: # c\n  enabled: false\n", true, "telemetry: # c\n  enabled: true\n"},
		{"anchor with trailing spaces", "telemetry:   \n  enabled: false\n", true, "telemetry:   \n  enabled: true\n"},
		{"insert uses the block indent", "telemetry:\n    dir: x\n", true, "telemetry:\n    enabled: true\n    dir: x\n"},
		{"insert uses the tab indent", "telemetry:\n\tdir: x\n", false, "telemetry:\n\tenabled: false\n\tdir: x\n"},
		{"insert skips blank and finds the first child", "telemetry:\n\n  dir: x\n", true, "telemetry:\n  enabled: true\n\n  dir: x\n"},
		{"insert into an empty block", "telemetry:\nother: 1\n", true, "telemetry:\n  enabled: true\nother: 1\n"},
		{"insert after an anchor without newline", "telemetry:", true, "telemetry:\n  enabled: true\n"},
		{"next top-level key ends the block", "telemetry:\n  dir: x\ntiers:\n  enabled: false\n", true, "telemetry:\n  enabled: true\n  dir: x\ntiers:\n  enabled: false\n"},
		{"inline flow mapping is not the anchor", "telemetry: {enabled: false}\n", true, "telemetry: {enabled: false}\n\n" + strings.Replace(tmpl, "%s", "true", 1)},
		{"commented anchor", "# telemetry:\nother: 1\n", true, "# telemetry:\nother: 1\n\n" + strings.Replace(tmpl, "%s", "true", 1)},
		{"nested anchor", "foo:\n  telemetry:\n    enabled: false\n", true, "foo:\n  telemetry:\n    enabled: false\n\n" + strings.Replace(tmpl, "%s", "true", 1)},
		{"missing newline before append", "tiers: {}", false, "tiers: {}\n\n" + strings.Replace(tmpl, "%s", "false", 1)},
		{"empty file", "", true, "\n" + strings.Replace(tmpl, "%s", "true", 1)},
		{"whitespace only", "   \n\t\n", false, "   \n\t\n\n" + strings.Replace(tmpl, "%s", "false", 1)},
		{"first anchor wins", "telemetry:\n  enabled: false\ntelemetry:\n  enabled: false\n", true, "telemetry:\n  enabled: true\ntelemetry:\n  enabled: false\n"},
		{"unicode separator ends a line", "telemetry:   enabled: false b: 2", true, "telemetry:   enabled: true b: 2"},
		{"NEL ends a line", "a: 1\u0085telemetry:\n  enabled: false\n", true, "a: 1\u0085telemetry:\n  enabled: true\n"},
		{"unicode space after colon is whitespace", "telemetry: \n  enabled: false\n", true, "telemetry: \n  enabled: true\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := edit(c.in, c.enabled); got != c.want {
				t.Errorf("got  %q\nwant %q", got, c.want)
			}
		})
	}
}

func TestSetEnabledInTextStubForMissingFile(t *testing.T) {
	want := stub + "\n" + strings.Replace(tmpl, "%s", "true", 1)
	if got := SetEnabledInText(nil, true); got != want {
		t.Errorf("got %q", got)
	}
	var doc map[string]map[string]any
	if err := yaml.Unmarshal([]byte(SetEnabledInText(nil, false)), &doc); err != nil || doc["telemetry"]["enabled"] != false {
		t.Errorf("stub is not valid YAML: %v %v", doc, err)
	}
}

func TestSplitLinesKeepEndsMatchesPython(t *testing.T) {
	cases := map[string][]string{
		"":                      nil,
		"a":                     {"a"},
		"a\n":                   {"a\n"},
		"a\r\nb":                {"a\r\n", "b"},
		"a\rb\n":                {"a\r", "b\n"},
		"a\n\nb":                {"a\n", "\n", "b"},
		"a\vb\fc":               {"a\v", "b\f", "c"},
		"a\x1cb\x1dc\x1ed\x1fe": {"a\x1c", "b\x1d", "c\x1e", "d\x1fe"},
		"a\u0085b c d":          {"a\u0085", "b ", "c ", "d"},
		"\r\r\n\n":              {"\r", "\r\n", "\n"},
	}
	for in, want := range cases {
		got := splitLinesKeepEnds(in)
		if strings.Join(got, "|") != strings.Join(want, "|") || len(got) != len(want) {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

// The shipped example config survives an enable byte for byte outside the
// telemetry block and still parses with the same meaning.
func TestRealisticConfigPreserved(t *testing.T) {
	data, err := os.ReadFile("../../../../examples/orchestra.yaml")
	if err != nil {
		t.Skip("example config not found")
	}
	original := string(data)
	text := original
	if !strings.Contains("\n"+text, "\ntelemetry:\n") {
		text += "\ntelemetry:\n  enabled: false\n"
	}
	out := edit(text, true)

	outLines := map[string]bool{}
	for _, l := range splitLinesKeepEnds(out) {
		outLines[l] = true
	}
	inBlock := false
	for _, line := range splitLinesKeepEnds(original) {
		stripped := strings.TrimRight(line, "\r\n")
		if stripped == "telemetry:" {
			inBlock = true
			continue
		}
		if inBlock {
			if strings.TrimSpace(stripped) == "" || stripped[0] == ' ' || stripped[0] == '\t' {
				continue
			}
			inBlock = false
		}
		if !outLines[line] {
			t.Fatalf("original line lost or modified: %q", line)
		}
	}
	var before, after map[string]any
	if err := yaml.Unmarshal(data, &before); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte(out), &after); err != nil {
		t.Fatal(err)
	}
	if after["telemetry"].(map[string]any)["enabled"] != true {
		t.Errorf("telemetry.enabled = %v", after["telemetry"])
	}
	for _, k := range []string{"tiers", "external_executors", "priority"} {
		if (before[k] == nil) != (after[k] == nil) {
			t.Errorf("%s changed", k)
		}
	}
}

func TestEnableDisableCyclesNeverDuplicate(t *testing.T) {
	text := "tiers:\n  light: []\ntelemetry:\n  enabled: false\nother: 1\n"
	for i := 0; i < 5; i++ {
		text = edit(text, i%2 == 0)
	}
	if n := strings.Count(text, "\ntelemetry:\n"); n != 1 {
		t.Errorf("telemetry blocks: %d\n%s", n, text)
	}
	if n := strings.Count(text, "enabled:"); n != 1 {
		t.Errorf("enabled keys: %d\n%s", n, text)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil || doc["telemetry"].(map[string]any)["enabled"] != true {
		t.Errorf("%v %v", doc, err)
	}
}

func TestToggleIsIdempotent(t *testing.T) {
	once := edit("telemetry:\n  enabled: false\n", true)
	if twice := edit(once, true); twice != once {
		t.Errorf("not idempotent: %q vs %q", once, twice)
	}
	stubbed := edit("", true)
	if strings.Count(edit(stubbed, true), "telemetry:") != 1 {
		t.Error("a stub toggled again grew a second block")
	}
}

// FuzzSetEnabledInText: the edit never panics, always mentions the value, and
// re-applying it to its own output is stable.
func FuzzSetEnabledInText(f *testing.F) {
	for _, seed := range []string{
		"", "telemetry:\n  enabled: false\n", "a: 1\r\ntelemetry:\r\n  enabled: no\r\n",
		"telemetry:\n\tdir: x", "# telemetry:\nx: 1", "telemetry: # c\n    enabled : 'false' # k\nz: 1\n",
		"telemetry:   enabled: true ", "\x00\xff telemetry:\n",
	} {
		f.Add(seed, true)
	}
	f.Fuzz(func(t *testing.T, text string, enabled bool) {
		if !utf8.ValidString(text) {
			return
		}
		out := SetEnabledInText(&text, enabled)
		again := SetEnabledInText(&out, enabled)
		if again != out {
			t.Fatalf("not stable:\n in   %q\n out  %q\n then %q", text, out, again)
		}
		if !strings.Contains(out, boolText(enabled)) {
			t.Fatalf("value missing: %q -> %q", text, out)
		}
	})
}

func TestToggleWritesFilesLikePythonsTextMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	work := t.TempDir()
	t.Chdir(work)

	path, err := Toggle("user", true)
	if err != nil || path != filepath.Join(home, ".claude", "orchestra.yaml") {
		t.Fatalf("Toggle = %q, %v", path, err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != stub+"\n"+strings.Replace(tmpl, "%s", "true", 1) {
		t.Errorf("new file = %q", got)
	}

	// An existing .yml is edited only while the .yaml is absent; CRLF and a
	// lone CR are read as LF.
	if err := os.MkdirAll(".claude", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".claude/orchestra.yml", []byte("a: 1\r\ntelemetry:\r  enabled: false\r"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, err = Toggle("project", true)
	if err != nil || !strings.HasSuffix(path, ".claude/orchestra.yml") || !filepath.IsAbs(path) {
		t.Fatalf("Toggle = %q, %v", path, err)
	}
	if got, _ := os.ReadFile(path); string(got) != "a: 1\ntelemetry:\n  enabled: true\n" {
		t.Errorf("edited = %q", got)
	}
	if err := os.WriteFile(".claude/orchestra.yaml", []byte("x: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if path, _ = Toggle("project", false); !strings.HasSuffix(path, "orchestra.yaml") {
		t.Errorf("both exist -> %q", path)
	}
	if _, err := Toggle("local", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(".claude/orchestra.local.yaml"); err != nil {
		t.Error("local file not created")
	}
	if _, err := Toggle("global", true); err == nil {
		t.Error("an unknown scope was accepted")
	}
}
