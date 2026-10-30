package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
)

// mergeTestRepo is a throwaway checkout on main with an assembly tree "asm"
// and an integration tree "feat" under it, the integration tree holding one
// commit (c.txt) past the assembly's HEAD. Both are planted through the Go API.
func mergeTestRepo(t *testing.T) (repo, asm, feat string) {
	t.Helper()
	pinLandMode(t, "ask")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "Merge User")
	t.Setenv("GIT_AUTHOR_EMAIL", "merge@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Merge User")
	t.Setenv("GIT_COMMITTER_EMAIL", "merge@example.com")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv(isolate.RequestRootEnv, t.TempDir())
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

	res, err := isolate.Create(repo, "asm", isolate.CreateOptions{
		Backend: isolate.BackendGit, Onto: "HEAD", Role: isolate.RoleAssembly,
	})
	if err != nil || res.Status != "created" {
		t.Fatalf("create asm: %+v, %v", res, err)
	}
	asm = res.Path

	res, err = isolate.Create(asm, "feat", isolate.CreateOptions{
		Backend: isolate.BackendGit, Onto: "HEAD", Role: isolate.RoleIntegration, Parent: asm,
	})
	if err != nil || res.Status != "created" {
		t.Fatalf("create feat: %+v, %v", res, err)
	}
	feat = res.Path
	landWrite(t, filepath.Join(feat, "c.txt"), "c\n")
	if err := gitx.CommitAll(feat, "feat: add c"); err != nil {
		t.Fatal(err)
	}
	return repo, asm, feat
}

// runIsolate executes cmd with args and returns its stdout and exit code.
func runIsolate(t *testing.T, cmd *cobra.Command, args ...string) (string, int) {
	t.Helper()
	var buf bytes.Buffer
	cmd.SetArgs(args)
	cmd.SetOut(&buf)
	cmd.SetErr(io.Discard)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	if err := cmd.Execute(); err != nil {
		return buf.String(), asExitError(cmd, err).Code
	}
	return buf.String(), 0
}

// usageErrOf executes cmd expecting a usage error and returns its message and
// exit code.
func usageErrOf(t *testing.T, cmd *cobra.Command, args ...string) (string, int) {
	t.Helper()
	cmd.SetArgs(args)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("%s %v: no error", cmd.Name(), args)
	}
	e := asExitError(cmd, err)
	return e.Msg, e.Code
}

func TestMergeSquashesIntegrationIntoAssembly(t *testing.T) {
	repo, asm, _ := mergeTestRepo(t)
	out, code := runIsolate(t, newIsolateMergeCmd(),
		"--task", "feat", "--repo", repo, "-m", "feat: add c", "-m", "body")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if res["status"] != "merged" {
		t.Fatalf("status: %v", res)
	}
	if got := strings.TrimSpace(landGit(t, asm, "log", "-1", "--format=%B")); got != "feat: add c\n\nbody" {
		t.Errorf("assembly message %q", got)
	}
}

func TestMergeUnderAutoLandsOnFeatureBranch(t *testing.T) {
	repo, _, _ := mergeTestRepo(t)
	pinLandMode(t, "auto")
	out, code := runIsolate(t, newIsolateMergeCmd(),
		"--task", "feat", "--repo", repo, "-m", "feat: add c")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var res struct {
		Status string `json:"status"`
		Land   struct {
			Status  string `json:"status"`
			Branch  string `json:"branch"`
			Created bool   `json:"created"`
		} `json:"land"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if res.Status != "merged" || res.Land.Status != "landed" || res.Land.Branch != "feature/feat" || !res.Land.Created {
		t.Errorf("result %+v", res)
	}
	if head := strings.TrimSpace(landGit(t, repo, "symbolic-ref", "--short", "HEAD")); head != "feature/feat" {
		t.Errorf("HEAD %q, want feature/feat", head)
	}
}

func TestMergeUnderAskLandsNothing(t *testing.T) {
	repo, _, _ := mergeTestRepo(t)
	out, code := runIsolate(t, newIsolateMergeCmd(),
		"--task", "feat", "--repo", repo, "-m", "feat: add c")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var res struct {
		Status string `json:"status"`
		Land   struct {
			Status string `json:"status"`
		} `json:"land"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if res.Status != "merged" || res.Land.Status != "ask" {
		t.Errorf("result %+v", res)
	}
	if head := strings.TrimSpace(landGit(t, repo, "symbolic-ref", "--short", "HEAD")); head != "main" {
		t.Errorf("HEAD %q, want main", head)
	}
	if branches := strings.TrimSpace(landGit(t, repo, "branch", "--list", "feature/*")); branches != "" {
		t.Errorf("feature branches %q, want none", branches)
	}
}

func TestMergeNeedsMessage(t *testing.T) {
	repo, _, _ := mergeTestRepo(t)
	msg, code := usageErrOf(t, newIsolateMergeCmd(), "--task", "feat", "--repo", repo)
	if code != 2 {
		t.Errorf("missing -m: exit %d (%q), want 2", code, msg)
	}
}

func TestMergeAssemblyIsRefused(t *testing.T) {
	repo, _, _ := mergeTestRepo(t)
	out, code := runIsolate(t, newIsolateMergeCmd(),
		"--task", "asm", "--repo", repo, "-m", "subject")
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, out)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if res["status"] != "refused" {
		t.Errorf("status: %v", res)
	}
}

func TestMergeTextPrintsMergedLine(t *testing.T) {
	repo, asm, _ := mergeTestRepo(t)
	out, code := runIsolate(t, newIsolateMergeCmd(),
		"--task", "feat", "--repo", repo, "-m", "subject", "--text")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	head := strings.TrimSpace(landGit(t, asm, "rev-parse", "HEAD"))
	// mergeTestRepo pins isolation.land to ask, so the merge line carries the ask note.
	want := "merged " + head[:8] + " into asm: 1 files (from feat); ask: isolation.land is ask: " +
		"when the user asks, land it from their checkout with 'agent-exec isolate land --request asm -m <message>'\n"
	if out != want {
		t.Errorf("text %q, want %q", out, want)
	}
}

func TestCreateParentMakesWorkTreeUnderIntegration(t *testing.T) {
	repo, _, feat := mergeTestRepo(t)
	out, code := runIsolate(t, newIsolateSimpleCmd("create", "create"),
		"--task", "t1", "--parent", "feat", "--repo", repo)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if res["status"] != "created" || res["role"] != "work" {
		t.Errorf("result: %v", res)
	}
	if res["parent"] != feat {
		t.Errorf("parent %v, want %s", res["parent"], feat)
	}
}

func TestCreateUnknownParentIsUsageError(t *testing.T) {
	repo, _, _ := mergeTestRepo(t)
	const want = "isolate create: --parent nope: no such tree"
	msg, code := usageErrOf(t, newIsolateSimpleCmd("create", "create"),
		"--task", "x", "--parent", "nope", "--repo", repo)
	if code != 2 || msg != want {
		t.Errorf("exit %d %q, want 2 %q", code, msg, want)
	}
}
