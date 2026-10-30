package doctor

import (
	"strings"
	"testing"
)

// The verdict is in doctor --json only when config resolved: without a
// resolved config the key is absent, not null.
func TestObjectPnpmGVSKeyOnlyWhenConfigResolved(t *testing.T) {
	if _, ok := (Report{}).Object()["pnpm_gvs"]; ok {
		t.Error("pnpm_gvs present without a resolved config")
	}
	r := Report{PnpmGVS: map[string]any{"setting": "auto", "on": true, "reason": "auto: pnpm 11.0.9 (11 or later)"}}
	v, ok := r.Object()["pnpm_gvs"].(map[string]any)
	if !ok || v["on"] != true || v["setting"] != "auto" {
		t.Errorf("pnpm_gvs = %v, want the verdict map", r.Object()["pnpm_gvs"])
	}
}

func TestFormatTextPnpmGVSLine(t *testing.T) {
	r := Report{PnpmGVS: map[string]any{"setting": "auto", "on": true, "reason": "auto: pnpm 11.0.9 (11 or later)"}}
	want := "pnpm global virtual store: on (auto: pnpm 11.0.9 (11 or later))\n"
	if got := FormatText(r); !strings.Contains(got, want) {
		t.Errorf("FormatText lacks %q", want)
	}
	if got := FormatText(Report{}); strings.Contains(got, "pnpm global virtual store") {
		t.Error("the pnpm line is printed without a verdict")
	}
}
