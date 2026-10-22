package host

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
)

// The fixtures are registries the retired Python hosts wrote (recorded by a
// generator script removed at the v0.55.0 cutover; see git history): Go
// must read them and write them back byte for byte, so registries left by a
// pre-cutover run stay usable.
func TestRegistryFixturesRoundTrip(t *testing.T) {
	files, _ := filepath.Glob("testdata/registry/*.json")
	if len(files) < 5 {
		t.Fatalf("expected the registry fixtures, found %v", files)
	}
	for _, f := range files {
		want, _ := os.ReadFile(f)
		got, err := EncodeRegistry(LoadRegistryFile(f))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: round trip differs:\n%s\nwant:\n%s", f, got, want)
		}
		// through Session too: every key survives
		data := LoadRegistryFile(f)
		for pkg, v := range data {
			rec, ok := v.(map[string]any)
			if !ok || pkg == "_warm" {
				continue
			}
			data[pkg] = SessionFromRecord("herdr", rec).Record()
		}
		if got, _ := EncodeRegistry(data); !bytes.Equal(got, want) {
			t.Errorf("%s: session round trip differs:\n%s", f, got)
		}
	}
}

// A registry Go builds from scratch is the file earlier versions wrote for
// the same sessions.
func TestRegistryWrittenByGoMatchesFixture(t *testing.T) {
	dir := t.TempDir()
	reg := &Registry{FileName: "herdr-sessions.json"}
	session := func(pkg string, extra map[string]any) *Session {
		s := &Session{Pkg: pkg, Worktree: "/repo/.claude/worktrees/pkg-" + pkg, Extra: map[string]any{
			"branch": "orchestra/abcdef12/pkg-" + pkg, "workspace": nil, "pane": nil, "repo": "/repo",
			"model": "opus", "cls": "standard", "state_dir": "herdr-start", "created": true,
			"herdr_session": "orchestra-abcdef12", "add_dirs": []string{"/repo", "herdr-start"}, "net_mark": 0,
		}}
		for k, v := range extra {
			s.Extra[k] = v
		}
		return s
	}
	a := session("a", map[string]any{"workspace": "w1", "pane": "w1:p1",
		"warm_session": "1b4e28ba-2fa1-41d2-883f-0016d3cca427", "warm_worktree": "/repo/.claude/worktrees/warm-opus"})
	a.Terminal = "p-a"
	if err := reg.Save(dir, a); err != nil {
		t.Fatal(err)
	}
	b := session("b-ünï", map[string]any{"model": "sonnet", "cls": "light", "created": false,
		"net_mark": 3, "fork_first": true})
	if err := reg.Save(dir, b); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(reg.Path(dir))
	want, _ := os.ReadFile("testdata/registry/herdr-two.json")
	if !bytes.Equal(got, want) {
		t.Errorf("go-written registry differs:\n%s\nwant:\n%s", got, want)
	}
	// floats survive a rewrite as numbers
	if err := reg.Update(dir, func(d map[string]any) bool {
		d["_warm"] = map[string]any{"x": map[string]any{"last_used": 100000.0, "created_at": 1e-05}}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	warm := config.ObjectAt(config.ObjectAt(reg.Load(dir), "_warm"), "x")
	num := func(v any) float64 { f, _ := strconv.ParseFloat(fmt.Sprint(v), 64); return f }
	if num(warm["last_used"]) != 100000 || num(warm["created_at"]) != 1e-05 {
		t.Errorf("floats not kept: %v", warm)
	}
	if err := reg.Forget(dir, "a"); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Load(dir)["a"]; ok {
		t.Error("forget left the record")
	}
	if s, ok := ReadSession(dir, "herdr-sessions.json", "herdr", "b-ünï"); !ok || s.Str("model") != "sonnet" || s.Terminal != "" {
		t.Errorf("ReadSession = %+v %v", s, ok)
	}
}

func TestSessionRecordAccessors(t *testing.T) {
	s := SessionFromRecord("orca", map[string]any{"pkg": "A", "worktree": "/w", "terminal": nil,
		"fresh": true, "add_dirs": []any{"/a", 3, "/b"}})
	if s.Pkg != "A" || s.Worktree != "/w" || s.Terminal != "" || s.Name != "orca" {
		t.Fatalf("%+v", s)
	}
	if !reflect.DeepEqual(s.Strings("add_dirs"), []string{"/a", "/b"}) {
		t.Error(s.Strings("add_dirs"))
	}
	if !s.Pop("fresh") || s.Pop("fresh") {
		t.Error("pop")
	}
	if rec := s.Record(); rec["terminal"] != nil || rec["pkg"] != "A" {
		t.Error(rec)
	}
}

func TestNormalizeEnabled(t *testing.T) {
	for v, want := range map[any]Tri{true: TriTrue, false: TriFalse, "false": TriFalse, "Never": TriFalse,
		"required": TriTrue, "auto": TriAuto, nil: TriAuto, "maybe": TriAuto, " ON ": TriTrue} {
		if got := NormalizeEnabled(v); got != want {
			t.Errorf("%v: %v want %v", v, got, want)
		}
	}
}

func resolved(t *testing.T, js string) *config.Resolved {
	t.Helper()
	v, err := fsutil.DecodeJSON([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	return &config.Resolved{Values: config.DeepMerge(config.Defaults(), v.(map[string]any))}
}

func TestMergeConfigKeepsDefaultsUnderNullAndMergesModels(t *testing.T) {
	c := MergeConfig(resolved(t, `{"orca": {"models": {"light": "haiku"}, "task_timeout": null, "enabled": "no"}}`), "orca")
	if c.Model("light") != "haiku" || c.Model("standard") != "opus" || c.Model("review") != "" {
		t.Error("models", c.Model("light"), c.Model("standard"))
	}
	if c.Settings().TaskTimeout != 3600*time.Second || c.Enabled != TriFalse {
		t.Error(c.Settings(), c.Enabled)
	}
	h := MergeConfig(resolved(t, `{"herdr": {"warm": {"min_packages": 5}}}`), "herdr")
	w := h.Sub("warm")
	if v, _ := w["min_packages"]; ConfigFloat(v, 0) != 5 {
		t.Error(v)
	}
	if v, _ := w["ttl_seconds"]; ConfigFloat(v, 0) != 3000 {
		t.Error("warm defaults lost", v)
	}
	if MergeConfig(nil, "herdr").Settings().StartupTimeout != 90*time.Second {
		t.Error("defaults")
	}
}

func TestIsolationFlags(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugin")
	_ = os.MkdirAll(filepath.Join(root, ".claude-plugin"), 0o755)
	_ = os.MkdirAll(filepath.Join(root, "tools"), 0o755)
	_ = os.WriteFile(filepath.Join(root, ".claude-plugin", "plugin.json"), []byte("{}"), 0o644)
	t.Setenv("AGENT_EXEC_BOOTSTRAP", filepath.Join(root, "tools", "agent-exec"))
	c := MergeConfig(nil, "orca")
	flags, err := IsolationFlags(c, []string{"claude"})
	if err != nil || !reflect.DeepEqual(flags, []string{"--strict-mcp-config", "--setting-sources", "project,local", "--plugin-dir", root}) {
		t.Errorf("%v %v", flags, err)
	}
	flags, _ = IsolationFlags(c, []string{"claude", "--strict-mcp-config", "--setting-sources=user", "--plugin-dir", "/p"})
	if len(flags) != 0 {
		t.Error(flags)
	}
	inherit := MergeConfig(resolved(t, `{"orca": {"user_config": "inherit"}}`), "orca")
	if flags, _ := IsolationFlags(inherit, []string{"claude"}); len(flags) != 0 {
		t.Error(flags)
	}
	bad := MergeConfig(resolved(t, `{"orca": {"user_config": "mine"}}`), "orca")
	if _, err := IsolationFlags(bad, nil); AsNeed(err).Kind != KindError || !strings.Contains(err.Error(), "invalid user_config: mine") {
		t.Error(err)
	}
}

var orcaTrustScreen = []string{
	" Accessing workspace:",
	" Quick safety check: Is this a project you created or one you trust? (Like your own code)",
	" ❯ No, exit",
	"   Yes, I trust this folder",
	" Enter to confirm · Esc to cancel",
}

func TestScreenClassifiers(t *testing.T) {
	if !IsTrust(orcaTrustScreen) || TrustYesSelected(orcaTrustScreen) {
		t.Error("trust")
	}
	if !TrustYesSelected([]string{"  ❯ Yes, I trust this folder"}) {
		t.Error("yes selected")
	}
	if !IsTrustText("Accessing workspace\nIs this a project you created or one you trust?") ||
		!IsTrustText("  2. Yes, I TRUST this folder") || IsTrustText("login required") {
		t.Error("trust text")
	}
	if !IsLoginText("  ⎿  Not logged in · Please run /login") || !IsLoginText("Invalid API key · Run /login") ||
		IsLoginText("fix the /login route") || IsLoginText("Not logged in") {
		t.Error("login text")
	}
	if !HasPromptBox([]string{"", "❯ "}) || IsDialog([]string{"", "❯ "}) {
		t.Error("prompt box")
	}
	if !IsDialog([]string{"❯ 1. Yes", "  2. No"}) || !IsDialog([]string{"❯ No, exit"}) {
		t.Error("dialog")
	}
	for _, screen := range [][]string{{"  No matches found for pattern"}, {"● Done", "  No issues found", "❯ "},
		{"  Yes, the file exists"}, {"Cancel the build if needed"}, {"Allow list updated"}} {
		if IsDialog(screen) {
			t.Errorf("not a dialog: %q", screen)
		}
	}
	screen := []string{"● Editing", "Do you want to make this edit to a.txt?", "❯ 1. Yes", "  2. No", "Esc to cancel"}
	if got := DialogLines(screen); !reflect.DeepEqual(got, screen[1:4]) {
		t.Errorf("dialog lines %q", got)
	}
}

func TestShellSplit(t *testing.T) {
	for in, want := range map[string][]string{
		`--model opus --permission-mode auto`: {"--model", "opus", "--permission-mode", "auto"},
		`a 'b c' "d \"e\" \$f \x" g\ h`:       {"a", "b c", `d "e" $f \x`, "g h"},
		``:                                    {},
	} {
		got, err := ShellSplit(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	if _, err := ShellSplit(`a "b`); err == nil {
		t.Error("unclosed quote")
	}
}

func TestPromptLineAndResultOutput(t *testing.T) {
	line := PromptLine([]string{"/p/one.md", "/p/two.md"}, "/s/a.result.json")
	want := `Read the worker prompt from /p/one.md and /p/two.md and carry it out. You are a worker: do it yourself in your working tree; do not load orchestra:run, call the Workflow tool, or run agent-exec dispatch or agent-exec wave. When you are completely done, ` +
		`write /s/a.result.json containing JSON {"status": "ok"|"escalate", "summary": "<=15 lines"}. ` +
		`If you must ESCALATE, put the reason in summary.`
	if line != want {
		t.Errorf("%s", line)
	}
	s := &Session{Pkg: "a", Worktree: "/w"}
	out := ResultOutput("herdr", s, "p-a", true, map[string]any{"status": "escalate", "summary": "why",
		"network_denials": []any{map[string]any{"host": "x", "port": 443}}})
	if out["answer"] != "ESCALATE: why" || out["session_id"] != "p-a" || out["resumed"] != true ||
		out["executor"] != "herdr" || out["network_denials"] == nil {
		t.Error(out)
	}
	if out := ResultOutput("orca", s, "", false, map[string]any{"status": "ok"}); out["answer"] != "" || out["network_denials"] != nil {
		t.Error(out)
	}
}

func TestReadResult(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "r.json")
	for body, ok := range map[string]bool{`{"status": "ok"}`: true, `{"status": "escalate"}`: true,
		`{"status": "done"}`: false, `{"status": "ok"`: false, `[1]`: false} {
		_ = os.WriteFile(p, []byte(body), 0o644)
		if got := ReadResult(p) != nil; got != ok {
			t.Errorf("%s: %v", body, got)
		}
	}
	ResetResult(filepath.Join(dir, "sub", "x.json"))
	if st, err := os.Stat(filepath.Join(dir, "sub")); err != nil || !st.IsDir() {
		t.Error("ResetResult did not make the dir")
	}
}
