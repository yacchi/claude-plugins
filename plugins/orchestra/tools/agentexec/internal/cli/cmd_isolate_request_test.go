package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// requestCmdRepo is a throwaway checkout on main with README.md committed. The
// request root is a temp dir of its own, so no test touches the real one.
func requestCmdRepo(t *testing.T) (repo, root string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "Request User")
	t.Setenv("GIT_AUTHOR_EMAIL", "request@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Request User")
	t.Setenv("GIT_COMMITTER_EMAIL", "request@example.com")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	root = t.TempDir()
	t.Setenv(isolate.RequestRootEnv, root)
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	alive := filepath.Join(tmp, "alive")
	t.Setenv("ORCHESTRA_ALIVE_DIR", alive)
	isolate.SetAliveDir(alive)
	t.Cleanup(func() { isolate.SetAliveDir("") })

	repo = filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	landGit(t, repo, "init", "-q", "-b", "main")
	landWrite(t, filepath.Join(repo, "README.md"), "hello\n")
	landGit(t, repo, "add", "-A")
	landGit(t, repo, "commit", "-q", "-m", "init")
	return repo, paths.RealPath(root)
}

// requestCmdJSON decodes a command's one-line JSON output.
func requestCmdJSON(t *testing.T, out string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	return m
}

func TestRequestStartCreatesAssembly(t *testing.T) {
	repo, _ := requestCmdRepo(t)
	out, code := runIsolate(t, newIsolateRequestCmd(), "start", "--repo", repo)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	res := requestCmdJSON(t, out)
	if res["status"] != "ready" {
		t.Fatalf("status: %v", res)
	}
	if created, _ := res["created"].([]any); len(created) != 1 || created[0] != "assembly" {
		t.Errorf("created: %v", res["created"])
	}
}

func TestRequestStartIntoCreatesIntegrationUnderWorkspace(t *testing.T) {
	repo, root := requestCmdRepo(t)
	if out, code := runIsolate(t, newIsolateRequestCmd(), "start", "--repo", repo); code != 0 {
		t.Fatalf("start: exit %d: %s", code, out)
	}
	out, code := runIsolate(t, newIsolateRequestCmd(), "start", "--into", "feat", "--repo", repo)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	res := requestCmdJSON(t, out)
	if created, _ := res["created"].([]any); len(created) != 1 || created[0] != "integration" {
		t.Errorf("created: %v", res["created"])
	}
	integration, _ := res["integration"].(string)
	if !strings.HasPrefix(integration, root+string(filepath.Separator)) ||
		!strings.Contains(integration, string(filepath.Separator)+".integration"+string(filepath.Separator)) {
		t.Errorf("integration %q is not under the workspace's .integration/", integration)
	}
}

func TestRequestShowReportsOpenRequest(t *testing.T) {
	repo, _ := requestCmdRepo(t)
	if out, code := runIsolate(t, newIsolateRequestCmd(), "start", "--into", "feat", "--repo", repo); code != 0 {
		t.Fatalf("start: exit %d: %s", code, out)
	}
	out, code := runIsolate(t, newIsolateRequestCmd(), "show")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	res := requestCmdJSON(t, out)
	if res["status"] != "open" {
		t.Fatalf("status: %v", res)
	}
	repos, _ := res["repos"].([]any)
	if len(repos) != 1 {
		t.Fatalf("repos: %v", res["repos"])
	}
	rr := repos[0].(map[string]any)
	if rr["alive"] != true {
		t.Errorf("alive: %v", rr["alive"])
	}
	integ, _ := rr["integration"].([]any)
	if len(integ) != 1 {
		t.Fatalf("integration: %v", rr["integration"])
	}
	in := integ[0].(map[string]any)
	if in["task"] != "feat" || in["children"] != float64(0) {
		t.Errorf("integration entry: %v", in)
	}
}

func TestRequestShowUnknownIDIsAbsent(t *testing.T) {
	requestCmdRepo(t)
	out, code := runIsolate(t, newIsolateRequestCmd(), "show", "--request", "nope")
	if code != 1 {
		t.Fatalf("exit %d: %s", code, out)
	}
	res := requestCmdJSON(t, out)
	if res["status"] != "absent" || res["reason"] != "no open request nope" {
		t.Errorf("output: %v", res)
	}
}

func TestRequestShowWithoutRequestIsAbsent(t *testing.T) {
	requestCmdRepo(t)
	out, code := runIsolate(t, newIsolateRequestCmd(), "show")
	if code != 1 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if res := requestCmdJSON(t, out); res["reason"] != "no open request for this session" {
		t.Errorf("output: %v", res)
	}
}

func TestRequestListEmptyIsArrayNotNull(t *testing.T) {
	requestCmdRepo(t)
	out, code := runIsolate(t, newIsolateRequestCmd(), "list")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if strings.TrimSpace(out) != `{"requests":[]}` {
		t.Errorf("output %q", out)
	}
}

func TestRequestListDropsRequestWhoseAssemblyWasRemoved(t *testing.T) {
	repo, _ := requestCmdRepo(t)
	out, code := runIsolate(t, newIsolateRequestCmd(), "start", "--repo", repo)
	if code != 0 {
		t.Fatalf("start: exit %d: %s", code, out)
	}
	asm := requestCmdJSON(t, out)["assembly"].(string)
	if out, code := runIsolate(t, newIsolateRequestCmd(), "list"); code != 0 || !strings.Contains(out, asm) {
		t.Fatalf("list before removal: exit %d: %s", code, out)
	}
	landGit(t, repo, "worktree", "remove", "--force", asm)

	out, code = runIsolate(t, newIsolateRequestCmd(), "list")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if strings.TrimSpace(out) != `{"requests":[]}` {
		t.Errorf("output %q", out)
	}
}

func TestRequestStartRefusesUncommittedChanges(t *testing.T) {
	repo, _ := requestCmdRepo(t)
	landWrite(t, filepath.Join(repo, "README.md"), "edited\n")

	out, code := runIsolate(t, newIsolateRequestCmd(), "start", "--repo", repo)
	if code != 1 {
		t.Fatalf("exit %d: %s", code, out)
	}
	res := requestCmdJSON(t, out)
	if res["status"] != "uncommitted" {
		t.Fatalf("status: %v", res)
	}
	if files, _ := res["files"].([]any); len(files) != 1 || files[0] != "README.md" {
		t.Errorf("files: %v", res["files"])
	}
}

func TestRequestStartIncludeAcceptsUncommittedChanges(t *testing.T) {
	repo, _ := requestCmdRepo(t)
	landWrite(t, filepath.Join(repo, "README.md"), "edited\n")

	out, code := runIsolate(t, newIsolateRequestCmd(), "start", "--uncommitted", "include", "--repo", repo)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if res := requestCmdJSON(t, out); res["uncommitted"] != isolate.UncommittedInclude {
		t.Errorf("uncommitted: %v", res["uncommitted"])
	}
}

func TestRequestStartRejectsUnknownUncommittedChoice(t *testing.T) {
	repo, _ := requestCmdRepo(t)
	out, code := runIsolate(t, newIsolateRequestCmd(), "start", "--uncommitted", "bogus", "--repo", repo)
	if code != 2 {
		t.Fatalf("exit %d: %s", code, out)
	}
}
