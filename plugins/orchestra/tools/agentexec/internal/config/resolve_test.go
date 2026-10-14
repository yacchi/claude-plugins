package config

import "testing"

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
