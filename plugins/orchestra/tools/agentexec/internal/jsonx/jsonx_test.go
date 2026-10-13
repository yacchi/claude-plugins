package jsonx

import (
	"math"
	"testing"
)

func mustDecode(t *testing.T, s string) *Object {
	t.Helper()
	v, err := Decode([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return v.(*Object)
}

func TestDeepMerge(t *testing.T) {
	cases := []struct{ name, base, over, want string }{
		{"objects recurse", `{"a": {"x": 1, "y": 2}}`, `{"a": {"y": 3, "z": 4}}`, `{"a": {"x": 1, "y": 3, "z": 4}}`},
		{"lists replace", `{"a": [1, 2]}`, `{"a": [3]}`, `{"a": [3]}`},
		{"null replaces", `{"a": {"x": 1}}`, `{"a": null}`, `{"a": null}`},
		{"scalar over object", `{"a": {"x": 1}}`, `{"a": 5}`, `{"a": 5}`},
		{"object over scalar", `{"a": 5}`, `{"a": {"x": 1}}`, `{"a": {"x": 1}}`},
		{"new keys append, old keep position", `{"a": 1, "b": 2}`, `{"c": 3, "a": 9}`, `{"a": 9, "b": 2, "c": 3}`},
	}
	for _, c := range cases {
		base := mustDecode(t, c.base)
		got, _ := Marshal(DeepMerge(base, mustDecode(t, c.over)))
		if string(got) != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
		if again, _ := Marshal(base); string(again) != mustMarshal(t, mustDecode(t, c.base)) {
			t.Errorf("%s: base was modified: %s", c.name, again)
		}
	}
}

func mustMarshal(t *testing.T, v any) string {
	b, err := Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The expected strings are what Python's json.dumps(v, ensure_ascii=False)
// (and indent=2) print for the same values.
func TestMarshalMatchesPython(t *testing.T) {
	o := NewObject().
		Set("s", "é\"\\\n\t\x01<>& ").
		Set("f", []any{1.0, 0.5, 1e-05, 0.0001, 1e16, 1e15, math.Copysign(0, -1), math.Inf(1)}).
		Set("i", int64(-3)).
		Set("e", []any{}).
		Set("o", NewObject()).
		Set("n", nil)
	want := `{"s": "é\"\\\n\t\u0001<>&` + " " + `", "f": [1.0, 0.5, 1e-05, 0.0001, 1e+16, 1000000000000000.0, -0.0, Infinity], "i": -3, "e": [], "o": {}, "n": null}`
	if got := mustMarshal(t, o); got != want {
		t.Errorf("Marshal\n got %s\nwant %s", got, want)
	}
	nested := NewObject().Set("a", []any{int64(1), NewObject().Set("b", "x, y: z")}).Set("c", []any{})
	wantIndent := "{\n  \"a\": [\n    1,\n    {\n      \"b\": \"x, y: z\"\n    }\n  ],\n  \"c\": []\n}"
	got, err := MarshalIndent(nested)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != wantIndent {
		t.Errorf("MarshalIndent\n got %s\nwant %s", got, wantIndent)
	}
}
