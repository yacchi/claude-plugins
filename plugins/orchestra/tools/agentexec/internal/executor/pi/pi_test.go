package pi

import (
	"encoding/json"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
)

// parseFixtures is testdata/parse_fixtures.json: the retired Python
// parse_pi_jsonl over hand-made and fakepi transcripts (recorded by a
// generator removed at the v0.55.0 cutover; see git history). Frozen
// regression data: never edit by hand.
type parseFixtures struct {
	Cases []struct {
		Name     string          `json:"name"`
		Stdout   string          `json:"stdout"`
		Stderr   string          `json:"stderr"`
		ExitCode int             `json:"exit_code"`
		Resumed  bool            `json:"resumed"`
		Want     json.RawMessage `json:"want"`
	} `json:"cases"`
}

func plain(t *testing.T, v any) any {
	t.Helper()
	raw, err := fsutil.MarshalJSON(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestParseJSONLFixtures(t *testing.T) {
	raw, err := os.ReadFile("testdata/parse_fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx parseFixtures
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if len(fx.Cases) == 0 {
		t.Fatal("no fixtures")
	}
	for _, c := range fx.Cases {
		t.Run(c.Name, func(t *testing.T) {
			got := ParseJSONL(c.Stdout, c.Stderr, c.ExitCode, c.Resumed)
			var want any
			if err := json.Unmarshal(c.Want, &want); err != nil {
				t.Fatal(err)
			}
			if g := plain(t, got); !reflect.DeepEqual(g, want) {
				t.Errorf("got  %v\nwant %v", g, want)
			}
			// Key order is part of the output contract.
			wantObj, _ := fsutil.DecodeJSON(c.Want)
			if k, w := slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(wantObj.(map[string]any))); !slices.Equal(k, w) {
				t.Errorf("keys %v, want %v", k, w)
			}
		})
	}
}

func TestBuildArgv(t *testing.T) {
	const secret = "SECRET-PROMPT-BODY"
	fresh := BuildArgv("pi", "openai-codex/gpt-5.6-luna", "medium", "/w", secret, "", "json", true)
	want := []string{"pi", "-p", "--mode", "json", "--no-approve", "--no-extensions", "--no-skills",
		"--no-prompt-templates", "--no-themes", "--model", "openai-codex/gpt-5.6-luna", "--thinking", "medium"}
	if !slices.Equal(fresh, want) {
		t.Fatalf("fresh argv %v", fresh)
	}
	resumed := BuildArgv("/bin/pi", "", "", "/w", secret, "sid-1", "json", false)
	if strings.Contains(strings.Join(resumed, " "), secret) {
		t.Fatal("prompt leaked into argv")
	}
	want = []string{"/bin/pi", "-p", "--mode", "json", "--no-approve", "--no-extensions", "--no-skills",
		"--no-prompt-templates", "--no-themes", "--no-context-files", "--session", "sid-1"}
	if !slices.Equal(resumed, want) {
		t.Fatalf("resumed argv %v", resumed)
	}
}

func TestVeryLongRecordIsParsed(t *testing.T) {
	long := strings.Repeat("y", 5<<20)
	stdout := `{"type": "session", "id": "s"}` + "\n" +
		`{"type": "tool_execution_end", "result": {"content": [{"type": "text", "text": "` + long + `"}]}}` + "\n" +
		`{"type": "message_end", "message": {"role": "assistant", "content": [{"type": "text", "text": "ok"}], "stopReason": "stop"}}` + "\n"
	got := ParseJSONL(stdout, "", 0, false)
	if s, _ := got["status"]; s != "ok" {
		t.Fatalf("status %v", s)
	}
}
