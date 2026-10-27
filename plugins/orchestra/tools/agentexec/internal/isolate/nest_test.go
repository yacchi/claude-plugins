package isolate

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// plantTree creates task's tree from root (Backend git, no carry) with role and
// parent ("" = none), failing the test unless it was created; it returns the path.
// A tree with a role starts exactly at root's HEAD commit (Onto "HEAD"); a work
// tree takes Create's default (Onto "").
func plantTree(t *testing.T, root, task, role, parent string) string {
	t.Helper()
	o := CreateOptions{Backend: BackendGit, Role: role, Parent: parent}
	if role != "" {
		o.Onto = "HEAD"
	}
	res, err := Create(root, task, o)
	if err != nil || res.Status != "created" {
		t.Fatalf("plant %s: %+v, %v", task, res, err)
	}
	return res.Path
}

func TestCreateAssemblyStartsAtHeadWithoutBaselineCommit(t *testing.T) {
	repo := newRepo(t)
	head := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))
	res, err := Create(repo, "asm", CreateOptions{Backend: BackendGit, Onto: "HEAD", Role: RoleAssembly})
	if err != nil || res.Status != "created" {
		t.Fatalf("create: %+v, %v", res, err)
	}
	if got := ReadRole(res.Path); got != RoleAssembly {
		t.Errorf("role marker = %q, want %q", got, RoleAssembly)
	}
	if got := ReadParent(res.Path); got != "" {
		t.Errorf("parent marker = %q, want none", got)
	}
	if res.Baseline != head {
		t.Errorf("baseline = %s, want the repo HEAD %s", res.Baseline, head)
	}
	if log := git(t, res.Path, "log", "--format=%s"); strings.Contains(log, "orchestra baseline") {
		t.Errorf("assembly carries a baseline commit:\n%s", log)
	}
	m := jsonObject(t, res)
	if m["role"] != RoleAssembly || m["parent"] != nil {
		t.Errorf("JSON role/parent = %v/%v, want assembly/null", m["role"], m["parent"])
	}
}

func TestCreateAssemblyFromCheckoutCarriesUncommittedWork(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, repo+"/README.md", "edited\n")
	res, err := Create(repo, "asm2", CreateOptions{Backend: BackendGit, Role: RoleAssembly})
	if err != nil || res.Status != "created" {
		t.Fatalf("create: %+v, %v", res, err)
	}
	if log := git(t, res.Path, "log", "--format=%s"); !strings.Contains(log, "orchestra baseline for asm2") {
		t.Errorf("uncommitted work is not in a baseline commit:\n%s", log)
	}
	if got := readFile(t, res.Path+"/README.md"); got != "edited\n" {
		t.Errorf("README in the assembly = %q, want the uncommitted edit", got)
	}
}

func TestCreateRecordsRoleAndParentForChildren(t *testing.T) {
	repo := newRepo(t)
	asm := plantTree(t, repo, "asm", RoleAssembly, "")
	res, err := Create(asm, "feat", CreateOptions{Onto: "HEAD", Role: RoleIntegration, Parent: asm})
	if err != nil || res.Status != "created" {
		t.Fatalf("integration: %+v, %v", res, err)
	}
	if got := ReadParent(res.Path); got != paths.RealPath(asm) {
		t.Errorf("parent marker = %q, want %q", got, paths.RealPath(asm))
	}
	m := jsonObject(t, res)
	if m["role"] != RoleIntegration || m["parent"] != paths.RealPath(asm) {
		t.Errorf("JSON role/parent = %v/%v", m["role"], m["parent"])
	}

	work, err := Create(res.Path, "t1", CreateOptions{Parent: res.Path})
	if err != nil || work.Status != "created" {
		t.Fatalf("work tree: %+v, %v", work, err)
	}
	if m := jsonObject(t, work); m["role"] != "work" {
		t.Errorf("work tree JSON role = %v, want work", m["role"])
	}
}

func TestCreateRefusesInvalidLineageBeforeAnyGitWrite(t *testing.T) {
	repo := newRepo(t)
	asm := plantTree(t, repo, "asm", RoleAssembly, "")
	feat := plantTree(t, repo, "feat", RoleIntegration, asm)
	outside := t.TempDir()
	cases := []struct {
		name   string
		opts   CreateOptions
		reason string
	}{
		{"unknown role", CreateOptions{Role: "integratoin"}, "unknown role integratoin"},
		{"assembly with parent", CreateOptions{Role: RoleAssembly, Parent: asm}, "an assembly worktree has no parent"},
		{"work under a non-integration tree", CreateOptions{Parent: asm}, "parent " + asm + " is not an integration tree"},
		{"integration under an integration tree", CreateOptions{Role: RoleIntegration, Parent: feat}, "parent " + feat + " is not an assembly worktree"},
		{"parent outside the repository", CreateOptions{Parent: outside}, "parent " + outside + " is not a worktree of this repository"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.opts.Backend = BackendGit
			res, err := Create(repo, "x", c.opts)
			if err != nil {
				t.Fatalf("Create returned error %v", err)
			}
			if res.Status != "error" || res.Reason != c.reason {
				t.Fatalf("result = %+v, want error %q", res, c.reason)
			}
			if slices.Contains(strings.Fields(branches(t, repo)), "orchestra/x") {
				t.Errorf("a refused create left branch orchestra/x behind")
			}
		})
	}
}

func TestCreateValidatesParentEvenWhenTheTreeExists(t *testing.T) {
	repo := newRepo(t)
	asm := plantTree(t, repo, "asm", RoleAssembly, "")
	feat := plantTree(t, repo, "feat", RoleIntegration, asm)
	plantTree(t, repo, "t1", "", feat)

	res, err := Create(repo, "t1", CreateOptions{Backend: BackendGit, Parent: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "error" {
		t.Fatalf("invalid parent on an existing tree = %+v, want error", res)
	}
}

func TestCreateExistingTreeKeepsItsFirstParent(t *testing.T) {
	repo := newRepo(t)
	asm := plantTree(t, repo, "asm", RoleAssembly, "")
	first := plantTree(t, repo, "first", RoleIntegration, asm)
	second := plantTree(t, repo, "second", RoleIntegration, asm)
	work := plantTree(t, repo, "t1", "", first)

	res, err := Create(repo, "t1", CreateOptions{Backend: BackendGit, Parent: second})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "exists" {
		t.Fatalf("second create of t1 = %+v, want exists", res)
	}
	if got := ReadParent(work); got != paths.RealPath(first) {
		t.Errorf("parent marker = %q, want the first parent %q", got, paths.RealPath(first))
	}
	if res.Parent != paths.RealPath(first) {
		t.Errorf("exists result parent = %q, want the marker's %q", res.Parent, paths.RealPath(first))
	}
}

// jsonObject is res's JSON as a map.
func jsonObject(t *testing.T, res CreateResult) map[string]any {
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
