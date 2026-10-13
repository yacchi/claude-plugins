package cooldown

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
)

func testConfig(t *testing.T, enabled bool) *config.Resolved {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "cd.json")
	return &config.Resolved{Values: jsonx.NewObject().Set("cooldown", jsonx.NewObject().
		Set("enabled", enabled).
		Set("path", path).
		Set("seconds", jsonx.NewObject().Set("quota", int64(3600)).Set("auth", int64(0))))}
}

func TestRecordAndActive(t *testing.T) {
	cfg := testConfig(t, true)
	now := time.Unix(1_000_000, 0)
	RecordUnavailable(cfg, "pi", "quota", now, nil, "")
	got := Active(cfg, now)
	if e := got["pi"]; e.Reason != "quota" || e.Until != 1_003_600 {
		t.Fatalf("active = %+v", got)
	}
	if len(Active(cfg, now.Add(2*time.Hour))) != 0 {
		t.Fatal("expired cooldown still active")
	}
}

func TestRecordSkips(t *testing.T) {
	cfg := testConfig(t, true)
	now := time.Unix(1_000_000, 0)
	zero := 0
	RecordUnavailable(cfg, "pi", "quota", now, &zero, "answered")        // evidently succeeded
	RecordUnavailable(cfg, "pi", "runaway", now, nil, "")                // never cools down
	RecordUnavailable(cfg, "pi", "auth", now, nil, "")                   // zero seconds
	RecordUnavailable(cfg, "pi", "unlisted", now, nil, "")               // no entry
	RecordUnavailable(testConfig(t, false), "pi", "quota", now, nil, "") // disabled
	if len(Active(cfg, now)) != 0 {
		t.Fatal("a cooldown was recorded")
	}
	if _, err := os.Stat(StatePath(cfg)); err == nil {
		t.Fatal("state file written")
	}
}

func TestRecordKeepsLongerExistingAndPrunes(t *testing.T) {
	cfg := testConfig(t, true)
	path := StatePath(cfg)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	state := `{"pi": {"reason": "credits", "until": 9999999}, "old": {"reason": "x", "until": 1}}`
	if err := os.WriteFile(path, []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_000_000, 0)
	RecordUnavailable(cfg, "pi", "quota", now, nil, "")
	RecordUnavailable(cfg, "other", "quota", now, nil, "")
	got := Active(cfg, now)
	if got["pi"].Reason != "credits" || got["other"].Reason != "quota" {
		t.Fatalf("active = %+v", got)
	}
	raw, _ := os.ReadFile(path)
	v, _ := jsonx.Decode(raw)
	if _, ok := v.(*jsonx.Object).Get("old"); ok {
		t.Fatal("expired entry not pruned")
	}
}

func TestClear(t *testing.T) {
	cfg := testConfig(t, true)
	now := time.Unix(1_000_000, 0)
	RecordUnavailable(cfg, "pi", "quota", now, nil, "")
	if _, err := Clear(cfg, "nosuch"); err == nil {
		t.Fatal("unknown executor accepted")
	}
	if got, err := Clear(cfg, "pi"); err != nil || len(got) != 1 {
		t.Fatalf("clear pi = %v, %v", got, err)
	}
	RecordUnavailable(cfg, "pi", "quota", now, nil, "")
	if got, _ := Clear(cfg, ""); len(got) != 1 {
		t.Fatalf("clear all = %v", got)
	}
	if _, err := os.Stat(StatePath(cfg)); err == nil {
		t.Fatal("state file remains")
	}
}
