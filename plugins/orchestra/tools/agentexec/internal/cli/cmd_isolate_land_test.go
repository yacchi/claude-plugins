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
)

// landTestRepo is a throwaway checkout on main with README.md committed, and
// an integration tree "prog" holding two commits (a.txt and c.txt) on top of
// the base, planted the way the wave runner does.
func landTestRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "Land User")
	t.Setenv("GIT_AUTHOR_EMAIL", "land@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Land User")
	t.Setenv("GIT_COMMITTER_EMAIL", "land@example.com")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	alive := filepath.Join(tmp, "alive")
	t.Setenv("ORCHESTRA_ALIVE_DIR", alive)
	isolate.SetAliveDir(alive)
	t.Cleanup(func() { isolate.SetAliveDir("") })

	repo := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	landGit(t, repo, "init", "-q", "-b", "main")
	landWrite(t, filepath.Join(repo, "README.md"), "hello\n")
	landWrite(t, filepath.Join(repo, "a.txt"), "one\ntwo\nthree\n")
	landGit(t, repo, "add", "-A")
	landGit(t, repo, "commit", "-q", "-m", "init")
	head := strings.TrimSpace(landGit(t, repo, "rev-parse", "HEAD"))

	res, err := isolate.Create(repo, "prog", isolate.CreateOptions{Backend: isolate.BackendGit, Onto: head})
	if err != nil || res.Status != "created" {
		t.Fatalf("create: %+v, %v", res, err)
	}
	landWrite(t, filepath.Join(res.Path, "a.txt"), "ONE\ntwo\nthree\n")
	if err := gitx.CommitAll(res.Path, "orchestra integrate pa"); err != nil {
		t.Fatal(err)
	}
	landWrite(t, filepath.Join(res.Path, "c.txt"), "c\n")
	if err := gitx.CommitAll(res.Path, "orchestra integrate pb"); err != nil {
		t.Fatal(err)
	}
	return repo
}

func landGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitx.Run(dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return out
}

func landWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// runLand executes `isolate land` with args and returns its stdout and exit code.
func runLand(t *testing.T, args ...string) (string, int) {
	t.Helper()
	var buf bytes.Buffer
	cmd := newIsolateLandCmd()
	cmd.SetArgs(args)
	cmd.SetOut(&buf)
	cmd.SetErr(io.Discard)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	if err := cmd.Execute(); err != nil {
		return buf.String(), asExitError(cmd, err).Code
	}
	return buf.String(), 0
}

func TestLandJoinsMessageParagraphs(t *testing.T) {
	repo := landTestRepo(t)
	out, code := runLand(t, "--task", "prog", "--repo", repo, "-m", "subject line", "-m", "body text")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if res["status"] != "landed" {
		t.Fatalf("status: %v", res)
	}
	if got := strings.TrimSpace(landGit(t, repo, "log", "-1", "--format=%B")); got != "subject line\n\nbody text" {
		t.Errorf("landed message %q", got)
	}
}

func TestLandNeedsMessage(t *testing.T) {
	repo := landTestRepo(t)
	if _, code := runLand(t, "--task", "prog", "--repo", repo); code != 2 {
		t.Errorf("missing -m: exit %d, want 2", code)
	}
}

func TestLandDirtyCheckoutExitsOne(t *testing.T) {
	repo := landTestRepo(t)
	landWrite(t, filepath.Join(repo, "README.md"), "edited by hand\n")
	out, code := runLand(t, "--task", "prog", "--repo", repo, "-m", "subject")
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, out)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if res["status"] != "dirty" {
		t.Errorf("status: %v", res)
	}
}

func TestLandTextPrintsLandedLine(t *testing.T) {
	repo := landTestRepo(t)
	out, code := runLand(t, "--task", "prog", "--repo", repo, "-m", "subject", "--text")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	head := strings.TrimSpace(landGit(t, repo, "rev-parse", "HEAD"))
	want := "landed " + head[:8] + " on main: 2 files (from prog)\n"
	if out != want {
		t.Errorf("text %q, want %q", out, want)
	}
}

// landUsageErr runs `isolate land` expecting a usage error and returns its
// message and exit code.
func landUsageErr(t *testing.T, args ...string) (string, int) {
	t.Helper()
	cmd := newIsolateLandCmd()
	cmd.SetArgs(args)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("land %v: no error", args)
	}
	e := asExitError(cmd, err)
	return e.Msg, e.Code
}

func TestLandNeedsExactlyOneSource(t *testing.T) {
	const want = "isolate land: give exactly one of --task and --path"
	for _, args := range [][]string{
		{"-m", "subject"},
		{"--task", "prog", "--path", "/tmp/x", "-m", "subject"},
	} {
		if msg, code := landUsageErr(t, args...); code != 2 || msg != want {
			t.Errorf("land %v: exit %d %q, want 2 %q", args, code, msg, want)
		}
	}
}

func TestLandBaselineNeedsPath(t *testing.T) {
	const want = "isolate land: --baseline goes with --path"
	msg, code := landUsageErr(t, "--task", "prog", "--baseline", "main", "-m", "subject")
	if code != 2 || msg != want {
		t.Errorf("exit %d %q, want 2 %q", code, msg, want)
	}
}

func TestLandPathNeedsMessage(t *testing.T) {
	if _, code := landUsageErr(t, "--path", "/tmp/x"); code != 2 {
		t.Errorf("missing -m: exit %d, want 2", code)
	}
}

func TestLandPathTextNamesPath(t *testing.T) {
	repo := landTestRepo(t)
	wt := filepath.Join(filepath.Dir(repo), "agent-x")
	landGit(t, repo, "worktree", "add", "-q", "-b", "worktree-agent-x", wt, "main")
	landWrite(t, filepath.Join(wt, "d.txt"), "d\n")
	out, code := runLand(t, "--path", wt, "--repo", repo, "-m", "subject", "--text")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !strings.HasPrefix(out, "landed ") || !strings.Contains(out, "on main: 1 files (from "+wt+")") {
		t.Errorf("text %q does not name the path %s", out, wt)
	}
}
