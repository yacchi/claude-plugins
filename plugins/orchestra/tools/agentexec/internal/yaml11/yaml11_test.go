package yaml11

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
)

// TestPyYAMLTable replays testdata/yaml11/cases.json, generated from PyYAML
// itself (by a script removed at the v0.55.0 cutover; see git history), so
// every implicit-typing rule is checked against the real implementation
// rather than a reading of it. Frozen data: never edit by hand.
func TestPyYAMLTable(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "yaml11", "cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var table struct {
		Cases []struct {
			YAML string `json:"yaml"`
			Want any    `json:"want"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &table); err != nil {
		t.Fatal(err)
	}
	if len(table.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range table.Cases {
		got, err := Load([]byte(c.YAML))
		var gotCanon any
		if err != nil {
			gotCanon = map[string]any{"error": true}
		} else {
			gotCanon = canon(got)
		}
		// Round-trip through encoding/json so both sides share one shape.
		b, _ := json.Marshal(gotCanon)
		var norm any
		_ = json.Unmarshal(b, &norm)
		if !reflect.DeepEqual(norm, c.Want) {
			wb, _ := json.Marshal(c.Want)
			t.Errorf("Load(%q)\n got %s\nwant %s (err=%v)", c.YAML, b, wb, err)
		}
	}
}

func canon(v any) any {
	switch x := v.(type) {
	case nil, bool, string:
		return x
	case int64:
		return map[string]any{"int": strconv.FormatInt(x, 10)}
	case float64:
		return map[string]any{"float": pyRepr(x)}
	case Timestamp:
		if x.DateOnly {
			return map[string]any{"date": x.ISO()}
		}
		return map[string]any{"datetime": x.ISO()}
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = canon(e)
		}
		return out
	case *jsonx.Object:
		pairs := []any{}
		for _, k := range x.Keys() {
			e, _ := x.Get(k)
			pairs = append(pairs, []any{k, canon(e)})
		}
		return map[string]any{"map": pairs}
	}
	return map[string]any{"unexpected": true}
}

func pyRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	return jsonx.FormatFloat(f)
}
