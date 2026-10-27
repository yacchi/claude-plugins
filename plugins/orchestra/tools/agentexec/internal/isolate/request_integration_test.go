package isolate

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// integWorkspace is the request workspace of newRepo's session on the pinned
// clock: where the assembly of repo lands, and the request's directory.
func integWorkspace(t *testing.T) string {
	t.Helper()
	return filepath.Join(paths.RealPath(os.Getenv(RequestRootEnv)), "20261028-local")
}

func TestIntegCreatesFromAssemblyHead(t *testing.T) {
	repo := newRepo(t)
	requestRootClock(t)
	head := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))

	res, err := EnsureIntegration(repo, "feat", "")
	if err != nil || res.Status != "ready" {
		t.Fatalf("EnsureIntegration() = %+v, %v; want ready", res, err)
	}
	if !reflect.DeepEqual(res.Created, []string{"assembly", "integration"}) {
		t.Errorf("created = %v; want [assembly integration]", res.Created)
	}
	want := filepath.Join(integWorkspace(t), ".integration", "repo", "feat")
	if res.Integration != want {
		t.Errorf("integration = %q; want %q", res.Integration, want)
	}
	if got := ReadRole(res.Integration); got != RoleIntegration {
		t.Errorf("role = %q; want %q", got, RoleIntegration)
	}
	if got := ReadParent(res.Integration); got != res.Assembly {
		t.Errorf("parent = %q; want the assembly %q", got, res.Assembly)
	}
	if got := readMarker(res.Integration, baselineFile); got != head || res.Head != head {
		t.Errorf("baseline = %q, head = %q; want the assembly HEAD %q", got, res.Head, head)
	}

	again, err := EnsureIntegration(repo, "feat", "")
	if err != nil || again.Status != "ready" || again.Integration != res.Integration {
		t.Fatalf("second EnsureIntegration() = %+v, %v; want ready at the same path", again, err)
	}
	if len(again.Created) != 0 {
		t.Errorf("second created = %v; want none", again.Created)
	}
	if again.Request != res.Request || again.Assembly != res.Assembly {
		t.Errorf("second request/assembly = %q/%q; want %q/%q", again.Request, again.Assembly, res.Request, res.Assembly)
	}
}

func TestIntegCutsFromAssemblyAfterCommit(t *testing.T) {
	repo := newRepo(t)
	requestRootClock(t)
	first, err := EnsureIntegration(repo, "feat", "")
	if err != nil || first.Status != "ready" {
		t.Fatalf("EnsureIntegration(feat) = %+v, %v; want ready", first, err)
	}

	// A merge lands in the assembly: the next feature must start from it.
	writeFile(t, filepath.Join(first.Assembly, "merged.txt"), "merged\n")
	git(t, first.Assembly, "add", "-A")
	git(t, first.Assembly, "commit", "-q", "-m", "merged feat")
	assemblyHead := strings.TrimSpace(git(t, first.Assembly, "rev-parse", "HEAD"))

	res, err := EnsureIntegration(repo, "feat2", "")
	if err != nil || res.Status != "ready" {
		t.Fatalf("EnsureIntegration(feat2) = %+v, %v; want ready", res, err)
	}
	if !reflect.DeepEqual(res.Created, []string{"integration"}) {
		t.Errorf("created = %v; want [integration]", res.Created)
	}
	if res.Head != assemblyHead {
		t.Errorf("head = %q; want the assembly's new HEAD %q", res.Head, assemblyHead)
	}
	if got := readFile(t, filepath.Join(res.Integration, "merged.txt")); got != "merged\n" {
		t.Errorf("feat2 merged.txt = %q; want the merged content", got)
	}
}

func TestIntegRefusesAssemblyTarget(t *testing.T) {
	repo := newRepo(t)
	requestRootClock(t)
	asm, err := EnsureAssembly(repo, "")
	if err != nil || asm.Status != "ready" {
		t.Fatalf("EnsureAssembly() = %+v, %v; want ready", asm, err)
	}

	res, err := EnsureIntegration(repo, asm.Task, "")
	if err != nil {
		t.Fatal(err)
	}
	want := asm.Task + " is an assembly worktree, not an integration target"
	if res.Status != "error" || res.Reason != want {
		t.Errorf("EnsureIntegration(request id) = %+v; want error %q", res, want)
	}
	if got := ReadRole(asm.Assembly); got != RoleAssembly {
		t.Errorf("assembly role = %q; want %q, untouched", got, RoleAssembly)
	}
}

func TestIntegAdoptsPlainWorkTree(t *testing.T) {
	repo := newRepo(t)
	requestRootClock(t)
	wt := create(t, repo, "w", "")

	res, err := EnsureIntegration(repo, "w", "")
	if err != nil || res.Status != "ready" {
		t.Fatalf("EnsureIntegration(w) = %+v, %v; want ready", res, err)
	}
	if len(res.Created) != 0 {
		t.Errorf("created = %v; want none", res.Created)
	}
	if res.Assembly != "" {
		t.Errorf("assembly = %q; want none for a tree made outside a request", res.Assembly)
	}
	if got := ReadRole(wt); got != RoleIntegration {
		t.Errorf("role of w = %q; want %q", got, RoleIntegration)
	}
}

func TestIntegUncommittedRefusalLeavesNothing(t *testing.T) {
	repo := ensureDirtyRepo(t)
	requestRootClock(t)

	res, err := EnsureIntegration(repo, "feat", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "uncommitted" {
		t.Fatalf("EnsureIntegration() = %+v; want uncommitted", res)
	}
	if names := ensureRequests(t); len(names) != 0 {
		t.Errorf("request workspaces after refusal = %v; want none", names)
	}
}

func TestIntegIncludeBuildsOnCheckoutEdit(t *testing.T) {
	repo := ensureDirtyRepo(t)
	requestRootClock(t)

	res, err := EnsureIntegration(repo, "feat", UncommittedInclude)
	if err != nil || res.Status != "ready" {
		t.Fatalf("EnsureIntegration(include) = %+v, %v; want ready", res, err)
	}
	if got := readFile(t, filepath.Join(res.Integration, "a.txt")); got != "edited\n" {
		t.Errorf("feat a.txt = %q; want the checkout's edit", got)
	}
}

func TestIntegConcurrentCallersShareOneTree(t *testing.T) {
	repo := newRepo(t)
	requestRootClock(t)

	const callers = 2
	results := make([]EnsureResult, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = EnsureIntegration(repo, "feat", "")
		}()
	}
	wg.Wait()

	creators := 0
	for i, res := range results {
		if errs[i] != nil || res.Status != "ready" {
			t.Fatalf("caller %d: %+v, %v; want ready", i, res, errs[i])
		}
		if res.Integration != results[0].Integration {
			t.Errorf("caller %d integration = %q; want %q", i, res.Integration, results[0].Integration)
		}
		for _, c := range res.Created {
			if c == "integration" {
				creators++
			}
		}
	}
	if creators != 1 {
		t.Errorf("%d callers created the integration tree; want exactly 1", creators)
	}
}
