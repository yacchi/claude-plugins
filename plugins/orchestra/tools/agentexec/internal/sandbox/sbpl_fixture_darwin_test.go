//go:build darwin

package sandbox

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestWrapArgvMatchesPython: the argv a dispatch would spawn -- above all
// the SBPL profile and its -D params -- is byte-identical to what the
// Python implementation produced for the same layout and config. The
// fixtures come from gen_fixtures.py; a difference here is a sandbox that
// enforces something else than the reviewed one.
func TestWrapArgvMatchesPython(t *testing.T) {
	f := loadFixtures(t)
	if len(f.Cases) == 0 {
		t.Fatal("no fixture cases")
	}
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			spec, argv, settings, path := replayCase(t, f, c)
			w := c.Want
			if spec.Backend != w.Backend || spec.Reason != w.Reason || spec.Refuse != w.Refuse {
				t.Errorf("spec: backend %q reason %q refuse %v\n want backend %q reason %q refuse %v",
					spec.Backend, spec.Reason, spec.Refuse, w.Backend, w.Reason, w.Refuse)
			}
			if !reflect.DeepEqual(nonNil(spec.Limits), nonNil(w.Limits)) {
				t.Errorf("limits %q, want %q", spec.Limits, w.Limits)
			}
			if len(argv) != len(w.Argv) {
				t.Fatalf("argv has %d elements, want %d\n got %q\nwant %q", len(argv), len(w.Argv), argv, w.Argv)
			}
			for i := range argv {
				if argv[i] != w.Argv[i] {
					t.Errorf("argv[%d]:\n got %s\nwant %s", i, argv[i], w.Argv[i])
				}
			}
			if w.SrtSettings != nil || settings != nil {
				gb, _ := json.Marshal(settings)
				wb, _ := json.Marshal(w.SrtSettings)
				if string(gb) != string(wb) {
					t.Errorf("srt settings:\n got %s\nwant %s", gb, wb)
				}
			}
			if path != w.SrtSettingsPath {
				t.Errorf("srt settings path %q, want %q", path, w.SrtSettingsPath)
			}
			if w.Backend == "seatbelt" && !strings.HasPrefix(argv[0], SandboxExec) {
				t.Errorf("argv[0] %q", argv[0])
			}
		})
	}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
