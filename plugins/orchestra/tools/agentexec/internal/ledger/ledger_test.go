package ledger

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
)

func obj(t *testing.T, text string) map[string]any {
	t.Helper()
	v, err := fsutil.DecodeJSON([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return v.(map[string]any)
}

func cfgOf(t *testing.T, text string) *config.Resolved {
	return &config.Resolved{Values: obj(t, text)}
}

func session(t *testing.T, id string) {
	t.Helper()
	t.Setenv("CLAUDE_CODE_SESSION_ID", id)
	ResetRetention()
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

func TestSanitizeAllowlistAndNumericValidation(t *testing.T) {
	raw := `{"ts": "2026-08-19T07:00:00+00:00", "executor": "pi", "cls": "standard",
		"model": "gpt-5.6-luna", "status": "error", "input_tokens": 1, "output_tokens": 2,
		"cached_input_tokens": 3, "api_duration_ms": 6, "session_duration_ms": 7, "extra": "drop"}`
	got := Sanitize(obj(t, raw))
	if _, ok := got["extra"]; ok {
		t.Error("extra key survived")
	}
	for _, k := range slices.Sorted(maps.Keys(obj(t, raw))) {
		if _, ok := got[k]; !ok && k != "extra" {
			t.Errorf("%s was dropped", k)
		}
	}
	for _, key := range []string{"input_tokens", "output_tokens", "cached_input_tokens",
		"api_duration_ms", "session_duration_ms", "cache_write_input_tokens", "reasoning_output_tokens"} {
		for _, bad := range []string{"true", "false", "-1", "1.5", `"7"`, "null"} {
			in := obj(t, raw)
			v, _ := fsutil.DecodeJSON([]byte(bad))
			in[key] = v
			if _, ok := Sanitize(in)[key]; ok {
				t.Errorf("%s=%s survived", key, bad)
			}
		}
		in := obj(t, raw)
		in[key] = int64(5)
		if v, _ := Sanitize(in)[key]; v != int64(5) {
			t.Errorf("%s=5 became %v", key, v)
		}
	}
}

func TestSanitizeRejectsOrFiltersEnums(t *testing.T) {
	for _, name := range []string{"copilot", "opencode"} {
		got := Sanitize(obj(t, `{"executor": "`+name+`", "status": "ok", "input_tokens": 1}`))
		if _, ok := got["executor"]; ok {
			t.Errorf("removed executor %s accepted", name)
		}
		if v, _ := got["input_tokens"]; v != int64(1) {
			t.Errorf("%s: input_tokens lost", name)
		}
	}
	for _, status := range []string{"not-a-real-status", "delegated"} {
		got := Sanitize(obj(t, `{"executor": "pi", "cls": "standard", "status": "`+status+`"}`))
		if _, ok := got["status"]; ok {
			t.Errorf("status %q accepted", status)
		}
	}
	for _, model := range []string{"has space", "has\\nnewline", strings.Repeat("x", 65)} {
		got := Sanitize(obj(t, `{"executor": "pi", "status": "ok", "input_tokens": 1, "model": "`+model+`"}`))
		if _, ok := got["model"]; ok {
			t.Errorf("model %q accepted", model)
		}
	}
	if Sanitize(obj(t, `{"input_tokens": 1}`)) != nil {
		t.Error("a record without executor/cls/status/model must be dropped")
	}
	if len(Sanitize(obj(t, `{"ts": "a\nb", "executor": "pi"}`))) != 1 {
		t.Error("a ts with a newline must be dropped")
	}
}

func TestAppendWritesCompactSessionFiles(t *testing.T) {
	session(t, "session-test")
	tmp := t.TempDir()
	cfg := cfgOf(t, `{"telemetry": {"dir": "`+filepath.Join(tmp, "telemetry")+`", "enabled": false}}`)
	rec := obj(t, `{"executor": "pi", "model": "gpt-5.6-luna", "status": "ok",
		"prompt": "secret prompt text", "nested": {"prompt": "secret"}}`)
	Append(rec, "wf_a26027ae-bdb", cfg)
	Append(rec, "wf_a26027ae-bdb", cfg)

	dir := filepath.Join(tmp, "runs")
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("ledger dir: %v %v", fi, err)
	}
	lines := readLines(t, filepath.Join(dir, "session-test", "001-wf_a26027ae-bdb.jsonl"))
	if len(lines) != 2 {
		t.Fatalf("lines = %d", len(lines))
	}
	for _, l := range lines {
		if strings.Contains(l, ": ") || strings.Contains(l, "secret") {
			t.Errorf("not compact or leaks: %s", l)
		}
	}
}

func TestAppendNeverFails(t *testing.T) {
	session(t, "session-test")
	Append(obj(t, `{"executor": "pi", "status": "ok"}`), "run",
		cfgOf(t, `{"telemetry": {"dir": "/dev/null/telemetry"}}`))
}

func TestAppendAllocatesOrdinalsAndReusesThem(t *testing.T) {
	session(t, "session-test")
	tmp := t.TempDir()
	cfg := cfgOf(t, `{"ledger": {"dir": "`+filepath.Join(tmp, "runs")+`", "enabled": true}}`)
	rec := obj(t, `{"executor": "pi", "status": "ok"}`)
	Append(rec, "wf_a", cfg)
	Append(rec, "wf_a", cfg)
	Append(rec, "wf_b", cfg)
	entries, _ := os.ReadDir(filepath.Join(tmp, "runs", "session-test"))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{".ordinal-001.reserve", ".ordinal-002.reserve", ".ordinal-run-wf_a.claim",
		".ordinal-run-wf_b.claim", "001-wf_a.jsonl", "002-wf_b.jsonl"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("files = %v", names)
	}
	if n := len(ReadFile(filepath.Join(tmp, "runs", "session-test", "001-wf_a.jsonl"))); n != 2 {
		t.Errorf("records = %d", n)
	}
}

func TestConcurrentWritersGetDistinctOrdinals(t *testing.T) {
	session(t, "parallel-session")
	tmp := t.TempDir()
	cfg := cfgOf(t, `{"ledger": {"dir": "`+filepath.Join(tmp, "runs")+`", "enabled": true}}`)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(run string) {
			defer wg.Done()
			Append(obj(t, `{"executor": "pi", "status": "ok"}`), run, cfg)
		}(fmt.Sprintf("wf_%d", i))
	}
	wg.Wait()
	dir := filepath.Join(tmp, "runs", "parallel-session")
	entries, _ := os.ReadDir(dir)
	var ordinals []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			ordinals = append(ordinals, strings.SplitN(e.Name(), "-", 2)[0])
			if n := len(ReadFile(filepath.Join(dir, e.Name()))); n != 1 {
				t.Errorf("%s has %d records", e.Name(), n)
			}
		}
	}
	sort.Strings(ordinals)
	if !reflect.DeepEqual(ordinals, []string{"001", "002", "003", "004", "005", "006"}) {
		t.Errorf("ordinals = %v", ordinals)
	}
}

func TestInvalidOrMissingSessionWritesNothing(t *testing.T) {
	for _, bad := range []string{"..", ".", "../../etc/x", "a/b", "", " ", strings.Repeat("x", 129), "line\nbreak"} {
		t.Run(fmt.Sprintf("%q", bad), func(t *testing.T) {
			session(t, bad)
			tmp := t.TempDir()
			cfg := cfgOf(t, `{"ledger": {"dir": "`+filepath.Join(tmp, "runs")+`"}}`)
			Append(obj(t, `{"executor": "pi", "status": "ok"}`), "run_id_ok", cfg)
			Append(obj(t, `{"executor": "pi", "status": "ok"}`), "", cfg)
			if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
				t.Errorf("created %v", entries)
			}
		})
	}
}

func TestLedgerSwitchAndRunIDValidation(t *testing.T) {
	session(t, "session-independence")
	tmp := t.TempDir()
	off := cfgOf(t, `{"ledger": {"dir": "`+filepath.Join(tmp, "off")+`", "enabled": false}}`)
	Append(obj(t, `{"executor": "pi", "status": "ok"}`), "run", off)
	notBool := cfgOf(t, `{"ledger": {"dir": "`+filepath.Join(tmp, "yes")+`", "enabled": "yes"}}`)
	Append(obj(t, `{"executor": "pi", "status": "ok"}`), "run", notBool)
	on := cfgOf(t, `{"ledger": {"dir": "`+filepath.Join(tmp, "on")+`", "enabled": true}}`)
	for _, bad := range []string{"a b", "../x", strings.Repeat("x", 65)} {
		Append(obj(t, `{"executor": "pi", "status": "ok"}`), bad, on)
	}
	Append(obj(t, `{"executor": "pi", "status": "ok"}`), "run", on)
	Append(obj(t, `{"executor": "pi", "status": "ok"}`), "", on)
	for _, d := range []string{"off", "yes"} {
		if _, err := os.Stat(filepath.Join(tmp, d)); err == nil {
			t.Errorf("%s was written", d)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(tmp, "on", "session-independence"))
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			names = append(names, e.Name())
		}
	}
	if !reflect.DeepEqual(names, []string{"001-run.jsonl", "no.run.jsonl"}) {
		t.Errorf("files = %v", names)
	}
}

func TestRetentionDeletesOldOnceAndKeepsTheRest(t *testing.T) {
	session(t, "session-retention")
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "runs")
	write := func(path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-40 * 24 * time.Hour)
	oldPath, newPath, notes := filepath.Join(dir, "old.jsonl"), filepath.Join(dir, "new.jsonl"), filepath.Join(dir, "notes.txt")
	nested := filepath.Join(dir, "session-old", "old.jsonl")
	for _, p := range []string{oldPath, newPath, notes, nested} {
		write(p)
	}
	subDir := filepath.Join(dir, "sub.jsonl")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{oldPath, nested, subDir} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	cfg := cfgOf(t, `{"ledger": {"dir": "`+dir+`", "enabled": true, "retention_days": 30}}`)
	rec := obj(t, `{"executor": "pi", "status": "ok"}`)
	Append(rec, "run", cfg)
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	if exists(oldPath) || exists(nested) {
		t.Error("old files survived")
	}
	if !exists(newPath) || !exists(notes) || !exists(subDir) || !exists(filepath.Dir(nested)) {
		t.Error("something that should stay was removed")
	}
	// A second append in the same process does not sweep again.
	write(oldPath)
	if err := os.Chtimes(oldPath, old, old); err != nil {
		t.Fatal(err)
	}
	Append(rec, "run", cfg)
	if !exists(oldPath) {
		t.Error("the sweep ran twice")
	}
}

func TestEnforceCapKeepsNewestLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.jsonl")
	var b strings.Builder
	for i := 0; i < maxLines+5; i++ {
		fmt.Fprintf(&b, "{\"n\":%d}\n", i)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	enforceCap(path)
	lines := readLines(t, path)
	if len(lines) != maxLines || lines[0] != "{\"n\":5}" {
		t.Errorf("lines=%d first=%s", len(lines), lines[0])
	}
}

func TestBuildRecordFlattensResult(t *testing.T) {
	result := obj(t, `{"status": "ok", "answer": "secret", "sandbox": {"denials": [1, 2], "granted": [1]},
		"usage": {"api_duration_ms": 9, "cost_micro_usd": 12, "tokens": {"input_tokens": 1, "output_tokens": 2,
		"cached_input_tokens": 3, "cache_write_input_tokens": 4, "reasoning_output_tokens": 5, "junk": 6}}}`)
	got := BuildRecord("pi", "m", "standard", result)
	b, _ := fsutil.MarshalJSON(got)
	want := `{"api_duration_ms":9,"cache_write_input_tokens":4,"cached_input_tokens":3,"cls":"standard",` +
		`"cost_micro_usd":12,"executor":"pi","input_tokens":1,"model":"m","output_tokens":2,` +
		`"reasoning_output_tokens":5,"sandbox_denials":2,"sandbox_granted":1,"status":"ok"}`
	if string(b) != want {
		t.Errorf("got  %s\nwant %s", b, want)
	}
	bare := BuildRecord("pi", "m", "light", obj(t, `{"status": "unavailable"}`))
	if len(bare) != 4 {
		t.Errorf("bare record = %v", slices.Sorted(maps.Keys(bare)))
	}
}

func TestDirFallsBackBesideTelemetry(t *testing.T) {
	if got := Dir(cfgOf(t, `{"telemetry": {"dir": "/x/telemetry"}}`)); got != "/x/runs" {
		t.Errorf("Dir = %s", got)
	}
	if got := Dir(cfgOf(t, `{"ledger": {"dir": "/y/led"}}`)); got != "/y/led" {
		t.Errorf("Dir = %s", got)
	}
}

func TestReadSideSkipsFlatFilesAndOrdersNumerically(t *testing.T) {
	dir := t.TempDir()
	put := func(rel, body string) {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put("flat.jsonl", `{"executor":"pi"}`+"\n")
	put("s1/999-a.jsonl", `{"executor":"pi"}`+"\n")
	put("s1/1000-b.jsonl", "bad\n\n[1]\n"+`{"executor":"pi"}`+"\r"+`{"executor":"claude"}`+"\n")
	put("s1/plain.jsonl", "")
	put("s1/no.run.jsonl", "")
	put("s2/001-a.jsonl", "")
	var rel []string
	for _, p := range Paths(dir) {
		r, _ := filepath.Rel(dir, p)
		rel = append(rel, r)
	}
	want := []string{"s1/999-a.jsonl", "s1/1000-b.jsonl", "s1/plain.jsonl", "s1/no.run.jsonl", "s2/001-a.jsonl"}
	if !reflect.DeepEqual(rel, want) {
		t.Errorf("Paths = %v", rel)
	}
	if n := len(ReadFile(filepath.Join(dir, "s1/1000-b.jsonl"))); n != 2 {
		t.Errorf("records = %d (a lone CR also ends a line)", n)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "s1")
	if run, ok := ResolveRunSelector(dir, "1000"); !ok || run != "b" {
		t.Errorf("ordinal 1000 -> %q %v", run, ok)
	}
	if run, ok := ResolveRunSelector(dir, "wf_x"); !ok || run != "wf_x" {
		t.Errorf("run id passthrough -> %q %v", run, ok)
	}
	if _, ok := ResolveRunSelector(dir, "7"); ok {
		t.Error("an unknown ordinal resolved")
	}
	if _, ok := ResolveRunSelector(dir, ".."); ok {
		t.Error("'..' resolved")
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	if _, ok := ResolveRunSelector(dir, "999"); ok {
		t.Error("an ordinal resolved without a session")
	}
	// A run selector never matches the flat file; a session selector never
	// matches a session directory of a different name.
	if got := SelectedPaths(dir, []string{"flat"}, nil); len(got) != 0 {
		t.Errorf("flat file selected: %v", got)
	}
	if got := SelectedPaths(dir, nil, []string{"s2"}); len(got) != 1 {
		t.Errorf("session s2: %v", got)
	}
	if got := SelectedPaths(dir, []string{"a"}, []string{"s1"}); len(got) != 5 {
		t.Errorf("run a + session s1: %v", got)
	}
	m := SessionOrdinalMap(dir, "s1")
	if !reflect.DeepEqual(m, map[string]int{"a": 999, "b": 1000}) {
		t.Errorf("ordinal map = %v", m)
	}
}
