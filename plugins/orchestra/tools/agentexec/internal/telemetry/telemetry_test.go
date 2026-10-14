package telemetry

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
)

// decode reads JSON the way the CLI does (ordered objects, int64 numbers).
func decode(t testing.TB, text string) map[string]any {
	t.Helper()
	v, err := fsutil.DecodeJSON([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return v.(map[string]any)
}

func raw(t testing.TB, text string) map[string]any {
	t.Helper()
	o := decode(t, text)
	m := map[string]any{}
	for _, k := range slices.Sorted(maps.Keys(o)) {
		m[k] = o[k]
	}
	return m
}

func sanitize(t testing.TB, text string) map[string]any {
	t.Helper()
	out, err := Sanitize(raw(t, text))
	if err != nil {
		t.Fatalf("%s: %v", text, err)
	}
	return out
}

func asJSON(o map[string]any) string {
	b, _ := fsutil.MarshalJSON(o)
	return string(b)
}

func cfgOf(t testing.TB, text string) *config.Resolved {
	return &config.Resolved{Values: decode(t, text)}
}

func TestSanitizeDropsDisguisedAndMistypedValues(t *testing.T) {
	cases := []struct{ name, in, key string }{
		{"reason with a path", `{"event": "dispatch", "reason": "quota: /Users/x/secret.py"}`, "reason"},
		{"status with a path", `{"event": "dispatch", "status": "ok because /etc/passwd"}`, "status"},
		{"executor with a path", `{"event": "dispatch", "executor": "claude /home/u/proj"}`, "executor"},
		{"resumed 1", `{"event": "dispatch", "resumed": 1}`, "resumed"},
		{"resumed 0", `{"event": "dispatch", "resumed": 0}`, "resumed"},
		{"version with injection", `{"event": "dispatch", "orchestra_version": "1.2.3; cat /etc/passwd"}`, "orchestra_version"},
		{"version with newline path", `{"event": "dispatch", "orchestra_version": "1.2.3\n/evil/path"}`, "orchestra_version"},
		{"version with prefix", `{"event": "dispatch", "orchestra_version": "x1.2.3"}`, "orchestra_version"},
		{"version with suffix", `{"event": "dispatch", "orchestra_version": "1.2.3extra"}`, "orchestra_version"},
		{"version not a string", `{"event": "dispatch", "orchestra_version": 12345}`, "orchestra_version"},
		{"count string", `{"event": "dispatch", "task_count": "5; rm -rf"}`, "task_count"},
		{"count float", `{"event": "dispatch", "task_count": 3.5}`, "task_count"},
		{"count negative", `{"event": "dispatch", "pass": -1}`, "pass"},
		{"count bool", `{"event": "dispatch", "fail": true}`, "fail"},
		{"count object", `{"event": "dispatch", "task_count": {"nested": "dict"}}`, "task_count"},
		{"classes list", `{"event": "dispatch", "classes": []}`, "classes"},
		{"classes string", `{"event": "dispatch", "classes": "not-a-dict"}`, "classes"},
		{"reason object", `{"event": "dispatch", "reason": {"nested": "dict"}}`, "reason"},
		{"reason null", `{"event": "dispatch", "reason": null}`, "reason"},
		{"usage non-dict", `{"event": "dispatch", "usage": 5}`, "usage"},
		{"usage empty", `{"event": "dispatch", "usage": {}}`, "usage"},
		{"usage all invalid", `{"event": "dispatch", "usage": {"input_tokens": true, "output_tokens": -1, "cached_input_tokens": 1.5, "api_duration_ms": "9", "session_duration_ms": null}}`, "usage"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, ok := sanitize(t, c.in)[c.key]; ok {
				t.Errorf("%s survived: %s", c.key, c.in)
			}
		})
	}
}

func TestSanitizeKeepsValidValues(t *testing.T) {
	out := sanitize(t, `{"event": "dispatch", "lane": "express", "resumed": false, "orchestra_version": "1.2.3",
		"reason": "quota", "executor": "pi", "cls": "independent-review", "status": "needs-permission",
		"task_count": 0, "sandbox_granted": 4,
		"classes": {"light": 5, "../../etc/passwd": 9, "deep": "/path", "review": 0},
		"executors_used": {"pi": 1, "claude": 2, "x": 3},
		"rounds": {"1": 2, "abc": 3, "-1": 4, "07": 5, "10": 6},
		"external_enabled": {"pi": true, "claude": 1, "evil": true},
		"usage": {"input_tokens": 1, "output_tokens": 2, "cached_input_tokens": 3,
		"api_duration_ms": 4, "session_duration_ms": 5, "prompt_tokens": 6}}`)
	got := asJSON(out)
	for _, want := range []string{
		`"resumed":false`, `"orchestra_version":"1.2.3"`, `"classes":{"light":5,"review":0}`,
		`"executors_used":{"claude":2,"pi":1}`, `"rounds":{"1":2,"10":6}`,
		`"external_enabled":{"pi":true}`, `"task_count":0`, `"sandbox_granted":4`,
		`"usage":{"api_duration_ms":4,"cached_input_tokens":3,"input_tokens":1,"output_tokens":2,"session_duration_ms":5}`,
		`"cls":"independent-review"`, `"status":"needs-permission"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	if strings.Contains(got, "prompt_tokens") {
		t.Error("unknown usage key survived")
	}
}

func TestSanitizeStampsCannotBeSpoofed(t *testing.T) {
	out := sanitize(t, `{"event": "dispatch", "ts": "EVIL", "os": "/etc/x", "schema_version": 999}`)
	if v, _ := out["schema_version"]; v != int64(2) {
		t.Errorf("schema_version = %v", v)
	}
	if v, _ := out["os"]; v != runtime.GOOS {
		t.Errorf("os = %v", v)
	}
	ts := out["ts"]
	if _, err := time.Parse("2006-01-02T15:04:05.999999999-07:00", ts.(string)); err != nil {
		t.Errorf("ts %v: %v", ts, err)
	}
	if got := slices.Sorted(maps.Keys(out)); !reflect.DeepEqual(got, []string{"event", "os", "schema_version", "ts"}) {
		t.Errorf("keys = %v", got)
	}
}

func TestSanitizeRejectsWithoutAValidEvent(t *testing.T) {
	for _, in := range []string{`{"status": "ok"}`, `{"event": "dispatch; rm"}`, `{"event": 1}`, `{"event": null}`, `{}`} {
		if _, err := Sanitize(raw(t, in)); err == nil {
			t.Errorf("%s was accepted", in)
		}
	}
	if _, err := Sanitize(nil); err == nil {
		t.Error("nil accepted")
	}
}

func TestSanitizeNeverLeaksSensitiveTopLevelKeys(t *testing.T) {
	m := map[string]any{"event": "dispatch"}
	for _, k := range []string{"prompt", "file", "path", "cwd", "argv", "error", "summary", "task_id", "stack"} {
		m[k] = "/Users/secret/leak-" + k
	}
	out, err := Sanitize(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(asJSON(out), "secret") {
		t.Errorf("leak: %s", asJSON(out))
	}
}

func TestSanitizeAcceptsPlainGoMaps(t *testing.T) {
	out, err := Sanitize(map[string]any{
		"event":   "dispatch",
		"classes": map[string]any{"light": 2, "x": 1},
		"usage":   map[string]any{"input_tokens": int64(3)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := asJSON(out); !strings.Contains(got, `"classes":{"light":2}`) || !strings.Contains(got, `"usage":{"input_tokens":3}`) {
		t.Errorf("got %s", got)
	}
}

// FuzzSanitize: whatever JSON is thrown at it, the output holds only
// allowlisted keys and no string outside the fixed enums and stamps.
func FuzzSanitize(f *testing.F) {
	for _, seed := range []string{
		`{"event": "dispatch", "reason": "quota: /x", "usage": {"input_tokens": 1}}`,
		`{"event": "run_summary", "classes": {"light": 1}, "rounds": {"1": 2}}`,
		`{"event": "dispatch", "external_enabled": {"pi": true}, "resumed": 1}`,
		`{"event": {"nested": "x"}}`,
	} {
		f.Add(seed)
	}
	allowed := map[string]bool{
		"event": true, "schema_version": true, "ts": true, "os": true, "orchestra_version": true,
		"lane": true, "executor": true, "cls": true, "status": true, "reason": true, "resumed": true,
		"task_count": true, "pass": true, "fail": true, "exhausted": true, "fallbacks": true,
		"sandbox_denials": true, "sandbox_granted": true, "classes": true, "executors_used": true,
		"rounds": true, "external_enabled": true, "usage": true,
	}
	f.Fuzz(func(t *testing.T, in string) {
		v, err := fsutil.DecodeJSON([]byte(in))
		if err != nil {
			return
		}
		obj, ok := v.(map[string]any)
		if !ok {
			return
		}
		m := map[string]any{}
		for _, k := range slices.Sorted(maps.Keys(obj)) {
			m[k] = obj[k]
		}
		out, err := Sanitize(m)
		if err != nil {
			return
		}
		for _, k := range slices.Sorted(maps.Keys(out)) {
			if !allowed[k] {
				t.Fatalf("key %q leaked", k)
			}
		}
		var generic map[string]any
		if err := json.Unmarshal([]byte(asJSON(out)), &generic); err != nil {
			t.Fatal(err)
		}
		for k, val := range generic {
			if s, isStr := val.(string); isStr && k != "ts" && k != "os" && k != "orchestra_version" {
				found := false
				for _, enum := range append(append(append(append(events, lanes...), executors...), classes...), append(statuses, reasons...)...) {
					found = found || enum == s
				}
				if !found {
					t.Fatalf("%s=%q is not an enum value", k, s)
				}
			}
		}
	})
}

func TestBuildDispatchRecordNeverCopiesFreeText(t *testing.T) {
	result := decode(t, `{"status": "ok", "answer": "the secret file is at /Users/x/.ssh/id_rsa",
		"session_id": "abc123", "reason": null, "exit_code": 0}`)
	rec := BuildDispatchRecord("pi", result, false, "")
	if got := slices.Sorted(maps.Keys(rec)); !reflect.DeepEqual(got, []string{"event", "executor", "reason", "resumed", "status"}) {
		t.Errorf("keys = %v", got)
	}
	rec = BuildDispatchRecord("pi", decode(t, `{"status": "unavailable", "answer": "/etc/passwd", "reason": "quota"}`), true, "standard")
	if got := asJSON(rec); got != `{"cls":"standard","event":"dispatch","executor":"pi","reason":"quota","resumed":true,"status":"unavailable"}` {
		t.Errorf("got %s", got)
	}
}

func TestBuildDispatchRecordFlattensUsage(t *testing.T) {
	result := decode(t, `{"status": "ok", "sandbox": {"denials": ["a", "b"], "granted": []},
		"usage": {"api_duration_ms": 5, "session_duration_ms": true, "tokens": {"input_tokens": 1,
		"output_tokens": -2, "cached_input_tokens": 0, "cache_write_input_tokens": 9}}}`)
	got := asJSON(BuildDispatchRecord("pi", result, false, "light"))
	want := `{"cls":"light","event":"dispatch","executor":"pi","reason":null,"resumed":false,` +
		`"sandbox_denials":2,"sandbox_granted":0,"status":"ok",` +
		`"usage":{"api_duration_ms":5,"cached_input_tokens":0,"input_tokens":1}}`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	for _, u := range []string{`{}`, `{"tokens": {}}`, `{"api_duration_ms": -1, "tokens": {"input_tokens": "7"}}`, `5`} {
		r := BuildDispatchRecord("pi", decode(t, `{"status": "ok", "usage": `+u+`}`), false, "")
		if _, ok := r["usage"]; ok {
			t.Errorf("usage %s should be omitted", u)
		}
	}
}

func TestAppendIsGatedAndSanitized(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "telemetry")
	rec := decode(t, `{"event": "dispatch", "status": "ok", "prompt": "/Users/secret/leak"}`)

	Append(rec, cfgOf(t, `{"telemetry": {"enabled": false, "dir": "`+dir+`"}}`))
	Append(rec, cfgOf(t, `{"telemetry": {"enabled": "yes", "dir": "`+dir+`"}}`))
	Append(rec, cfgOf(t, `{}`))
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("disabled telemetry created its directory")
	}

	on := cfgOf(t, `{"telemetry": {"enabled": true, "dir": "`+dir+`"}}`)
	Append(rec, on)
	Append(decode(t, `{"event": "bogus"}`), on)
	Append(nil, on)
	records := ReadRecords(filepath.Join(dir, "records.jsonl"))
	if len(records) != 1 {
		t.Fatalf("records = %d", len(records))
	}
	line, _ := os.ReadFile(filepath.Join(dir, "records.jsonl"))
	if strings.Contains(string(line), "secret") || !strings.Contains(string(line), `"status":"ok"`) {
		t.Errorf("line = %s", line)
	}
}

func TestAppendNeverFailsOnGarbage(t *testing.T) {
	on := cfgOf(t, `{"telemetry": {"enabled": true, "dir": "/dev/null/x"}}`)
	Append(decode(t, `{"event": "dispatch"}`), on)
	Append(decode(t, `{"event": "dispatch", "classes": "not-a-dict"}`), cfgOf(t, `{"telemetry": {"enabled": true, "dir": "`+t.TempDir()+`"}}`))
}

func TestAppendEnforcesTheLineCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "records.jsonl")
	var b strings.Builder
	for i := 0; i < maxLines; i++ {
		b.WriteString(`{"event": "dispatch", "n": ` + "1" + "}\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	Append(decode(t, `{"event": "run_summary"}`), cfgOf(t, `{"telemetry": {"enabled": true, "dir": "`+dir+`"}}`))
	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != maxLines || !strings.Contains(lines[len(lines)-1], "run_summary") {
		t.Errorf("lines = %d, last = %s", len(lines), lines[len(lines)-1])
	}
}

func TestReadRecordsAndSummary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.jsonl")
	body := `{"event": "dispatch", "status": "ok"}` + "\n\nnot json\n[1]\n" +
		`{"event": "dispatch", "status": "unavailable", "reason": "quota"}` + "\n" +
		`{"event": "run_summary", "status": 7}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	records := ReadRecords(path)
	if len(records) != 4 {
		t.Fatalf("records = %d", len(records))
	}
	want := "total records: 4\n" +
		`by event: {"dispatch":2,"run_summary":1}` + "\n" +
		`by status: {"7":1,"ok":1,"unavailable":1}` + "\n" +
		`by reason: {"quota":1}` + "\n"
	if got := Summary(records); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if got := ReadRecords(filepath.Join(t.TempDir(), "missing")); len(got) != 0 {
		t.Errorf("missing file -> %v", got)
	}
}
