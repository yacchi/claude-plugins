package isolate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// createDirTemp is a realpath'd temp dir, so paths compare as git reports them.
func createDirTemp(t *testing.T) string {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return tmp
}

func TestCreateDirMakesMissingParents(t *testing.T) {
	repo := newRepo(t)
	dir := filepath.Join(createDirTemp(t), "ws", "repo")

	res, err := Create(repo, "x", CreateOptions{Backend: BackendAuto, Dir: dir})
	if err != nil || res.Status != "created" {
		t.Fatalf("create at %s: %+v, %v", dir, res, err)
	}
	if paths.RealPath(res.Path) != paths.RealPath(dir) {
		t.Fatalf("tree at %s, want %s", res.Path, dir)
	}
	if res.Backend != BackendGit || res.Branch != "orchestra/x" {
		t.Fatalf("backend %s branch %s, want git orchestra/x", res.Backend, res.Branch)
	}
	if readMarker(res.Path, "orchestra-baseline") == "" {
		t.Fatal("baseline marker missing")
	}
}

func TestCreateDirUsesEmptyDirectory(t *testing.T) {
	repo := newRepo(t)
	dir := filepath.Join(createDirTemp(t), "empty")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := Create(repo, "x", CreateOptions{Dir: dir})
	if err != nil || res.Status != "created" {
		t.Fatalf("create in empty %s: %+v, %v", dir, res, err)
	}
	if paths.RealPath(res.Path) != paths.RealPath(dir) {
		t.Fatalf("tree at %s, want %s", res.Path, dir)
	}
}

func TestCreateDirRefusesNonEmptyDirectory(t *testing.T) {
	repo := newRepo(t)
	dir := filepath.Join(createDirTemp(t), "busy")
	writeFile(t, filepath.Join(dir, "f.txt"), "mine\n")

	res, err := Create(repo, "x", CreateOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "error" || res.Reason != dir+" exists and is not an empty directory" {
		t.Fatalf("want refusal, got %+v", res)
	}
	if got := branches(t, repo); got != "main" {
		t.Fatalf("refusal left branches %q, want only main", got)
	}
	if got := readFile(t, filepath.Join(dir, "f.txt")); got != "mine\n" {
		t.Fatalf("f.txt changed to %q", got)
	}
}

func TestCreateDirRefusesFile(t *testing.T) {
	repo := newRepo(t)
	file := filepath.Join(createDirTemp(t), "plain")
	writeFile(t, file, "not a dir\n")

	res, err := Create(repo, "x", CreateOptions{Dir: file})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "error" {
		t.Fatalf("want refusal for a file, got %+v", res)
	}
	if got := branches(t, repo); got != "main" {
		t.Fatalf("refusal left branches %q, want only main", got)
	}
}

func TestCreateDirReusedTaskReturnsExistingPath(t *testing.T) {
	repo := newRepo(t)
	first := filepath.Join(createDirTemp(t), "first")
	second := filepath.Join(createDirTemp(t), "second")
	if res, err := Create(repo, "x", CreateOptions{Dir: first}); err != nil || res.Status != "created" {
		t.Fatalf("first create: %+v, %v", res, err)
	}

	res, err := Create(repo, "x", CreateOptions{Dir: second})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "exists" || paths.RealPath(res.Path) != paths.RealPath(first) {
		t.Fatalf("want exists at %s, got %+v", first, res)
	}
	if exists(second) {
		t.Fatal("second Dir was created despite the existing tree")
	}
}

func TestCreateDirCombinesWithRole(t *testing.T) {
	repo := newRepo(t)
	dir := filepath.Join(createDirTemp(t), "assembly")

	res, err := Create(repo, "y", CreateOptions{Onto: "HEAD", Role: RoleAssembly, Dir: dir})
	if err != nil || res.Status != "created" {
		t.Fatalf("create assembly: %+v, %v", res, err)
	}
	if paths.RealPath(res.Path) != paths.RealPath(dir) || res.Role != RoleAssembly {
		t.Fatalf("want assembly at %s, got %+v", dir, res)
	}
}
