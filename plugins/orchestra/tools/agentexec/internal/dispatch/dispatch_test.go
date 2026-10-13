package dispatch

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
)

// testConfig is the defaults with the ledger (and so tokens/, sessions/)
// under a temp dir.
func testConfig(t *testing.T) *config.Resolved {
	t.Helper()
	base := t.TempDir()
	values := config.Defaults()
	values.Object("ledger").Set("dir", filepath.Join(base, "runs"))
	return &config.Resolved{Values: values, Root: base}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func age(t *testing.T, path string, d time.Duration) {
	t.Helper()
	old := time.Now().Add(-d)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

func ptr(s string) *string { return &s }

func TestPrepareRoundTripAndPermissions(t *testing.T) {
	cfg := testConfig(t)
	wd := t.TempDir()
	token, err := Prepare(cfg, PrepareSpec{
		Class: "standard", Workdir: wd, PromptFiles: []string{filepath.Join(wd, "not-yet.md")},
		Task: ptr("t-1"), RunID: ptr("run-1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !TokenPattern.MatchString(token) {
		t.Fatalf("token %q", token)
	}
	fi, err := os.Stat(filepath.Join(TokenDir(cfg), token+".json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("spec file mode: %v %v", fi.Mode(), err)
	}
	if di, _ := os.Stat(TokenDir(cfg)); di.Mode().Perm() != 0o700 {
		t.Fatalf("token dir mode %v", di.Mode())
	}
	got, err := LoadToken(cfg, token)
	if err != nil {
		t.Fatal(err)
	}
	want := PrepareSpec{Class: "standard", Archetype: "default", Workdir: wd, Isolate: "auto",
		ConfigRoot: cfg.Root, PromptFiles: []string{filepath.Join(wd, "not-yet.md")},
		Task: ptr("t-1"), RunID: ptr("run-1")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	other, _ := Prepare(cfg, PrepareSpec{Class: "light", Workdir: wd, PromptFiles: []string{"x"}})
	if other == token {
		t.Fatal("tokens must be unique")
	}
}

func TestLoadTokenRejectsBadTokensAndSpecs(t *testing.T) {
	cfg := testConfig(t)
	dir := TokenDir(cfg)
	good := `{"class":"light","archetype":"default","workdir":"/w","run_id":null,"isolate":"never","task":null,"prompt_files":["/w/p.md"]}`
	mustWrite(t, filepath.Join(dir, "dsp-000000000001.json"), good)
	if _, err := LoadToken(cfg, "dsp-000000000001"); err != nil {
		t.Fatalf("a spec without config_root still loads: %v", err)
	}
	bad := map[string]string{
		"dsp-000000000002": `{"class":"light"`,
		"dsp-000000000003": `["not", "an", "object"]`,
		"dsp-000000000004": strings.Replace(good, `"isolate":"never"`, `"isolate":"sometimes"`, 1),
		"dsp-000000000005": strings.Replace(good, `"/w/p.md"`, `"p.md"`, 1),
		"dsp-000000000006": strings.Replace(good, `"task":null,`, ``, 1),
		"dsp-000000000007": strings.Replace(good, `"run_id":null`, `"run_id":7`, 1),
		"dsp-000000000008": strings.Replace(good, `"class":"light"`, `"class":null`, 1),
	}
	for token, content := range bad {
		mustWrite(t, filepath.Join(dir, token+".json"), content)
		if _, err := LoadToken(cfg, token); err != ErrInvalidToken {
			t.Errorf("%s (%s): err %v, want ErrInvalidToken", token, content, err)
		}
	}
	// A symlinked spec is never followed.
	if err := os.Symlink(filepath.Join(dir, "dsp-000000000001.json"), filepath.Join(dir, "dsp-00000000000a.json")); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"dsp-00000000000a", "dsp-XYZ", "../tokens/dsp-000000000001", "dsp-000000000001.json", "", "dsp-ffffffffffff"} {
		if _, err := LoadToken(cfg, token); err != ErrInvalidToken {
			t.Errorf("%q: err %v, want ErrInvalidToken", token, err)
		}
	}
}

func TestSessionStore(t *testing.T) {
	cfg := testConfig(t)
	wd := t.TempDir()
	if _, ok := ReadSession(cfg, "pi", "t-1"); ok {
		t.Fatal("empty store must miss")
	}
	rec, err := WriteSession(cfg, "pi", "t-1", "sid-1", wd)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := ReadSession(cfg, "pi", "t-1")
	if !ok || got != rec || got.SessionID != "sid-1" || got.Workdir != wd {
		t.Fatalf("read back %+v, %v", got, ok)
	}
	fi, _ := os.Stat(filepath.Join(SessionDir(cfg), "pi-t-1.json"))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("record mode %v", fi.Mode())
	}
	for _, content := range []string{"", "{", `{"executor":"pi"}`, `[1]`,
		`{"executor":"pi","task":"t-1","session_id":"","workdir":"/w","updated":"x"}`,
		`{"executor":"pi","task":"t-1","session_id":5,"workdir":"/w","updated":"x"}`} {
		mustWrite(t, filepath.Join(SessionDir(cfg), "pi-t-1.json"), content)
		if _, ok := ReadSession(cfg, "pi", "t-1"); ok {
			t.Errorf("%q must read as a miss", content)
		}
	}
	if _, err := WriteSession(cfg, "pi", "///", "sid", wd); err == nil {
		t.Error("an unusable task id must not be written")
	}
	if _, ok := ReadSession(cfg, "../pi", "../../x"); ok {
		t.Error("traversal must miss")
	}
}

func TestSameWorkdirResolvesSymlinks(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if !sameWorkdir(real, link) {
		t.Error("a symlink to the same tree is the same workdir")
	}
	if sameWorkdir(real, t.TempDir()) || sameWorkdir("", real) {
		t.Error("different or empty workdirs are not the same")
	}
}

func TestSweepRetention(t *testing.T) {
	cfg := testConfig(t)
	cfg.Values.Object("ledger").Set("retention_days", int64(2))
	tokens, sessions, detach := TokenDir(cfg), SessionDir(cfg), DetachDir(cfg)
	files := map[string]bool{ // path -> survives
		filepath.Join(tokens, "dsp-000000000001.json"):  false,
		filepath.Join(tokens, "notes.txt"):              true,
		filepath.Join(sessions, "pi-old.json"):          false,
		filepath.Join(detach, "dsp-0000000000dd.json"):  false,
		filepath.Join(detach, "dsp-0000000000dd.out"):   false,
		filepath.Join(detach, "dsp-0000000000aa.json"):  true, // its child is alive
		filepath.Join(detach, "dsp-0000000000aa.out"):   true,
		filepath.Join(ledgerDirOf(cfg), "s", "x.jsonl"): false,
	}
	for path := range files {
		content := "{}"
		switch filepath.Base(path) {
		case "dsp-0000000000aa.json":
			content = `{"pid": ` + strconv.Itoa(os.Getpid()) + `}`
		case "dsp-0000000000dd.json":
			content = `{"pid": 999999999}`
		}
		mustWrite(t, path, content)
		age(t, path, 3*24*time.Hour)
	}
	fresh := filepath.Join(tokens, "dsp-000000000002.json")
	mustWrite(t, fresh, "{}")
	files[fresh] = true

	ResetRetention()
	SweepRetention(cfg)
	for path, survives := range files {
		_, err := os.Stat(path)
		if (err == nil) != survives {
			t.Errorf("%s: exists=%v, want %v", path, err == nil, survives)
		}
	}
}

func ledgerDirOf(cfg *config.Resolved) string {
	d, _ := cfg.String("ledger", "dir")
	return d
}

func TestSummarizeCountsCodePoints(t *testing.T) {
	for _, c := range []struct {
		answer    string
		truncated bool
	}{
		{strings.Repeat("x😀", 200), false}, // exactly 400 code points
		{strings.Repeat("x😀", 201), true},
		{strings.Repeat("あ", 401), true},
		{"short", false},
	} {
		in := jsonx.NewObject().Set("status", "ok").Set("answer", c.answer)
		out := Summarize(in)
		got, _ := out.Get("answer")
		_, flag := out.Get("answer_truncated")
		if flag != c.truncated || len([]rune(got.(string))) > SummaryAnswerChars {
			t.Errorf("%d code points: truncated=%v len=%d", len([]rune(c.answer)), flag, len([]rune(got.(string))))
		}
		if c.truncated && got.(string) != string([]rune(c.answer)[:SummaryAnswerChars]) {
			t.Error("the cut keeps the first 400 code points")
		}
		if orig, _ := in.Get("answer"); orig != c.answer {
			t.Error("Summarize must not modify its input")
		}
	}
}

func TestExtractJSONObject(t *testing.T) {
	cases := map[string]bool{
		`{"status": "ok"}`:                         true,
		"banner line\n{\"status\": \"ok\"}\ntrail": true,
		"":             false,
		"no json here": false,
		`[1, 2]`:       false,
		"} reversed {": false,
	}
	for raw, want := range cases {
		m, ok := ExtractJSONObject(raw)
		if ok != want || (ok && m["status"] != "ok") {
			t.Errorf("%q: %v %v", raw, m, ok)
		}
	}
}

func putState(t *testing.T, cfg *config.Resolved, token, content string) {
	t.Helper()
	mustWrite(t, detachFile(cfg, token, ".json"), content)
}

func TestWaitStates(t *testing.T) {
	cfg := testConfig(t)
	if out, _ := Wait(cfg, "dsp-000000000001", 0, false); get(out, "status") != "unknown-token" {
		t.Fatalf("no state: %v", out)
	}

	// A live child (this process) is still running after a zero wait.
	putState(t, cfg, "dsp-000000000002", `{"pid": `+strconv.Itoa(os.Getpid())+`, "wait_calls": 0}`)
	out, _ := Wait(cfg, "dsp-000000000002", 0, false)
	if get(out, "status") != "running" || get(out, "wait_calls") != int64(1) {
		t.Fatalf("running: %v", out)
	}
	out, _ = Wait(cfg, "dsp-000000000002", 0, false)
	if get(out, "wait_calls") != int64(2) {
		t.Fatalf("wait_calls must count every wait: %v", out)
	}

	// A dead child with garbage output is a persisted relay failure.
	putState(t, cfg, "dsp-000000000003", `{"pid": 999999999}`)
	mustWrite(t, detachFile(cfg, "dsp-000000000003", ".out"), "Traceback: boom")
	out, _ = Wait(cfg, "dsp-000000000003", 0, false)
	if get(out, "status") != "relay-failed" || get(out, "raw") != "Traceback: boom" {
		t.Fatalf("relay-failed: %v", out)
	}
	again, _ := Wait(cfg, "dsp-000000000003", 0, false)
	if get(again, "status") != "relay-failed" {
		t.Fatalf("the result is persisted: %v", again)
	}
	state := readObject(detachFile(cfg, "dsp-000000000003", ".json"))
	if get(state, "collected") != true || get(state, "wait_calls") != int64(2) {
		t.Fatalf("state after collect: %v", state)
	}
	if _, err := os.Stat(detachFile(cfg, "dsp-000000000003", ".out")); err == nil {
		t.Error("the output file is removed once collected")
	}

	// A finished child's JSON result gets its answer file; --summary cuts.
	putState(t, cfg, "dsp-000000000004", `{"pid": 999999999}`)
	long := strings.Repeat("語", 450)
	mustWrite(t, detachFile(cfg, "dsp-000000000004", ".out"), `noise {"status": "ok", "answer": "`+long+`"}`)
	out, _ = Wait(cfg, "dsp-000000000004", 0, true)
	if get(out, "answer_truncated") != true || len([]rune(get(out, "answer").(string))) != 400 {
		t.Fatalf("summary: %v", get(out, "answer_truncated"))
	}
	path, _ := get(out, "answer_path").(string)
	if b, err := os.ReadFile(path); err != nil || string(b) != long {
		t.Fatalf("answer file %q: %v", path, err)
	}
	full, _ := Wait(cfg, "dsp-000000000004", 0, false)
	if get(full, "answer") != long {
		t.Error("a later wait without --summary returns the full answer")
	}
}

func TestDetachIsIdempotentPerToken(t *testing.T) {
	cfg := testConfig(t)
	putState(t, cfg, "dsp-000000000001", `{"pid": `+strconv.Itoa(os.Getpid())+`}`)
	if out := Detach(cfg, "dsp-000000000001", nil); get(out, "status") != "running" {
		t.Fatalf("live child: %v", out)
	}
	putState(t, cfg, "dsp-000000000002", `{"pid": 999999999}`)
	mustWrite(t, detachFile(cfg, "dsp-000000000002", ".out"), `{"status": "ok"}`)
	if out := Detach(cfg, "dsp-000000000002", nil); get(out, "status") != "done" {
		t.Fatalf("finished, not collected: %v", out)
	}
	if _, err := os.Stat(detachFile(cfg, "dsp-000000000002", ".out")); err != nil {
		t.Error("done must keep the output for wait")
	}
}

func TestListDetached(t *testing.T) {
	cfg := testConfig(t)
	wd := t.TempDir()
	token, _ := Prepare(cfg, PrepareSpec{Class: "deep", Workdir: wd, PromptFiles: []string{"p"}, Task: ptr("t-9")})
	putState(t, cfg, token, `{"pid": 999999999, "token": "`+token+`", "started": 1.5, "wait_calls": 2}`)
	mustWrite(t, detachFile(cfg, token, ".out"), `{"status": "ok"}`)
	putState(t, cfg, "dsp-000000000002", `{"pid": `+strconv.Itoa(os.Getpid())+`, "collected": true}`)
	mustWrite(t, detachFile(cfg, "dsp-000000000002", ".result.json"), `{}`)
	putState(t, cfg, "dsp-000000000003", `not json`)

	list := ListDetached(cfg)
	byToken := map[string]Detached{}
	for _, d := range list {
		byToken[d.Token] = d
	}
	if len(list) != 2 {
		t.Fatalf("list %+v", list)
	}
	d := byToken[token]
	if d.Alive || !d.ResultPresent || d.Collected || d.WaitCalls != 2 || d.Class != "deep" || d.Task != "t-9" {
		t.Errorf("finished uncollected: %+v", d)
	}
	d = byToken["dsp-000000000002"]
	if !d.Alive || !d.ResultPresent || !d.Collected {
		t.Errorf("collected: %+v", d)
	}
}

func TestWriteFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "out.txt")
	out, code := WriteFile(path, 3, strings.NewReader("日本語\n"))
	if code != 0 || get(out, "chars") != int64(3) || get(out, "path") != path {
		t.Fatalf("%v %d", out, code)
	}
	if b, _ := os.ReadFile(path); string(b) != "日本語\n" {
		t.Fatalf("content %q", b)
	}
	out, code = WriteFile(path, 1, strings.NewReader("new\n\n"))
	if code != 1 || get(out, "status") != "mismatch" || get(out, "chars") != int64(4) {
		t.Fatalf("mismatch: %v %d", out, code)
	}
	if b, _ := os.ReadFile(path); string(b) != "日本語\n" {
		t.Fatal("a mismatch must leave the file untouched")
	}
	if _, code := WriteFile(path, -1, strings.NewReader("")); code != 1 {
		t.Fatal("a negative count can never match")
	}
	out, code = WriteFile(filepath.Join(dir, "crlf"), NoExpect, strings.NewReader("a\r\nb\r\n"))
	if code != 0 || get(out, "chars") != int64(5) {
		t.Fatalf("crlf: %v", out)
	}
	if _, code := WriteFile(filepath.Join(dir, "bin"), NoExpect, strings.NewReader("\xff\xfe")); code != 2 {
		t.Fatal("invalid UTF-8 is an error")
	}
	mustWrite(t, filepath.Join(dir, "file"), "x")
	if out, code := WriteFile(filepath.Join(dir, "file", "under"), NoExpect, strings.NewReader("x")); code != 2 || get(out, "error") == nil {
		t.Fatalf("a file as parent: %v %d", out, code)
	}
}

func TestRequestArgs(t *testing.T) {
	r := Request{Class: "deep", Workdir: "/w", Isolate: "never", PromptFiles: []string{"/a", "/b"},
		RunID: "r", Task: "t", NoResume: true, Exhausted: []string{"pi", "claude"}}
	want := []string{"--class", "deep", "--archetype", "default", "--workdir", "/w", "--isolate", "never",
		"--prompt-file", "/a", "--prompt-file", "/b", "--run-id", "r", "--task", "t", "--no-resume",
		"--exhausted", "pi,claude"}
	if got := r.Args(); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}
