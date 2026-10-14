package conformance

import (
	"encoding/json"
	"reflect"
	"testing"
)

// roundTrip is the value compare sees for one side.
func roundTrip(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	must(t, err)
	var out any
	must(t, json.Unmarshal(b, &out))
	return out
}

func TestContentIsSemantic(t *testing.T) {
	n := &normalizer{numbered: map[string]map[string]string{}, keys: map[string]bool{"build": true, "pid": true}}
	shape := func(s string) (string, any) {
		sh, v := n.content(s)
		return sh, roundTrip(t, v)
	}

	// Key order, indentation and number spelling do not matter.
	aShape, a := shape("{\n  \"b\": 1.0,\n  \"a\": [1e0, \"x\"]\n}\n")
	bShape, b := shape(`{"a":[1,"x"],"b":1}`)
	if aShape != "json" || bShape != "json" || !reflect.DeepEqual(a, b) {
		t.Errorf("indented vs compact: %s %v / %s %v", aShape, a, bShape, b)
	}

	// Presence and null vs absent do.
	_, withNull := shape(`{"a":1,"b":null}`)
	_, without := shape(`{"a":1}`)
	if reflect.DeepEqual(withNull, without) {
		t.Error("null and absent compared equal")
	}

	// JSON lines stay lines.
	if sh, _ := n.content("{\"a\":1}\n{\"a\":2}\n"); sh != "jsonl" {
		t.Errorf("jsonl shape = %s", sh)
	}

	// A masked object keeps its keys; a masked null stays null at the top.
	_, got := shape(`{"build":{"go":"go1.26","source_hash":null},"pid":null}`)
	want := map[string]any{
		"build": map[string]any{"go": "<BUILD>", "source_hash": "<BUILD>"},
		"pid":   nil,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("masking: got %v, want %v", got, want)
	}
}
