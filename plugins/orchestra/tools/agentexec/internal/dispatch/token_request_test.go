package dispatch

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
)

// prepareCheckRepo is a tracked repository, dirty when asked, with a request
// root of its own so no test reaches the real request state.
func prepareCheckRepo(t *testing.T, dirty bool) (repo, requests string) {
	t.Helper()
	repo = trackedDispatchRepo(t)
	requests = t.TempDir()
	t.Setenv("ORCHESTRA_REQUEST_ROOT", requests)
	if dirty {
		mustWrite(t, filepath.Join(repo, "README.md"), "edited\n")
	}
	return repo, requests
}

// prepareCheckSpec is an always-isolated run with a run id and a task.
func prepareCheckSpec(repo string) PrepareSpec {
	return PrepareSpec{
		Class: "standard", Workdir: repo, Isolate: IsolateAlways,
		Task: ptr("t1"), RunID: ptr("run-1"),
	}
}

// prepareCheckCount is the number of entries in dir that are not dotfiles
// (0 when it does not exist). The request root's .lock is not a workspace.
func prepareCheckCount(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			n++
		}
	}
	return n
}

func TestPrepareCheckRefusesDirtyCheckoutWithoutRequest(t *testing.T) {
	for _, mode := range []string{IsolateAlways, IsolateCli} {
		t.Run(mode, func(t *testing.T) {
			repo, requests := prepareCheckRepo(t, true)
			cfg := testConfig(t)
			spec := prepareCheckSpec(repo)
			spec.Isolate = mode

			token, err := Prepare(cfg, spec)
			if err == nil || !strings.Contains(err.Error(), "isolate request start --into run-1") {
				t.Fatalf("Prepare = %q, %v; want the request start refusal", token, err)
			}
			if token != "" {
				t.Fatalf("Prepare returned token %q on refusal", token)
			}
			if n := prepareCheckCount(t, TokenDir(cfg)); n != 0 {
				t.Fatalf("refused prepare wrote %d token file(s)", n)
			}
			if n := prepareCheckCount(t, requests); n != 0 {
				t.Fatalf("refused prepare created %d request workspace(s)", n)
			}
		})
	}
}

func TestPrepareCheckAcceptsAfterAssemblyDecision(t *testing.T) {
	repo, requests := prepareCheckRepo(t, true)
	cfg := testConfig(t)
	res, err := isolate.EnsureAssembly(repo, isolate.UncommittedIgnore)
	if err != nil || res.Status != "ready" {
		t.Fatalf("EnsureAssembly = %+v, %v; want ready", res, err)
	}
	before := prepareCheckCount(t, requests)

	token, err := Prepare(cfg, prepareCheckSpec(repo))
	if err != nil || token == "" {
		t.Fatalf("Prepare = %q, %v; want a token", token, err)
	}
	if n := prepareCheckCount(t, requests); n != before {
		t.Fatalf("prepare changed the request workspaces: %d -> %d", before, n)
	}
}

func TestPrepareCheckAcceptsCleanCheckoutWithoutCreatingRequest(t *testing.T) {
	repo, requests := prepareCheckRepo(t, false)
	cfg := testConfig(t)

	token, err := Prepare(cfg, prepareCheckSpec(repo))
	if err != nil || token == "" {
		t.Fatalf("Prepare = %q, %v; want a token", token, err)
	}
	if n := prepareCheckCount(t, requests); n != 0 {
		t.Fatalf("prepare created %d request workspace(s) on a clean checkout", n)
	}
}

func TestPrepareCheckSkipsUnchecked(t *testing.T) {
	cases := map[string]func(s *PrepareSpec){
		"auto isolate":  func(s *PrepareSpec) { s.Isolate = IsolateAuto },
		"never isolate": func(s *PrepareSpec) { s.Isolate = IsolateNever },
		"no run id":     func(s *PrepareSpec) { s.RunID = nil },
		"empty run id":  func(s *PrepareSpec) { s.RunID = ptr("") },
		"no task":       func(s *PrepareSpec) { s.Task = nil },
		"empty task":    func(s *PrepareSpec) { s.Task = ptr("") },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			repo, _ := prepareCheckRepo(t, true)
			cfg := testConfig(t)
			spec := prepareCheckSpec(repo)
			change(&spec)

			token, err := Prepare(cfg, spec)
			if err != nil || token == "" {
				t.Fatalf("Prepare = %q, %v; want a token", token, err)
			}
			if _, err := LoadToken(cfg, token); err != nil {
				t.Fatalf("LoadToken(%q): %v", token, err)
			}
		})
	}
}
