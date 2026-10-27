package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// requestDispatchRepo is a tracked repository, dirty when asked, with a request
// root of its own so no test reaches the real request state. It returns the
// request root as well.
func requestDispatchRepo(t *testing.T, dirty bool) (repo, requests string) {
	t.Helper()
	repo = trackedDispatchRepo(t)
	requests = t.TempDir()
	t.Setenv("ORCHESTRA_REQUEST_ROOT", requests)
	if dirty {
		mustWrite(t, filepath.Join(repo, "README.md"), "edited\n")
	}
	return repo, requests
}

// requestDispatchPrompt is a prompt file for a dry-run dispatch.
func requestDispatchPrompt(t *testing.T) string {
	t.Helper()
	prompt := filepath.Join(t.TempDir(), "prompt.md")
	mustWrite(t, prompt, "do it\n")
	t.Setenv(DryRunEnv, "1")
	return prompt
}

// requestDispatchWorkspaces counts the request workspaces under the request
// root; zero means the dispatch created none. The root's .lock file is not one.
func requestDispatchWorkspaces(t *testing.T, requests string) int {
	t.Helper()
	entries, err := os.ReadDir(requests)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			n++
		}
	}
	return n
}

func TestRequestDispatchCutsTaskTreeFromIntegrationTree(t *testing.T) {
	repo, requests := requestDispatchRepo(t, false)

	iso := isolateFor(testConfig(t), repo, IsolateAlways, "t1", "run-1")

	if !iso.isolated || iso.refused != "" {
		t.Fatalf("isolation %+v, want an isolated task tree", iso.obj)
	}
	ensured, err := isolate.EnsureIntegration(repo, "run-1", "")
	if err != nil || ensured.Status != "ready" {
		t.Fatalf("integration tree: %+v, %v", ensured, err)
	}
	if !strings.HasPrefix(paths.RealPath(ensured.Integration), paths.RealPath(requests)) {
		t.Errorf("integration tree %q is not under the request root %q", ensured.Integration, requests)
	}
	if got := isolate.ReadParent(iso.path); got != paths.RealPath(ensured.Integration) {
		t.Errorf("parent %q, want integration tree %q", got, ensured.Integration)
	}
	if got, ok := isolate.ReadBaseline(iso.path); !ok || got != ensured.Head {
		t.Errorf("baseline %q (%v), want integration HEAD %q", got, ok, ensured.Head)
	}
	want := "; cut from integration tree " + ensured.Into + " of request " + ensured.Request
	if reason, _ := iso.obj["reason"].(string); !strings.HasSuffix(reason, want) {
		t.Errorf("reason %q lacks suffix %q", reason, want)
	}
}

func TestRequestDispatchWithoutRunIDKeepsCheckoutStart(t *testing.T) {
	repo, requests := requestDispatchRepo(t, false)
	prompt := requestDispatchPrompt(t)

	out := Execute(context.Background(), testConfig(t), Request{
		Executor: "pi", Isolate: IsolateAlways, Task: "t1", Workdir: repo,
		PromptFiles: []string{prompt},
	})

	if out.Exit != 0 || out.Diag != "" {
		t.Fatalf("exit %d diag %q, want a clean dispatch", out.Exit, out.Diag)
	}
	path, _, ok := isolate.ResolveWorktree(repo, "t1", "")
	if !ok {
		t.Fatal("no task tree for t1")
	}
	if got := isolate.ReadParent(path); got != "" {
		t.Errorf("parent %q, want none", got)
	}
	if n := requestDispatchWorkspaces(t, requests); n != 0 {
		t.Errorf("request workspaces %d, want none", n)
	}
}

func TestRequestDispatchAutoIgnoresRunIDWhenDirty(t *testing.T) {
	repo, requests := requestDispatchRepo(t, true)
	prompt := requestDispatchPrompt(t)

	out := Execute(context.Background(), testConfig(t), Request{
		Executor: "pi", Isolate: IsolateAuto, Task: "t1", RunID: "run-1",
		Workdir: repo, PromptFiles: []string{prompt},
	})

	if out.Exit != 0 || out.Diag != "" {
		t.Fatalf("exit %d diag %q, want a clean dispatch", out.Exit, out.Diag)
	}
	path, _, ok := isolate.ResolveWorktree(repo, "t1", "")
	if !ok {
		t.Fatal("no task tree for t1")
	}
	if got := isolate.ReadParent(path); got != "" {
		t.Errorf("parent %q, want none", got)
	}
	if n := requestDispatchWorkspaces(t, requests); n != 0 {
		t.Errorf("request workspaces %d, want none", n)
	}
}

func TestRequestDispatchRefusesWithoutRequest(t *testing.T) {
	repo, requests := requestDispatchRepo(t, true)
	prompt := requestDispatchPrompt(t)

	out := Execute(context.Background(), testConfig(t), Request{
		Executor: "pi", Isolate: IsolateAlways, Task: "t1", RunID: "run-1",
		Workdir: repo, PromptFiles: []string{prompt},
	})

	if out.Exit != 1 {
		t.Fatalf("exit %d, want 1 (diag %q)", out.Exit, out.Diag)
	}
	if !strings.Contains(out.Diag, "isolate request start --into run-1") {
		t.Errorf("diag %q lacks the request start hint", out.Diag)
	}
	if n := trackedDispatchWorktrees(t, repo); n != 1 {
		t.Errorf("worktrees %d, want only the repository itself", n)
	}
	if n := requestDispatchWorkspaces(t, requests); n != 0 {
		t.Errorf("request workspaces %d, want none", n)
	}
}

func TestRequestDispatchKeepsIntegrationWorkdirParent(t *testing.T) {
	repo, integration := parentDispatchSetup(t)
	requests := t.TempDir()
	t.Setenv("ORCHESTRA_REQUEST_ROOT", requests)
	prompt := requestDispatchPrompt(t)

	out := Execute(context.Background(), testConfig(t), Request{
		Executor: "pi", Isolate: IsolateAlways, Task: "t1", RunID: "run-1",
		Workdir: integration, PromptFiles: []string{prompt},
	})

	if out.Exit != 0 || out.Diag != "" {
		t.Fatalf("exit %d diag %q, want a clean dispatch", out.Exit, out.Diag)
	}
	path, _, ok := isolate.ResolveWorktree(repo, "t1", "")
	if !ok {
		t.Fatal("no task tree for t1")
	}
	if got := isolate.ReadParent(path); got != paths.RealPath(integration) {
		t.Errorf("parent %q, want the integration tree %q", got, integration)
	}
	if n := requestDispatchWorkspaces(t, requests); n != 0 {
		t.Errorf("request workspaces %d, want none", n)
	}
}
