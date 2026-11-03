package telemetry

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRunSummaryDerivesTheRecord(t *testing.T) {
	tasks := []RunTask{
		{Cls: "standard", Status: "integrated", Executor: "pi", Attempts: 1},
		{Cls: "light", Status: "needs", Executor: "claude", Attempts: 3},
		{Cls: "deep", Status: "failed", Executor: "", Attempts: 0},
		{Cls: "standard", Status: "blocked", Executor: "opencode", Attempts: 1},
	}
	dispatches := []map[string]any{
		decode(t, `{"status":"ok","executor":"pi","input_tokens":100,"output_tokens":7}`),
		decode(t, `{"status":"unavailable","executor":"claude","input_tokens":50,"api_duration_ms":-5}`),
	}
	cases := []struct {
		name  string
		tasks []RunTask
		want  string
	}{
		{
			name:  "with wave tasks",
			tasks: tasks,
			want: `{"event":"run_summary","lane":"orchestrated","task_count":4,"pass":1,"fail":2,"exhausted":1,"fallbacks":1,` +
				`"classes":{"standard":2,"light":1,"deep":1},"rounds":{"1":2,"3":1},"executors_used":{"pi":1,"claude":2},` +
				`"usage":{"input_tokens":150,"output_tokens":7}}`,
		},
		{
			name: "without wave tasks",
			want: `{"event":"run_summary","lane":"orchestrated","task_count":2,"pass":1,"fail":1,"fallbacks":1,` +
				`"executors_used":{"pi":1,"claude":1},"usage":{"input_tokens":150,"output_tokens":7}}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := RunSummary(c.tasks, dispatches)
			if want := decode(t, c.want); !reflect.DeepEqual(got, want) {
				t.Errorf("RunSummary = %s\nwant %s", asJSON(got), c.want)
			}
		})
	}
}

func TestRunSummaryEmptyRunKeepsZeroCountsAndNoHistograms(t *testing.T) {
	got := RunSummary(nil, nil)
	want := decode(t, `{"event":"run_summary","lane":"orchestrated","task_count":0,"pass":0,"fail":0,"fallbacks":0}`)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RunSummary = %s", asJSON(got))
	}
}

func TestRunSummaryUsageSumsOnlyNonNegativeIntegers(t *testing.T) {
	dispatches := []map[string]any{
		{"status": "ok", "input_tokens": int64(0), "output_tokens": 2.5, "api_duration_ms": "9"},
		{"status": "ok", "input_tokens": -4, "cached_input_tokens": int64(3)},
		{"status": "ok", "usage_extra": int64(1), "session_duration_ms": int64(0)},
	}
	usage, ok := RunSummary(nil, dispatches)["usage"].(map[string]any)
	if !ok {
		t.Fatal("usage missing")
	}
	want := map[string]any{"input_tokens": int64(0), "cached_input_tokens": int64(3), "session_duration_ms": int64(0)}
	if !reflect.DeepEqual(usage, want) {
		t.Errorf("usage = %v, want %v", usage, want)
	}
	if _, present := RunSummary(nil, []map[string]any{{"status": "ok", "input_tokens": -1}})["usage"]; present {
		t.Error("usage written when empty")
	}
}

func TestRunSummaryRoundsAndExecutorsIgnoreUnusableValues(t *testing.T) {
	tasks := []RunTask{
		{Cls: "light", Status: "integrated", Executor: "", Attempts: 0},
		{Cls: "light", Status: "integrated", Executor: "", Attempts: 2},
	}
	got := RunSummary(tasks, nil)
	if !reflect.DeepEqual(got["rounds"], map[string]any{"2": int64(1)}) {
		t.Errorf("rounds = %v", got["rounds"])
	}
	if _, present := got["executors_used"]; present {
		t.Errorf("empty executors written: %v", got["executors_used"])
	}
	if _, present := RunSummary(tasks, nil)["exhausted"]; !present {
		t.Error("exhausted missing from the wave branch")
	}
}

func TestRunSummaryTasklessBranchHasNoWaveKeys(t *testing.T) {
	dispatches := []map[string]any{
		{"status": "needs-permission"},
		{"cls": "deep", "executor": "pi", "status": "ok"},
	}
	got := RunSummary(nil, dispatches)
	for _, key := range []string{"exhausted", "rounds"} {
		if _, present := got[key]; present {
			t.Errorf("%s written without tasks: %s", key, asJSON(got))
		}
	}
	if got["pass"] != int64(1) || got["fail"] != int64(1) {
		t.Errorf("pass/fail = %v/%v, want 1/1 (missing status is a fail)", got["pass"], got["fail"])
	}
}

func TestRunSummarySurvivesSanitize(t *testing.T) {
	tasks := []RunTask{
		{Cls: "standard", Status: "integrated", Executor: "pi", Attempts: 2},
		{Cls: "deep", Status: "needs", Executor: "claude", Attempts: 3},
	}
	dispatches := []map[string]any{
		decode(t, `{"status":"ok","executor":"pi","input_tokens":5,"api_duration_ms":8}`),
		decode(t, `{"status":"unavailable","executor":"claude","output_tokens":1}`),
	}
	full := RunSummary(tasks, dispatches)
	clean, err := Sanitize(full)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range full {
		if !reflect.DeepEqual(clean[k], v) {
			t.Errorf("Sanitize changed %s: %v -> %v", k, v, clean[k])
		}
	}
}

func TestWriteReturnsTheRecordAsWritten(t *testing.T) {
	dir := t.TempDir()
	cfg := cfgOf(t, `{"telemetry": {"enabled": true, "dir": "`+dir+`"}}`)
	returned, err := Write(decode(t, `{"event": "run_summary", "pass": 1, "prompt": "/Users/secret"}`), cfg)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(RecordsPath(cfg))
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if got := asJSON(decode(t, lines[len(lines)-1])); got != asJSON(returned) {
		t.Errorf("written %s, returned %s", got, asJSON(returned))
	}
	if _, ok := returned["ts"]; !ok {
		t.Error("ts missing")
	}
	if returned["schema_version"] != int64(2) {
		t.Errorf("schema_version = %v", returned["schema_version"])
	}
	if _, ok := returned["prompt"]; ok {
		t.Error("non-allowlisted key returned")
	}
}

func TestWriteErrors(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "telemetry")
	off := cfgOf(t, `{"telemetry": {"enabled": "yes", "dir": "`+dir+`"}}`)
	if _, err := Write(decode(t, `{"event": "run_summary"}`), off); !errors.Is(err, ErrDisabled) {
		t.Errorf("disabled: err = %v", err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("disabled write created the directory")
	}

	on := cfgOf(t, `{"telemetry": {"enabled": true, "dir": "`+dir+`"}}`)
	if _, err := Write(decode(t, `{"event": "bogus"}`), on); !errors.Is(err, ErrRejected) {
		t.Errorf("invalid event: err = %v", err)
	}
	if _, err := Write(nil, on); !errors.Is(err, ErrRejected) {
		t.Errorf("nil record: err = %v", err)
	}

	bad := cfgOf(t, `{"telemetry": {"enabled": true, "dir": "/dev/null/x"}}`)
	_, err := Write(decode(t, `{"event": "run_summary"}`), bad)
	if err == nil || errors.Is(err, ErrDisabled) || errors.Is(err, ErrRejected) {
		t.Errorf("I/O failure: err = %v", err)
	}
}
