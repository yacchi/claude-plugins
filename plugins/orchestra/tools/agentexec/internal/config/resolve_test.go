package config

import "testing"

// isolation.land is "auto" or "ask", exactly; anything else is a config error.
func TestLandAcceptsOnlyAutoOrAsk(t *testing.T) {
	if got := ObjectAt(Defaults(), "isolation")["land"]; got != "auto" {
		t.Errorf("default land = %#v, want \"auto\"", got)
	}
	values := Defaults()
	for _, good := range []any{"auto", "ask"} {
		ObjectAt(values, "isolation")["land"] = good
		if err := validateLand(values); err != nil {
			t.Errorf("%#v rejected: %v", good, err)
		}
	}
	for _, bad := range []any{"never", "Ask", " ask", true, nil} {
		ObjectAt(values, "isolation")["land"] = bad
		if err := validateLand(values); err == nil {
			t.Errorf("%#v accepted, want an error", bad)
		}
	}
	ObjectAt(values, "isolation")["land"] = "never"
	want := `invalid isolation.land never: must be "auto" or "ask"`
	if err := validateLand(values); err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
}

// Unquoted dates load as the text the author wrote, not as RFC 3339.
func TestLoadYAMLKeepsTimestampsAsText(t *testing.T) {
	v, err := loadYAML([]byte("since: 2024-01-01\nlist: [2024-01-02]\nquoted: \"2024-01-03\"\nstamp: 2024-01-01T10:00:00Z\n"))
	if err != nil {
		t.Fatal(err)
	}
	m := v.(map[string]any)
	if m["since"] != "2024-01-01" || m["quoted"] != "2024-01-03" || m["stamp"] != "2024-01-01T10:00:00Z" {
		t.Errorf("got %#v", m)
	}
	if l := m["list"].([]any); l[0] != "2024-01-02" {
		t.Errorf("list = %#v", l)
	}
}
