package isolate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSanitizeTaskID(t *testing.T) {
	for raw, want := range map[string]string{
		"fix-login_2.0":     "fix-login_2.0",
		"../../etc/passwd":  "etc-passwd",
		"a b\tc;$(rm -rf)":  "a-b-c-rm-rf",
		"feature/x\\y":      "feature-x-y",
		"..hidden.":         "hidden",
		"--lead--trail--":   "lead-trail",
		"ünïcode":           "n-code",
		"./a/./b/../c":      "a-b-c",
		"refs/heads/main~1": "refs-heads-main-1",
	} {
		got, err := SanitizeTaskID(raw)
		if err != nil || got != want {
			t.Errorf("SanitizeTaskID(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"", "///", "..", "./..", "$$$"} {
		if _, err := SanitizeTaskID(raw); err == nil {
			t.Errorf("SanitizeTaskID(%q) accepted", raw)
		}
	}
	_, err := SanitizeTaskID("///")
	if err == nil || err.Error() != `task id "///" has no usable characters` {
		t.Errorf("message: %v", err)
	}
}

func TestBranchNamespacesUnderAUsableSessionOnly(t *testing.T) {
	for _, c := range []struct{ task, session, want string }{
		{"t1", "", "orchestra/t1"},
		{"t1", "ABCDEF12-xyz", "orchestra/abcdef12/t1"},
		{"t1", "abc", "orchestra/t1"},          // too short
		{"t1", "abc/../x-yz", "orchestra/t1"},  // not alphanumeric
		{"t1", "../../../../", "orchestra/t1"}, // hostile
		{"a/../b c", sessA, "orchestra/aaaaaaaa/a-b-c"},
	} {
		if got := Branch(c.task, c.session); got != c.want {
			t.Errorf("Branch(%q, %q) = %q, want %q", c.task, c.session, got, c.want)
		}
	}
}

func TestDetectCarryDirs(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, filepath.Join(repo, ".gitignore"), "node_modules/\n.venv/\ncache-dir/\n")
	writeFile(t, filepath.Join(repo, "node_modules", "a", "node_modules", "b", "i.js"), "x")
	writeFile(t, filepath.Join(repo, "packages", "web", "node_modules", "i.js"), "x")
	writeFile(t, filepath.Join(repo, "vendor", "lib.go"), "tracked") // tracked: arrives anyway
	writeFile(t, filepath.Join(repo, "cache-dir", "x"), "x")
	writeFile(t, filepath.Join(repo, "a", "b", "c", "d", "node_modules", "x"), "too deep")
	git(t, repo, "add", "vendor", ".gitignore")
	got := DetectCarryDirs(repo, nil)
	want := []string{"node_modules", "packages/web/node_modules"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	got = DetectCarryDirs(repo, []string{"cache-dir"})
	if !slices.Contains(got, "cache-dir") {
		t.Fatalf("carry_extra not honored: %v", got)
	}
}

func TestCarryDir(t *testing.T) {
	srcRoot := t.TempDir()
	src := filepath.Join(srcRoot, "deps")
	writeFile(t, filepath.Join(src, "sub", "f.txt"), "content")
	if err := os.Symlink("sub/f.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	dstRoot := filepath.Join(t.TempDir(), "deeper", "dst")
	if err := CarryDir(srcRoot, dstRoot, "deps"); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dstRoot, "deps")
	if readFile(t, filepath.Join(dst, "sub", "f.txt")) != "content" {
		t.Fatal("content not copied")
	}
	if link, err := os.Readlink(filepath.Join(dst, "link")); err != nil || link != "sub/f.txt" {
		t.Fatalf("symlink not kept as a link: %q %v", link, err)
	}
	// A failed clone onto an existing directory must not wipe it.
	existingRoot := t.TempDir()
	writeFile(t, filepath.Join(existingRoot, "deps", "keep.txt"), "mine")
	_ = CarryDir(srcRoot, existingRoot, "deps")
	if readFile(t, filepath.Join(existingRoot, "deps", "keep.txt")) != "mine" {
		t.Fatal("an existing destination was clobbered")
	}
	if err := CarryDir(srcRoot, t.TempDir(), "missing"); err == nil {
		t.Fatal("a missing source was reported as copied")
	}
}

func TestCreateCarriesUncommittedWorkAndDependencies(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "hello\nwip\n")
	writeFile(t, filepath.Join(repo, "new.txt"), "untracked\n")
	writeFile(t, filepath.Join(repo, "node_modules", "dep", "i.js"), "dep\n")
	res, err := Create(repo, "t1", CreateOptions{Backend: BackendGit, Carry: true})
	if err != nil || res.Status != "created" {
		t.Fatalf("%+v %v", res, err)
	}
	if res.Path != filepath.Join(Home(repo), "t1") || strings.HasPrefix(res.Path, repo+string(os.PathSeparator)) {
		t.Fatalf("worktree at %s", res.Path)
	}
	if readFile(t, filepath.Join(res.Path, "README.md")) != "hello\nwip\n" ||
		readFile(t, filepath.Join(res.Path, "new.txt")) != "untracked\n" {
		t.Fatal("uncommitted work not carried")
	}
	if !slices.Equal(res.Carried, []string{"node_modules"}) || res.CarryMethod == carryNone ||
		!exists(filepath.Join(res.Path, "node_modules", "dep", "i.js")) {
		t.Fatalf("dependencies not carried: %+v", res)
	}
	// The carried state is the baseline: an untouched worktree diffs empty,
	// and the worker's edit never reaches the user's tree.
	d, err := Diff(repo, "t1", "", true)
	if err != nil || d.Status != "ok" || len(d.Files) != 0 || d.Patch != "" {
		t.Fatalf("fresh diff %+v %v", d, err)
	}
	writeFile(t, filepath.Join(res.Path, "README.md"), "worker\n")
	writeFile(t, filepath.Join(res.Path, "node_modules", "dep", "i.js"), "worker\n")
	writeFile(t, filepath.Join(res.Path, "added.txt"), "added\n")
	if readFile(t, filepath.Join(repo, "README.md")) != "hello\nwip\n" {
		t.Fatal("worker edit reached the user's tree")
	}
	d, _ = Diff(repo, "t1", "", true)
	if !slices.Equal(d.Files, []string{"README.md", "added.txt"}) || !strings.Contains(d.Patch, "+worker") {
		t.Fatalf("diff %+v", d)
	}
	again, _ := Create(repo, "t1", CreateOptions{Backend: BackendGit, Carry: true})
	if again.Status != "exists" || again.Path != res.Path {
		t.Fatalf("second create %+v", again)
	}
}

func TestCreateWithoutCarryCopiesNoDependenciesOrLocalFiles(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, filepath.Join(repo, "node_modules", "i.js"), "x")
	writeFile(t, filepath.Join(repo, "CLAUDE.local.md"), "local\n")
	res, _ := Create(repo, "t1", CreateOptions{Backend: BackendGit})
	if exists(filepath.Join(res.Path, "node_modules")) || res.CarryMethod != carryNone {
		t.Fatalf("dependencies carried: %+v", res)
	}
	if exists(filepath.Join(res.Path, "CLAUDE.local.md")) {
		t.Fatal("a local file was carried without carry")
	}
	deps := true
	res2, _ := Create(repo, "t2", CreateOptions{Backend: BackendGit, CarryDeps: &deps})
	if !slices.Equal(res2.Carried, []string{"node_modules"}) {
		t.Fatalf("CarryDeps did not override Carry: %+v", res2)
	}
}

func TestLocalFilesAreCarriedButNeverOverwrittenOrEscaping(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, filepath.Join(repo, ".gitignore"), ".claude/settings.local.json\nCLAUDE.local.md\n")
	git(t, repo, "commit", "-qam", "ignore local files")
	writeFile(t, filepath.Join(repo, ".claude", "settings.local.json"), "{}\n")
	outside := filepath.Join(filepath.Dir(repo), "secret.txt")
	writeFile(t, outside, "secret")
	res, _ := Create(repo, "t1", CreateOptions{
		Backend: BackendGit, Carry: true,
		CarryFiles: []string{".claude/settings.local.json", "../secret.txt", "CLAUDE.local.md"},
	})
	if !slices.Equal(res.CarriedFiles, []string{".claude/settings.local.json"}) {
		t.Fatalf("carried %v", res.CarriedFiles)
	}
	if exists(filepath.Join(filepath.Dir(res.Path), "secret.txt")) {
		t.Fatal("a ../ path escaped the worktree")
	}
	// Already present in the target: left as found.
	path := filepath.Join(res.Path, ".claude", "settings.local.json")
	writeFile(t, path, "worker's\n")
	if got := carryLocalFiles(repo, res.Path, []string{".claude/settings.local.json"}); len(got) != 0 ||
		readFile(t, path) != "worker's\n" {
		t.Fatalf("overwrote an existing file: %v", got)
	}
}

func TestListReportsOnlyOrchestraWorktreesWithTheirSession(t *testing.T) {
	repo := newRepo(t)
	create(t, repo, "t1", sessA)
	create(t, repo, "t1", sessB)
	create(t, repo, "legacy", "")
	git(t, repo, "worktree", "add", "-q", "-b", "feature/x", filepath.Join(filepath.Dir(repo), "other"))
	t.Setenv("CLAUDE_CODE_SESSION_ID", sessA)
	list, _ := List(repo, "")
	got := map[string]Entry{}
	for _, e := range list {
		got[e.Branch] = e
	}
	if len(list) != 3 {
		t.Fatalf("list %+v", list)
	}
	if e := got["orchestra/aaaaaaaa/t1"]; e.Session != "aaaaaaaa" || !e.Current || e.Task != "t1" {
		t.Errorf("own tree %+v", e)
	}
	if e := got["orchestra/bbbbbbbb/t1"]; e.Current {
		t.Errorf("other session's tree is current: %+v", e)
	}
	if e := got["orchestra/legacy"]; e.Session != "" || e.Current {
		t.Errorf("legacy tree %+v", e)
	}
	byFull, _ := List(repo, sessB)
	byShort, _ := List(repo, "bbbbbbbb")
	if len(byFull) != 1 || len(byShort) != 1 || !byFull[0].Current {
		t.Fatalf("session filter: %+v / %+v", byFull, byShort)
	}
}

func TestResolution(t *testing.T) {
	repo := newRepo(t)
	pathA := create(t, repo, "t1", sessA)
	create(t, repo, "t1", sessB)
	create(t, repo, "solo", sessB)
	legacy := create(t, repo, "old", "")

	if p, _, ok := ResolveWorktree(repo, "t1", sessA); !ok || p != pathA {
		t.Errorf("the current session's tree did not win: %q", p)
	}
	if _, _, err := resolveWorktree(repo, "t1", ""); err == nil ||
		err.Error() != "ambiguous worktrees for task t1: orchestra/aaaaaaaa/t1, orchestra/bbbbbbbb/t1" {
		t.Errorf("two cross-session matches: %v", err)
	}
	if _, _, ok := ResolveWorktree(repo, "t1", "cccccccc"); ok {
		t.Error("an ambiguous resolution returned a guess")
	}
	d, err := Diff(repo, "solo", sessA, false)
	if err != nil || d.Session != "bbbbbbbb" {
		t.Errorf("a unique cross-session match: %+v %v", d, err)
	}
	if p, _, ok := ResolveWorktree(repo, "old", sessA); !ok || p != legacy {
		t.Errorf("legacy tree not resolvable: %q", p)
	}
}

func TestRemoveGate(t *testing.T) {
	repo := newRepo(t)
	path := create(t, repo, "t1", "")
	if r := remove(t, repo, "nope", false, ""); r.Status != "absent" {
		t.Fatalf("unknown task: %+v", r)
	}
	writeFile(t, filepath.Join(path, "worker.txt"), "w\n")
	r := remove(t, repo, "t1", false, "")
	if r.Status != "dirty" || !slices.Equal(r.Files, []string{"worker.txt"}) ||
		r.Reason != "worktree holds 1 changed file(s) not yet collected; collect the diff or pass --force" {
		t.Fatalf("uncollected work: %+v", r)
	}
	// Collect, edit again: refused again. Revert to the collected content:
	// removable.
	if c, _ := Collect(repo, "t1", ""); c.Status != "collected" || len(c.Digest) != 64 {
		t.Fatalf("collect %+v", c)
	}
	writeFile(t, filepath.Join(path, "worker.txt"), "w2\n")
	if r := remove(t, repo, "t1", false, ""); r.Status != "dirty" {
		t.Fatalf("edit after collection: %+v", r)
	}
	writeFile(t, filepath.Join(path, "worker.txt"), "w\n")
	if r := remove(t, repo, "t1", false, ""); r.Status != "removed" || exists(path) {
		t.Fatalf("collected content: %+v", r)
	}
	if strings.Contains(branches(t, repo), "orchestra/t1") {
		t.Fatal("branch survived")
	}
}

func TestRemoveMarkerEdgeCases(t *testing.T) {
	repo := newRepo(t)
	// Untouched, even with a stale or corrupt collected marker: removable.
	p1 := create(t, repo, "t1", "")
	_ = WriteCollected(p1, "stale")
	if r := remove(t, repo, "t1", false, ""); r.Status != "removed" {
		t.Fatalf("untouched with stale marker: %+v", r)
	}
	// A corrupt marker on a changed tree is "not collected".
	p2 := create(t, repo, "t2", "")
	writeFile(t, filepath.Join(p2, "x.txt"), "x")
	_ = WriteCollected(p2, "garbage")
	if r := remove(t, repo, "t2", false, ""); r.Status != "dirty" {
		t.Fatalf("corrupt marker: %+v", r)
	}
	if r := remove(t, repo, "t2", true, ""); r.Status != "removed" || exists(p2) {
		t.Fatalf("force: %+v", r)
	}
}

func TestIgnoredSurface(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, filepath.Join(repo, "node_modules", "dep", "i.js"), "dep\n")

	// The carried dependency dir alone, even mutated, is not worker output.
	p1 := create(t, repo, "t1", "")
	writeFile(t, filepath.Join(p1, "node_modules", "dep", "i.js"), "rebuilt\n")
	writeFile(t, filepath.Join(p1, "node_modules", "new", "j.js"), "added\n")
	if r := remove(t, repo, "t1", false, ""); r.Status != "removed" {
		t.Fatalf("carried dir: %+v", r)
	}

	// A new ignored entry since create refuses, even with no tracked change
	// and after an earlier collection, and the file survives.
	p2 := create(t, repo, "t2", "")
	if _, err := DiffAndCollect(repo, "t2"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(p2, ".env"), "SECRET=1\n")
	r := remove(t, repo, "t2", false, "")
	if r.Status != "dirty" || len(r.Files) != 0 ||
		r.Reason != "worktree's ignored files changed since it was created and were never collected; collect the diff or pass --force" {
		t.Fatalf("new ignored file: %+v", r)
	}
	if !exists(filepath.Join(p2, ".env")) {
		t.Fatal("the ignored file was destroyed")
	}
	// Collected with a tracked edit and a new ignored dir; deleting that
	// collected ignored entry changes the digest, so the edit is
	// uncollected again.
	p3 := create(t, repo, "t3", "")
	writeFile(t, filepath.Join(p3, "README.md"), "edited\n")
	writeFile(t, filepath.Join(p3, ".venv", "x"), "x")
	_, _ = Collect(repo, "t3", "")
	if err := os.RemoveAll(filepath.Join(p3, ".venv")); err != nil {
		t.Fatal(err)
	}
	if r := remove(t, repo, "t3", false, ""); r.Status != "dirty" {
		t.Fatalf("deleted collected ignored entry: %+v", r)
	}
	_, _ = Collect(repo, "t2", "")
	if r := remove(t, repo, "t2", false, ""); r.Status != "removed" {
		t.Fatalf("collected ignored surface: %+v", r)
	}
}

func TestMissingCreateMarker(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, filepath.Join(repo, "node_modules", "i.js"), "x")
	path := create(t, repo, "t1", "")
	_ = removeMarkers(path, createdIgnoredFile)
	// Unknown create-time surface + non-empty ignored surface: refuse
	// unless collected.
	if r := remove(t, repo, "t1", false, ""); r.Status != "dirty" {
		t.Fatalf("no marker: %+v", r)
	}
	_, _ = Collect(repo, "t1", "")
	if r := remove(t, repo, "t1", false, ""); r.Status != "removed" {
		t.Fatalf("no marker but collected: %+v", r)
	}
}

// createIntegration plants a parentless integration tree for task carrying
// one integrated commit; returns its path.
func createIntegration(t *testing.T, repo, task string) string {
	t.Helper()
	path := plantTree(t, repo, task, RoleIntegration, "")
	gateCommit(t, path, task+".txt", "integrated\n")
	return path
}

// gateCommit commits name with content in the tree at path.
func gateCommit(t *testing.T, path, name, content string) {
	t.Helper()
	writeFile(t, filepath.Join(path, name), content)
	git(t, path, "add", "-A")
	git(t, path, "commit", "-q", "-m", "commit "+name)
}

// gateHead is the HEAD sha of the tree at path.
func gateHead(t *testing.T, path string) string {
	t.Helper()
	return strings.TrimSpace(git(t, path, "rev-parse", "HEAD"))
}

func TestIntegrationTreeKeptUntilMergedAtHead(t *testing.T) {
	repo := newRepo(t)
	asm := plantTree(t, repo, "asm", RoleAssembly, "")
	x := plantTree(t, repo, "x", RoleIntegration, asm)

	// Nothing committed past its baseline: nothing to lose.
	if r := remove(t, repo, "x", false, ""); r.Status != "removed" || exists(x) {
		t.Fatalf("untouched integration tree: %+v", r)
	}

	x = plantTree(t, repo, "x", RoleIntegration, asm)
	gateCommit(t, x, "x.txt", "integrated\n")
	writeFile(t, filepath.Join(x, ".venv", "built"), "x")
	g1 := "integration tree x holds commits not merged into its assembly; merge it with " +
		"'agent-exec isolate merge --task x -m <message>', or pass --force to discard them"
	if r := remove(t, repo, "x", false, ""); r.Status != "dirty" || r.Reason != g1 || !exists(x) {
		t.Fatalf("unmerged integration tree: %+v", r)
	}
	sw := sweep(t, repo, SweepOptions{IncludeCurrent: true, IncludeLive: true})
	if w := byTask(sw)["x"]; w.Status != "dirty" || !exists(x) {
		t.Fatalf("sweep took an unmerged integration tree: %+v", w)
	}

	// Merged at HEAD, then committed again: unmerged again.
	_ = writeMarker(x, mergedFile, gateHead(t, x))
	gateCommit(t, x, "y.txt", "later\n")
	if r := remove(t, repo, "x", false, ""); r.Status != "dirty" || r.Reason != g1 {
		t.Fatalf("commit after the merge: %+v", r)
	}

	// Merged at HEAD: only uncommitted work counts, ignored drift exempt.
	_ = writeMarker(x, mergedFile, gateHead(t, x))
	writeFile(t, filepath.Join(x, "x.txt"), "edited after merging\n")
	r := remove(t, repo, "x", false, "")
	if r.Status != "dirty" || !slices.Equal(r.Files, []string{"x.txt"}) || !exists(x) ||
		r.Reason != "integration tree holds 1 uncommitted change(s) on top of its merged commits; commit and merge them, or pass --force" {
		t.Fatalf("merged integration tree with an uncommitted edit: %+v", r)
	}
	writeFile(t, filepath.Join(x, "x.txt"), "integrated\n")
	if r := remove(t, repo, "x", false, ""); r.Status != "removed" || exists(x) {
		t.Fatalf("merged integration tree: %+v", r)
	}

	// No parent: nothing to merge into.
	lone := createIntegration(t, repo, "lone")
	if r := remove(t, repo, "lone", false, ""); r.Status != "dirty" ||
		r.Reason != "integration tree lone has no assembly to merge into; pass --force to discard it" {
		t.Fatalf("parentless integration tree: %+v", r)
	}
	if r := remove(t, repo, "lone", true, ""); r.Status != "removed" || exists(lone) {
		t.Fatalf("--force: %+v", r)
	}
}

func TestAssemblyKeptUntilLandedAtHead(t *testing.T) {
	repo := newRepo(t)
	asm := plantTree(t, repo, "asm", RoleAssembly, "")
	if r := remove(t, repo, "asm", false, ""); r.Status != "removed" || exists(asm) {
		t.Fatalf("untouched assembly: %+v", r)
	}

	asm = plantTree(t, repo, "asm", RoleAssembly, "")
	gateCommit(t, asm, "a.txt", "merged\n")
	if r := remove(t, repo, "asm", false, ""); r.Status != "dirty" || !exists(asm) ||
		r.Reason != "assembly asm holds work not landed on your branch; land it from your checkout with "+
			"'agent-exec isolate land --task asm -m <message>', or pass --force to discard it" {
		t.Fatalf("unlanded assembly: %+v", r)
	}
	_ = writeMarker(asm, landedFile, gateHead(t, asm))
	writeFile(t, filepath.Join(asm, "a.txt"), "edited\n")
	r := remove(t, repo, "asm", false, "")
	if r.Status != "dirty" || !slices.Equal(r.Files, []string{"a.txt"}) ||
		r.Reason != "assembly holds 1 uncommitted change(s) on top of its landed commits; commit and land them, or pass --force" {
		t.Fatalf("landed assembly with an uncommitted edit: %+v", r)
	}
	writeFile(t, filepath.Join(asm, "a.txt"), "merged\n")
	if r := remove(t, repo, "asm", false, ""); r.Status != "removed" || exists(asm) {
		t.Fatalf("landed assembly: %+v", r)
	}
}

// gateHierarchy plants assembly a <- integration x <- work t.
func gateHierarchy(t *testing.T, repo string) (a, x, w string) {
	t.Helper()
	a = plantTree(t, repo, "a", RoleAssembly, "")
	x = plantTree(t, repo, "x", RoleIntegration, a)
	w = plantTree(t, repo, "t", "", x)
	return a, x, w
}

func TestTreeWithChildrenIsNeverRemoved(t *testing.T) {
	repo := newRepo(t)
	a, x, w := gateHierarchy(t, repo)
	for _, force := range []bool{false, true} {
		r := remove(t, repo, "a", force, "")
		if r.Status != "has-children" || len(r.Children) != 1 || r.Children[0].Task != "x" || !exists(a) ||
			r.Reason != "1 child tree(s) still open; merge or discard them first (--force never removes a tree with children)" {
			t.Fatalf("assembly with a child (force %v): %+v", force, r)
		}
	}
	r := remove(t, repo, "x", true, "")
	if r.Status != "has-children" || len(r.Children) != 1 || r.Children[0].Task != "t" || !exists(x) {
		t.Fatalf("integration tree with a child: %+v", r)
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Status   string  `json:"status"`
		Task     string  `json:"task"`
		Path     string  `json:"path"`
		Children []Child `json:"children"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "has-children" || got.Task != "x" || got.Path != x ||
		len(got.Children) != 1 || got.Children[0].Task != "t" || got.Children[0].Path != w {
		t.Errorf("has-children JSON = %s", b)
	}
	for _, task := range []string{"t", "x", "a"} {
		if r := remove(t, repo, task, true, ""); r.Status != "removed" {
			t.Fatalf("remove %s once its children are gone: %+v", task, r)
		}
	}
}

func TestAdoptedChildKeepsItsParent(t *testing.T) {
	repo := newRepo(t)
	a := plantTree(t, repo, "a", RoleAssembly, "")
	x := plantTree(t, repo, "x", RoleIntegration, a)
	ext := filepath.Join(filepath.Dir(repo), "external")
	git(t, repo, "worktree", "add", "-q", "-b", "feature/ext", ext)
	if err := WriteAdopted(ext, "ext-task"); err != nil {
		t.Fatal(err)
	}
	if err := WriteParent(ext, x); err != nil {
		t.Fatal(err)
	}
	r := remove(t, repo, "x", true, "")
	if r.Status != "has-children" || len(r.Children) != 1 || r.Children[0].Task != "ext-task" || !exists(x) {
		t.Fatalf("integration tree with an adopted child: %+v", r)
	}
}

func TestRemoveSessionGoesBottomUp(t *testing.T) {
	repo := newRepo(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", sessA)
	a, x, w := gateHierarchy(t, repo)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	r, err := RemoveSession(repo, sessA, true)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, one := range r.Removed {
		if one.Status != "removed" {
			t.Errorf("%+v", one)
		}
		got = append(got, one.Task)
	}
	if !slices.Equal(got, []string{"t", "x", "a"}) {
		t.Fatalf("removal order = %v, want [t x a]", got)
	}
	for _, p := range []string{a, x, w} {
		if exists(p) {
			t.Errorf("%s survived", p)
		}
	}
}

func TestMissingBaselineIsNotReadAsClean(t *testing.T) {
	repo := newRepo(t)
	path := create(t, repo, "t1", "")
	writeFile(t, filepath.Join(path, "w.txt"), "w\n")
	git(t, path, "add", "-A")
	git(t, path, "commit", "-q", "-m", "worker committed")
	_ = removeMarkers(path, baselineFile)
	if r := remove(t, repo, "t1", false, ""); r.Status != "dirty" || !exists(path) {
		t.Fatalf("no baseline: %+v", r)
	}
}

func TestLeasedTreeIsBusyUntilTheLeaseGoesStale(t *testing.T) {
	repo := newRepo(t)
	t.Setenv("ORCHESTRA_LEASE_WINDOW", "120")
	path := create(t, repo, "t1", "")
	lease := func(age time.Duration) {
		seen := float64(time.Now().Add(-age).UnixNano()) / 1e9
		_ = writeMarker(path, leaseFile, fmt.Sprintf(`{"agent_id":"a1","agent_type":"engineer","last_seen":%f,"since":%f}`, seen, seen))
	}
	lease(5 * time.Second)
	if r := remove(t, repo, "t1", true, ""); r.Status != "busy" || !strings.Contains(r.Reason, "engineer") || !exists(path) {
		t.Fatalf("fresh lease: %+v", r)
	}
	lease(time.Hour)
	if r := remove(t, repo, "t1", false, ""); r.Status != "removed" || exists(path) {
		t.Fatalf("stale lease: %+v", r)
	}
}

func TestBashActivityKeepsTheTreeBusy(t *testing.T) {
	repo := newRepo(t)
	t.Setenv("ORCHESTRA_ACTIVITY_WINDOW", "")
	path := create(t, repo, "t1", "")
	activity := func(age time.Duration) {
		seen := float64(time.Now().Add(-age).UnixNano()) / 1e9
		_ = writeMarker(path, activityFile, fmt.Sprintf(`{"agent_id":"a1","agent_type":"tester","last_seen":%f,"command":"pnpm test"}`, seen))
	}
	activity(5 * time.Minute)
	if r := remove(t, repo, "t1", true, ""); r.Status != "busy" || !strings.Contains(r.Reason, `"pnpm test"`) || !exists(path) {
		t.Fatalf("fresh activity: %+v", r)
	}
	activity(time.Hour)
	if r := remove(t, repo, "t1", false, ""); r.Status != "removed" || exists(path) {
		t.Fatalf("stale activity: %+v", r)
	}
}

func TestFailedPruneRestoresTheTree(t *testing.T) {
	repo := newRepo(t)
	path := create(t, repo, "t1", "")
	writeFile(t, filepath.Join(path, "keep.txt"), "keep\n")
	admin := strings.TrimSpace(git(t, path, "rev-parse", "--absolute-git-dir"))
	// A locked worktree survives prune, so the removal must roll back.
	git(t, repo, "worktree", "lock", path)
	r := remove(t, repo, "t1", true, "")
	if r.Status != "error" || !strings.Contains(r.Reason, "restored at "+path) {
		t.Fatalf("locked tree: %+v", r)
	}
	if readFile(t, filepath.Join(path, "keep.txt")) != "keep\n" || !exists(admin) {
		t.Fatal("tree or metadata not intact")
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".*"+trashMarker+"*")); len(left) != 0 {
		t.Fatalf("trash left behind: %v", left)
	}
}

func TestRemovedTreeIsDeletedInTheBackground(t *testing.T) {
	repo := newRepo(t)
	path := create(t, repo, "t1", "")
	writeFile(t, filepath.Join(path, "deep", "er", "f.txt"), "x\n")
	if r := remove(t, repo, "t1", true, ""); r.Status != "removed" || r.Reason != "" || exists(path) {
		t.Fatalf("remove: %+v", r)
	}
	trash := filepath.Join(filepath.Dir(path), ".*"+trashMarker+"*")
	for deadline := time.Now().Add(10 * time.Second); ; {
		left, _ := filepath.Glob(trash)
		if len(left) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the detached delete never finished: %v", left)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRemoveSession(t *testing.T) {
	repo := newRepo(t)
	create(t, repo, "t1", sessA)
	create(t, repo, "t2", sessA)
	other := create(t, repo, "t1", sessB)
	t.Setenv("CLAUDE_CODE_SESSION_ID", sessB) // not the session being removed
	r, err := RemoveSession(repo, sessA, false)
	if err != nil || r.Session != "aaaaaaaa" || len(r.Removed) != 2 {
		t.Fatalf("%+v %v", r, err)
	}
	for _, one := range r.Removed {
		if one.Status != "removed" {
			t.Errorf("%+v", one)
		}
	}
	if !exists(other) {
		t.Fatal("another session's tree was removed")
	}
	if _, err := RemoveSession(repo, "x!", false); err == nil {
		t.Fatal("invalid session accepted")
	}
}

func TestAdoptedTreeIsResolvedButNeverRemoved(t *testing.T) {
	repo := newRepo(t)
	ext := filepath.Join(filepath.Dir(repo), "external")
	git(t, repo, "worktree", "add", "-q", "-b", "feature/ext", ext)
	t.Setenv("CLAUDE_CODE_SESSION_ID", sessA)
	if err := WriteAdopted(ext, "ext-task"); err != nil {
		t.Fatal(err)
	}
	if task, ok := ReadAdopted(ext); !ok || task != "ext-task" {
		t.Fatalf("ReadAdopted = %q %v", task, ok)
	}
	if p, b, ok := ResolveWorktree(repo, "ext-task", ""); !ok || p != ext || b != "feature/ext" {
		t.Fatalf("resolve %q %q %v", p, b, ok)
	}
	sw, _ := Sweep(repo, SweepOptions{IncludeCurrent: true, IncludeLive: true})
	if sw.Summary.External != 1 || !exists(ext) {
		t.Fatalf("sweep %+v", sw.Summary)
	}
	r := remove(t, repo, "ext-task", true, "")
	if r.Status != "unadopted" || !exists(ext) || !strings.Contains(branches(t, repo), "feature/ext") {
		t.Fatalf("%+v", r)
	}
	if _, ok := ReadAdopted(ext); ok {
		t.Fatal("adoption marker survived")
	}
}

func TestBaselineFallsBackToTheBranchTip(t *testing.T) {
	repo := newRepo(t)
	path := create(t, repo, "t1", "")
	recorded, _ := ReadBaseline(path)
	_ = removeMarkers(path, baselineFile)
	tip, ok := ReadBaseline(path)
	if !ok || tip != recorded {
		t.Fatalf("fallback %q vs %q", tip, recorded)
	}
}
