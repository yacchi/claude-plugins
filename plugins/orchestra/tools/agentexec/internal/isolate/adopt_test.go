package isolate

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// adoptExternalTree adds a worktree orchestra did not create, as an external
// app would, and returns its path.
func adoptExternalTree(t *testing.T, repo, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	git(t, repo, "worktree", "add", "-q", "-b", name, dir)
	return dir
}

// adoptJSON is res as the CLI prints it, decoded.
func adoptJSON(t *testing.T, res AdoptResult) map[string]any {
	t.Helper()
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestAdoptRecordsParentUnderIntegrationTree(t *testing.T) {
	repo := newRepo(t)
	integ := plantTree(t, repo, "X", RoleIntegration, "")
	ext := adoptExternalTree(t, repo, "ext")

	res, err := Adopt(repo, "pkg-a", ext, AdoptOptions{Parent: integ})
	if err != nil || res.Status != "adopted" {
		t.Fatalf("adopt: %+v, %v", res, err)
	}
	if got := ReadParent(ext); got != paths.RealPath(integ) {
		t.Fatalf("ReadParent = %q, want %q", got, paths.RealPath(integ))
	}
	if got := adoptJSON(t, res)["parent"]; got != paths.RealPath(integ) {
		t.Fatalf("JSON parent = %v, want %q", got, paths.RealPath(integ))
	}
}

func TestAdoptRefusesParentThatIsNotIntegrationTree(t *testing.T) {
	repo := newRepo(t)
	work := plantTree(t, repo, "wk", "", "")
	for i, parent := range []string{work, repo, filepath.Join(t.TempDir(), "gone")} {
		ext := adoptExternalTree(t, repo, fmt.Sprintf("ext%d", i))

		res, err := Adopt(repo, "pkg-a", ext, AdoptOptions{Parent: parent})
		if err != nil {
			t.Fatalf("adopt under %s: %v", parent, err)
		}
		if res.Status != "error" || res.Note != "parent "+parent+" is not an integration tree" {
			t.Fatalf("adopt under %s: status %q note %q", parent, res.Status, res.Note)
		}
		if _, ok := readAdoption(ext); ok {
			t.Errorf("refused adopt under %s left an adoption marker", parent)
		}
		if got := ReadParent(ext); got != "" {
			t.Errorf("refused adopt under %s left parent marker %q", parent, got)
		}
	}
}

func TestAdoptReAdoptKeepsOrReplacesParent(t *testing.T) {
	repo := newRepo(t)
	first := plantTree(t, repo, "X", RoleIntegration, "")
	second := plantTree(t, repo, "Y", RoleIntegration, "")
	ext := adoptExternalTree(t, repo, "ext")
	if _, err := Adopt(repo, "pkg-a", ext, AdoptOptions{Parent: first}); err != nil {
		t.Fatal(err)
	}

	res, err := Adopt(repo, "pkg-a", ext, AdoptOptions{})
	if err != nil || res.Status != "exists" {
		t.Fatalf("re-adopt: %+v, %v", res, err)
	}
	if got := ReadParent(ext); got != paths.RealPath(first) {
		t.Fatalf("re-adopt without Parent changed parent to %q", got)
	}
	if got := adoptJSON(t, res)["parent"]; got != paths.RealPath(first) {
		t.Fatalf("re-adopt JSON parent = %v, want %q", got, paths.RealPath(first))
	}

	res, err = Adopt(repo, "pkg-a", ext, AdoptOptions{Parent: second})
	if err != nil || res.Status != "exists" {
		t.Fatalf("re-adopt under new parent: %+v, %v", res, err)
	}
	if got := ReadParent(ext); got != paths.RealPath(second) {
		t.Fatalf("ReadParent = %q, want %q", got, paths.RealPath(second))
	}
}

func TestAdoptUnadoptClearsParentMarker(t *testing.T) {
	repo := newRepo(t)
	integ := plantTree(t, repo, "X", RoleIntegration, "")
	ext := adoptExternalTree(t, repo, "ext")
	if _, err := Adopt(repo, "pkg-a", ext, AdoptOptions{Parent: integ}); err != nil {
		t.Fatal(err)
	}

	if res, err := Unadopt(repo, "pkg-a", ""); err != nil || res.Status != "unadopted" {
		t.Fatalf("unadopt: %+v, %v", res, err)
	}
	if got := ReadParent(ext); got != "" {
		t.Fatalf("ReadParent after unadopt = %q, want none", got)
	}
}
