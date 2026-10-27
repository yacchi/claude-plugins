package isolate

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// lineageLink makes child's parent marker name parent.
func lineageLink(t *testing.T, child, parent string) {
	t.Helper()
	if err := WriteParent(child, parent); err != nil {
		t.Fatal(err)
	}
}

// lineageTasks are the tasks of children in listing order.
func lineageTasks(children []Child) []string {
	tasks := []string{}
	for _, c := range children {
		tasks = append(tasks, c.Task)
	}
	return tasks
}

func TestLineageChildrenAndDepth(t *testing.T) {
	repo := newRepo(t)
	asm := create(t, repo, "asm", "")
	integ := create(t, repo, "integ", "")
	work := create(t, repo, "work", "")
	if err := WriteRole(asm, RoleAssembly); err != nil {
		t.Fatal(err)
	}
	if err := WriteRole(integ, RoleIntegration); err != nil {
		t.Fatal(err)
	}
	lineageLink(t, integ, asm)
	lineageLink(t, work, integ)

	got, err := Children(repo, asm)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(lineageTasks(got), []string{"integ"}) {
		t.Fatalf("children of assembly = %+v, want only integ", got)
	}
	if paths.RealPath(got[0].Path) != paths.RealPath(integ) {
		t.Fatalf("child path = %s, want %s", got[0].Path, integ)
	}

	got, err = Children(repo, integ)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(lineageTasks(got), []string{"work"}) {
		t.Fatalf("children of integration = %+v, want only work", got)
	}

	got, err = Children(repo, work)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("children of leaf = %#v, want empty non-nil", got)
	}

	for tree, want := range map[string]int{asm: 0, integ: 1, work: 2} {
		if d := lineageDepth(tree); d != want {
			t.Errorf("lineageDepth(%s) = %d, want %d", tree, d, want)
		}
	}
	if RoleName(asm) != "assembly" || RoleName(work) != "work" {
		t.Errorf("RoleName assembly=%q work=%q", RoleName(asm), RoleName(work))
	}
}

func TestLineageSymlinkedParent(t *testing.T) {
	repo := newRepo(t)
	asm := create(t, repo, "asm", "")
	work := create(t, repo, "work", "")
	link := filepath.Join(t.TempDir(), "asm-link")
	if err := os.Symlink(asm, link); err != nil {
		t.Fatal(err)
	}

	lineageLink(t, work, link)
	if got := ReadParent(work); got != paths.RealPath(asm) {
		t.Fatalf("ReadParent = %q, want real path %q", got, paths.RealPath(asm))
	}
	got, err := Children(repo, link)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(lineageTasks(got), []string{"work"}) {
		t.Fatalf("children through symlink = %+v, want only work", got)
	}
}

func TestLineageAdoptedChildListedByAdoptionTask(t *testing.T) {
	repo := newRepo(t)
	asm := create(t, repo, "asm", "")
	ext := filepath.Join(t.TempDir(), "ext")
	git(t, repo, "worktree", "add", "-q", "-b", "external-branch", ext)
	if err := WriteAdopted(ext, "adopted-task"); err != nil {
		t.Fatal(err)
	}
	lineageLink(t, ext, asm)

	got, err := Children(repo, asm)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(lineageTasks(got), []string{"adopted-task"}) {
		t.Fatalf("children = %+v, want the adopted task", got)
	}
}

func TestLineageDeletedChildNotListed(t *testing.T) {
	repo := newRepo(t)
	asm := create(t, repo, "asm", "")
	work := create(t, repo, "work", "")
	lineageLink(t, work, asm)
	if err := os.RemoveAll(work); err != nil {
		t.Fatal(err)
	}

	got, err := Children(repo, asm)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("children after the directory was deleted = %+v, want none", got)
	}
}

func TestLineageClearAdoptedUnlinksChild(t *testing.T) {
	repo := newRepo(t)
	asm := create(t, repo, "asm", "")
	work := create(t, repo, "work", "")
	lineageLink(t, work, asm)

	if err := ClearAdopted(work); err != nil {
		t.Fatal(err)
	}
	if ReadParent(work) != "" {
		t.Fatalf("ReadParent after ClearAdopted = %q, want none", ReadParent(work))
	}
	got, err := Children(repo, asm)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("children after ClearAdopted = %+v, want none", got)
	}
}

func TestLineageIntegrationTreeFor(t *testing.T) {
	repo := newRepo(t)
	integ := create(t, repo, "integ", "")
	create(t, repo, "work", "")
	if err := WriteRole(integ, RoleIntegration); err != nil {
		t.Fatal(err)
	}

	for branch, want := range map[string]string{
		"refs/heads/orchestra/integ": integ,
		"orchestra/integ":            integ,
		"orchestra/work":             "",
		"main":                       "",
		"":                           "",
	} {
		got := IntegrationTreeFor(repo, branch)
		if want == "" && got != "" {
			t.Errorf("IntegrationTreeFor(%q) = %q, want none", branch, got)
		}
		if want != "" && paths.RealPath(got) != paths.RealPath(want) {
			t.Errorf("IntegrationTreeFor(%q) = %q, want %q", branch, got, want)
		}
	}
}

func TestLineageCycleIsBounded(t *testing.T) {
	repo := newRepo(t)
	asm := create(t, repo, "asm", "")
	work := create(t, repo, "work", "")
	lineageLink(t, asm, work)
	lineageLink(t, work, asm)

	if d := lineageDepth(asm); d > maxLineageDepth {
		t.Fatalf("lineageDepth over a cycle = %d, want at most %d", d, maxLineageDepth)
	}
}

func TestLineageAssemblyIgnoredNotDigested(t *testing.T) {
	repo := newRepo(t)
	wt := create(t, repo, "asm", "")
	writeFile(t, filepath.Join(wt, "node_modules", "pkg", "index.js"), "x\n")

	if !slices.Contains(ignoredForDigest(wt), "node_modules/") {
		t.Fatalf("work tree digest should include its ignored entries, got %v", ignoredForDigest(wt))
	}
	if err := WriteRole(wt, RoleAssembly); err != nil {
		t.Fatal(err)
	}
	if got := ignoredForDigest(wt); got != nil {
		t.Fatalf("assembly digest ignored entries = %v, want nil", got)
	}
}
