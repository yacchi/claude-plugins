package isolate

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/orcanest"
)

// failingOrca installs an orca that logs its argv and rejects every call,
// and returns the log and the nester's stderr.
func failingOrca(t *testing.T) (log string, stderr *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls")
	bin := filepath.Join(dir, "orca")
	writeFile(t, bin, "#!/bin/sh\necho \"$*\" >> '"+log+"'\n"+
		"echo '{\"ok\":false,\"error\":{\"message\":\"unknown worktree\"}}'\nexit 1\n")
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	orcanest.ResetWarning(t)
	stderr = &bytes.Buffer{}
	saved := nester
	nester = &orcanest.Nester{Binary: bin, Stderr: stderr}
	t.Cleanup(func() { nester = saved })
	return log, stderr
}

func TestCreatedTreesNestUnderTheirBaseBranchTreeWhateverOrcaSays(t *testing.T) {
	repo := newRepo(t)
	log, stderr := failingOrca(t)
	integ := filepath.Join(filepath.Dir(repo), "integ")
	git(t, repo, "worktree", "add", "-q", "-b", "integ", integ)
	var paths []string
	for _, task := range []string{"a", "b"} {
		res, err := Create(repo, task, CreateOptions{Backend: BackendGit, Onto: "integ", BaseBranch: "integ"})
		if err != nil || res.Status != "created" {
			t.Fatalf("a failed nesting failed the create: %+v %v", res, err)
		}
		paths = append(paths, res.Path)
	}
	calls := strings.Split(strings.TrimSpace(readFile(t, log)), "\n")
	if len(calls) != 2 {
		t.Fatalf("calls: %q", calls)
	}
	for i, c := range calls {
		want := "worktree set --worktree path:" + paths[i] + " --parent-worktree path:" + integ + " --json"
		if c != want {
			t.Errorf("not nested under the base branch's tree:\n got %s\nwant %s", c, want)
		}
	}
	if n := strings.Count(stderr.String(), "could not nest"); n != 1 {
		t.Errorf("want one warning per process, got %d: %s", n, stderr)
	}
}
