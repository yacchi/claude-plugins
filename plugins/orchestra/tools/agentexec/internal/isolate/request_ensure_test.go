package isolate

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// ensureDirtyRepo is newRepo with a committed a.txt, then a tracked edit, an
// untracked file and the carry file the checkout must never hand over.
func ensureDirtyRepo(t *testing.T) string {
	t.Helper()
	repo := newRepo(t)
	writeFile(t, filepath.Join(repo, "a.txt"), "old\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "add a")
	writeFile(t, filepath.Join(repo, "a.txt"), "edited\n")
	writeFile(t, filepath.Join(repo, "n.txt"), "new\n")
	writeFile(t, filepath.Join(repo, ".claude", "settings.local.json"), "{}\n")
	return repo
}

// ensureRequests lists the request workspaces under the pinned root, leaving
// out the lock file every request operation leaves there.
func ensureRequests(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(os.Getenv(RequestRootEnv))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if e.Name() != ".lock" {
			names = append(names, e.Name())
		}
	}
	return names
}

// ensureSecondRepo is a repository named "repo" under parent, so it shares
// its base name with the first one.
func ensureSecondRepo(t *testing.T, parent string) string {
	t.Helper()
	repo := filepath.Join(parent, "repo")
	writeFile(t, filepath.Join(repo, "README.md"), "other\n")
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "init")
	return repo
}

func TestEnsureAssemblyCreatesFromCleanCheckout(t *testing.T) {
	repo := newRepo(t)
	requestRootClock(t)
	head := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))

	res, err := EnsureAssembly(repo, "")
	if err != nil || res.Status != "ready" {
		t.Fatalf("EnsureAssembly() = %+v, %v; want ready", res, err)
	}
	want := filepath.Join(paths.RealPath(os.Getenv(RequestRootEnv)), "20261028-local", "repo")
	if res.Assembly != want {
		t.Errorf("assembly = %q; want %q", res.Assembly, want)
	}
	if res.Uncommitted != "clean" || len(res.Files) != 0 {
		t.Errorf("uncommitted = %q, files = %v; want clean, none", res.Uncommitted, res.Files)
	}
	if !reflect.DeepEqual(res.Created, []string{"assembly"}) {
		t.Errorf("created = %v; want [assembly]", res.Created)
	}
	if got := ReadRole(res.Assembly); got != RoleAssembly {
		t.Errorf("role = %q; want %q", got, RoleAssembly)
	}
	if got := readMarker(res.Assembly, baselineFile); got != head {
		t.Errorf("baseline = %q; want HEAD %q", got, head)
	}
	if got := readMarker(res.Assembly, includedFile); got != "" {
		t.Errorf("included marker = %q; want none on a clean checkout", got)
	}

	again, err := EnsureAssembly(repo, "")
	if err != nil || again.Assembly != res.Assembly || len(again.Created) != 0 {
		t.Fatalf("second EnsureAssembly() = %+v, %v; want the same assembly, created none", again, err)
	}
}

func TestEnsureAssemblyRefusesUncommittedWithoutChoice(t *testing.T) {
	repo := ensureDirtyRepo(t)
	requestRootClock(t)

	res, err := EnsureAssembly(repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "uncommitted" || !reflect.DeepEqual(res.Files, []string{"a.txt", "n.txt"}) {
		t.Fatalf("EnsureAssembly() = %+v; want uncommitted with [a.txt n.txt]", res)
	}
	if !strings.Contains(res.Reason, "a.txt, n.txt") {
		t.Errorf("reason = %q; want it to name the files", res.Reason)
	}
	if names := ensureRequests(t); len(names) != 0 {
		t.Errorf("request workspaces after refusal = %v; want none", names)
	}
	if strings.Contains(branches(t, repo), "orchestra/") {
		t.Errorf("branches after refusal = %q; want no orchestra branch", branches(t, repo))
	}
}

func TestEnsureAssemblyIncludeBuildsOnCheckoutChanges(t *testing.T) {
	repo := ensureDirtyRepo(t)
	requestRootClock(t)
	head := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))

	res, err := EnsureAssembly(repo, UncommittedInclude)
	if err != nil || res.Status != "ready" || res.Uncommitted != UncommittedInclude {
		t.Fatalf("EnsureAssembly(include) = %+v, %v; want ready, include", res, err)
	}
	if got := readFile(t, filepath.Join(res.Assembly, "a.txt")); got != "edited\n" {
		t.Errorf("assembly a.txt = %q; want the edit", got)
	}
	if got := readFile(t, filepath.Join(res.Assembly, "n.txt")); got != "new\n" {
		t.Errorf("assembly n.txt = %q; want the untracked file", got)
	}
	if exists(filepath.Join(res.Assembly, ".claude", "settings.local.json")) {
		t.Error("carry file reached the assembly")
	}
	if got := readMarker(res.Assembly, includedFile); got != head {
		t.Errorf("included = %q; want the checkout HEAD %q", got, head)
	}
	if got := readMarker(res.Assembly, baselineFile); got == head {
		t.Errorf("baseline = HEAD; want the baseline commit that carries the work")
	}
}

func TestEnsureAssemblyIgnoreStartsFromHead(t *testing.T) {
	repo := ensureDirtyRepo(t)
	requestRootClock(t)

	res, err := EnsureAssembly(repo, UncommittedIgnore)
	if err != nil || res.Status != "ready" {
		t.Fatalf("EnsureAssembly(ignore) = %+v, %v; want ready", res, err)
	}
	if !reflect.DeepEqual(res.Files, []string{"a.txt", "n.txt"}) {
		t.Errorf("files = %v; want [a.txt n.txt]", res.Files)
	}
	if got := readFile(t, filepath.Join(res.Assembly, "a.txt")); got != "old\n" {
		t.Errorf("assembly a.txt = %q; want HEAD's content", got)
	}
	if exists(filepath.Join(res.Assembly, "n.txt")) {
		t.Error("untracked file reached the assembly under ignore")
	}
}

func TestEnsureAssemblyRefusesOrchestraTree(t *testing.T) {
	repo := newRepo(t)
	requestRootClock(t)
	wt := create(t, repo, "w", "")

	res, err := EnsureAssembly(wt, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "error" || !strings.Contains(res.Reason, "is an orchestra tree") {
		t.Fatalf("EnsureAssembly(orchestra tree) = %+v; want an error naming the tree", res)
	}
	if names := ensureRequests(t); len(names) != 0 {
		t.Errorf("request workspaces = %v; want none", names)
	}
}

func TestEnsureAssemblyRefusesTrackedExcludedDirs(t *testing.T) {
	repo := newRepo(t)
	requestRootClock(t)
	writeFile(t, filepath.Join(repo, "node_modules", "x", "index.js"), "x\n")
	git(t, repo, "add", "-f", "node_modules")
	git(t, repo, "commit", "-q", "-m", "track deps")

	res, err := EnsureAssembly(repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "error" || !strings.Contains(res.Reason, "node_modules/") {
		t.Fatalf("EnsureAssembly() = %+v; want an error naming node_modules/", res)
	}
	if names := ensureRequests(t); len(names) != 0 {
		t.Errorf("request workspaces = %v; want none", names)
	}
}

func TestEnsureAssemblyFailedCreateLeavesNoWorkspace(t *testing.T) {
	repo := newRepo(t)
	requestRootClock(t)
	// A work tree already on the branch an assembly would take: creation is
	// refused, and the request it would have started must not stay behind.
	create(t, repo, "20261028-local", "")

	res, err := EnsureAssembly(repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "error" || !strings.Contains(res.Reason, "which is not an assembly") {
		t.Fatalf("EnsureAssembly() = %+v; want the not-an-assembly error", res)
	}
	if names := ensureRequests(t); len(names) != 0 {
		t.Errorf("request workspaces = %v; want none", names)
	}
}

func TestCheckAssemblyCreatesNothing(t *testing.T) {
	repo := ensureDirtyRepo(t)
	requestRootClock(t)

	res, err := CheckAssembly(repo, "")
	if err != nil || res.Status != "uncommitted" {
		t.Fatalf("CheckAssembly() = %+v, %v; want uncommitted", res, err)
	}
	if names := ensureRequests(t); len(names) != 0 {
		t.Errorf("request workspaces = %v; want none", names)
	}

	if _, err := EnsureAssembly(repo, UncommittedInclude); err != nil {
		t.Fatal(err)
	}
	res, err = CheckAssembly(repo, "")
	if err != nil || res.Status != "ready" || res.Assembly == "" {
		t.Fatalf("CheckAssembly() after creation = %+v, %v; want ready with the assembly", res, err)
	}
}

func TestEnsureAssemblyNamesSecondRepoWithSameBaseName(t *testing.T) {
	repo := newRepo(t)
	requestRootClock(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", sessA)
	first, err := EnsureAssembly(repo, "")
	if err != nil || first.Status != "ready" {
		t.Fatalf("first EnsureAssembly() = %+v, %v", first, err)
	}

	other := ensureSecondRepo(t, filepath.Join(filepath.Dir(repo), "other"))
	second, err := EnsureAssembly(other, "")
	if err != nil || second.Status != "ready" {
		t.Fatalf("second EnsureAssembly() = %+v, %v", second, err)
	}
	if second.Request != first.Request {
		t.Errorf("request = %q; want the session's request %q", second.Request, first.Request)
	}
	want := filepath.Join(first.Dir, "repo-2")
	if second.Assembly != want {
		t.Errorf("assembly = %q; want %q", second.Assembly, want)
	}
}
