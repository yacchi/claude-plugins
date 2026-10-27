package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// waveRunReqEnv pins every path the command could write under, so a test can
// only reach its own temp dirs. It returns the request root.
func waveRunReqEnv(t *testing.T) string {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Join(tmp, "home"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(tmp, "claude"))
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "Wave User")
	t.Setenv("GIT_AUTHOR_EMAIL", "wave@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Wave User")
	t.Setenv("GIT_COMMITTER_EMAIL", "wave@example.com")
	root := filepath.Join(tmp, "requests")
	t.Setenv(isolate.RequestRootEnv, root)
	alive := filepath.Join(tmp, "alive")
	t.Setenv("ORCHESTRA_ALIVE_DIR", alive)
	isolate.SetAliveDir(alive)
	t.Cleanup(func() { isolate.SetAliveDir("") })
	return root
}

// waveRunReqRepo is a checkout on main with README.md committed, and a
// tracked edit to it when dirty.
func waveRunReqRepo(t *testing.T, dirty bool) string {
	t.Helper()
	waveRunReqEnv(t)
	repo := filepath.Join(paths.RealPath(t.TempDir()), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	landGit(t, repo, "init", "-q", "-b", "main")
	landWrite(t, filepath.Join(repo, "README.md"), "hello\n")
	landGit(t, repo, "add", "-A")
	landGit(t, repo, "commit", "-q", "-m", "init")
	if dirty {
		landWrite(t, filepath.Join(repo, "README.md"), "edited\n")
	}
	return repo
}

// runWaveRunReq executes `wave run` with args and returns its stdout and exit code.
func runWaveRunReq(t *testing.T, args ...string) (string, int) {
	t.Helper()
	var buf bytes.Buffer
	cmd := newWaveCmd()
	cmd.SetArgs(append([]string{"run"}, args...))
	cmd.SetOut(&buf)
	cmd.SetErr(io.Discard)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	if err := cmd.Execute(); err != nil {
		return buf.String(), asExitError(cmd, err).Code
	}
	return buf.String(), 0
}

// waveRunReqArgs is a `wave run` that never gets past the pre-flight or the
// plan load: its plan file does not exist.
func waveRunReqArgs(t *testing.T, repo string, extra ...string) []string {
	t.Helper()
	state := filepath.Join(t.TempDir(), "s", "state.json")
	args := []string{"--plan", filepath.Join(t.TempDir(), "p.json"), "--state", state, "--into", "x", "--repo", repo}
	return append(args, extra...)
}

// waveRunReqAssertNoTree fails when a tree besides repo's own, or a request
// workspace, was created. The request root's .lock is taken even by a look.
func waveRunReqAssertNoTree(t *testing.T, repo, root string) {
	t.Helper()
	list, err := gitx.WorktreeList(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Errorf("worktrees: %v, want only the checkout", list)
	}
	entries, err := os.ReadDir(root)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != ".lock" {
			t.Errorf("request root holds %q, want no request workspace", e.Name())
		}
	}
}

func TestWaveRunRefusesDirtyCheckoutWithoutUncommitted(t *testing.T) {
	repo := waveRunReqRepo(t, true)
	root := os.Getenv(isolate.RequestRootEnv)
	out, code := runWaveRunReq(t, waveRunReqArgs(t, repo)...)
	if code != 3 {
		t.Fatalf("exit %d, want 3: %s", code, out)
	}
	var rep map[string]any
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if rep["status"] != "error" || rep["kind"] != "uncommitted" {
		t.Errorf("report: %v", rep)
	}
	if reason, _ := rep["reason"].(string); !strings.Contains(reason, "README.md") {
		t.Errorf("reason %q does not name the edited path", reason)
	}
	waveRunReqAssertNoTree(t, repo, root)
}

func TestWaveRunUncommittedRefusalTextOutput(t *testing.T) {
	repo := waveRunReqRepo(t, true)
	root := os.Getenv(isolate.RequestRootEnv)
	out, code := runWaveRunReq(t, waveRunReqArgs(t, repo, "--text")...)
	if code != 3 {
		t.Fatalf("exit %d, want 3: %s", code, out)
	}
	line := strings.TrimSuffix(out, "\n")
	if !strings.HasPrefix(line, "wave run error: ") || strings.Contains(line, "\n") {
		t.Errorf("text output %q, want one `wave run error:` line", out)
	}
	if !strings.Contains(line, "README.md") {
		t.Errorf("text output %q does not name the edited path", out)
	}
	waveRunReqAssertNoTree(t, repo, root)
}

func TestWaveRunRejectsUnknownUncommittedValue(t *testing.T) {
	repo := waveRunReqRepo(t, true)
	root := os.Getenv(isolate.RequestRootEnv)
	out, code := runWaveRunReq(t, waveRunReqArgs(t, repo, "--uncommitted", "maybe")...)
	if code != 2 {
		t.Fatalf("exit %d, want 2: %s", code, out)
	}
	waveRunReqAssertNoTree(t, repo, root)
}

func TestWaveRunIgnoreStillFailsOnPlanWithoutCreatingTree(t *testing.T) {
	repo := waveRunReqRepo(t, true)
	root := os.Getenv(isolate.RequestRootEnv)
	out, code := runWaveRunReq(t, waveRunReqArgs(t, repo, "--uncommitted", "ignore")...)
	if code != 2 {
		t.Fatalf("exit %d, want 2: %s", code, out)
	}
	var rep map[string]any
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if rep["kind"] != "plan" {
		t.Errorf("report: %v, want the plan error", rep)
	}
	waveRunReqAssertNoTree(t, repo, root)
}
