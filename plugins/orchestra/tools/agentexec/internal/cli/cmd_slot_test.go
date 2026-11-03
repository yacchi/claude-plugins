package cli

import (
	"encoding/json"
	"testing"
)

// slotClassifyRun runs `slot classify` with args and returns its stdout and
// exit code.
func slotClassifyRun(t *testing.T, args ...string) (string, int) {
	t.Helper()
	hookCmdIsolate(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	return hookCmdRun(t, "", append([]string{"slot", "classify"}, args...)...)
}

// slotClassifyKeys is the complete key set of a classify line; nothing that
// would tell the hooks how to rewrite the command may appear in it.
var slotClassifyKeys = []string{"op", "heavy", "kind", "pool", "scope"}

func TestSlotClassifyPrintsOnlyTheFiveKeys(t *testing.T) {
	for _, command := range []string{"pnpm test", "go test ./internal/x/...", "ls"} {
		out, code := slotClassifyRun(t, "--command", command)
		if code != 0 {
			t.Fatalf("slot classify %q: exit %d", command, code)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(out), &m); err != nil {
			t.Fatalf("slot classify %q: output %q: %v", command, out, err)
		}
		if len(m) != len(slotClassifyKeys) {
			t.Errorf("slot classify %q: keys = %v, want exactly %v", command, m, slotClassifyKeys)
		}
		for _, key := range slotClassifyKeys {
			if _, ok := m[key]; !ok {
				t.Errorf("slot classify %q: missing key %q: %v", command, key, m)
			}
		}
	}
}

func TestSlotClassifyHeavyNodeLine(t *testing.T) {
	out, code := slotClassifyRun(t, "--command", "pnpm test")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatal(err)
	}
	if m["heavy"] != true || m["kind"] != "test" {
		t.Errorf("pnpm test: heavy = %v, kind = %v; want true, test", m["heavy"], m["kind"])
	}
}

func TestSlotClassifyLightCommandHasNullKind(t *testing.T) {
	out, code := slotClassifyRun(t, "--command", "ls")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatal(err)
	}
	if m["heavy"] != false {
		t.Errorf("heavy = %v, want false", m["heavy"])
	}
	if m["kind"] != nil {
		t.Errorf("kind = %v, want null", m["kind"])
	}
}

func TestSlotClassifyRejectsCwd(t *testing.T) {
	for _, args := range [][]string{
		{"--command", "go test ./...", "--cwd", "/tmp"},
		{"--command", "go test ./...", "--cwd", ""},
	} {
		if _, code := slotClassifyRun(t, args...); code != 2 {
			t.Errorf("slot classify %v: exit %d, want 2", args, code)
		}
	}
}
