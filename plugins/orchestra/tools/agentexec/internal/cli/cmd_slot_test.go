package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/sandbox"
)

// slotGuardClassify runs `slot classify` on command and decodes its one
// JSON line.
func slotGuardClassify(t *testing.T, command string) map[string]any {
	t.Helper()
	hookCmdIsolate(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	out, code := hookCmdRun(t, "", "slot", "classify", "--command", command)
	if code != 0 {
		t.Fatalf("slot classify %q: exit %d", command, code)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("slot classify %q: output %q: %v", command, out, err)
	}
	return m
}

func TestSlotGuardHeavyCommandGetsTokenAndPrefix(t *testing.T) {
	out := slotGuardClassify(t, "go test ./internal/x/...")
	if out["heavy"] != true {
		t.Fatalf("heavy = %v, want true", out["heavy"])
	}
	token, ok := out["guard_token"].(string)
	if !ok || !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(token) {
		t.Fatalf("guard_token = %v, want 16 lowercase hex characters", out["guard_token"])
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		t.Fatal(err)
	}
	want := sandbox.ShellQuote(exe) + " proc guard --root $$ --token " + token + " </dev/null >/dev/null 2>&1; "
	if out["guard_prefix"] != want {
		t.Errorf("guard_prefix = %v, want %q", out["guard_prefix"], want)
	}
	capped, _ := out["capped_command"].(string)
	if strings.Contains(capped, "proc guard") || strings.Contains(capped, want) {
		t.Errorf("capped_command carries the guard prefix: %q", capped)
	}
}

func TestSlotGuardTokensDiffer(t *testing.T) {
	a := slotGuardClassify(t, "go test ./internal/x/...")
	b := slotGuardClassify(t, "go test ./internal/x/...")
	ta, _ := a["guard_token"].(string)
	tb, _ := b["guard_token"].(string)
	if ta == "" || tb == "" {
		t.Fatalf("missing guard_token: %v, %v", a["guard_token"], b["guard_token"])
	}
	if ta == tb {
		t.Errorf("two classify calls share guard_token %q", ta)
	}
	if pa, _ := a["guard_prefix"].(string); !strings.Contains(pa, " --token "+ta+" ") {
		t.Errorf("first guard_prefix %q does not carry its token %q", pa, ta)
	}
	if pb, _ := b["guard_prefix"].(string); !strings.Contains(pb, " --token "+tb+" ") {
		t.Errorf("second guard_prefix %q does not carry its token %q", pb, tb)
	}
}

func TestSlotGuardLightCommandHasNoKeys(t *testing.T) {
	out := slotGuardClassify(t, "ls")
	if out["heavy"] != false {
		t.Fatalf("heavy = %v, want false", out["heavy"])
	}
	for _, key := range []string{"guard_token", "guard_prefix"} {
		if _, ok := out[key]; ok {
			t.Errorf("light command has key %q: %v", key, out[key])
		}
	}
}
