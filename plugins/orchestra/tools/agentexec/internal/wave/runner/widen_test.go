package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/plan"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/state"
)

// A mark --widen lands while the run is live: the next context file must list
// the widened paths and carry the mark's detail, or a reused session keeps
// escalating on the old files_owned.
func TestWidenReachesTheNextContextFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "context"), 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "state.json")
	store, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Init("plan.md", []string{"A"}, ""); err != nil {
		t.Fatal(err)
	}
	r := &runner{
		store: store, stateDir: dir, notes: map[string]string{}, widenNotes: map[string]string{},
		plan: &plan.Plan{Packages: []plan.Package{{ID: "A", FilesOwned: []string{"a.txt"}}}},
	}
	if _, code := Mark(MarkOptions{State: statePath, Pkg: "A", Widen: []string{"src/*.py"}, Status: "pending", Detail: "also fix src"}); code != 0 {
		t.Fatalf("mark exit %d", code)
	}
	r.noteWidening("A", r.applyOverrides()["A"])
	path, err := r.writeContext("A", "")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	for _, want := range []string{"- src/*.py", "you now also own: src/*.py", "also fix src"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("context lacks %q:\n%s", want, raw)
		}
	}
	r.noteWidening("A", r.applyOverrides()["A"])
	path, _ = r.writeContext("A", "")
	raw, _ = os.ReadFile(path)
	if strings.Contains(string(raw), "you now also own") || !strings.Contains(string(raw), "- src/*.py") {
		t.Errorf("a later dispatch must keep the path but drop the note:\n%s", raw)
	}
}
