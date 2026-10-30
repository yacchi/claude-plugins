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
// an assembly "prog" holding two commits (a.txt and c.txt) on top of the
// base, planted the way the wave runner does.
func landTestRepo(t *testing.T) string {
	t.Helper()
	pinLandMode(t, "ask")
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
	t.Setenv("ORCHESTRA_REQUEST_ROOT", filepath.Join(tmp, "requests"))
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

	res, err := isolate.Create(repo, "prog", isolate.CreateOptions{Backend: isolate.BackendGit, Onto: head, Role: isolate.RoleAssembly})
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

// pinLandMode sets isolation.land to mode through a project config of its own,
// so the test neither reads the user's config nor dirties the checkout.
func pinLandMode(t *testing.T, mode string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	landWrite(t, filepath.Join(dir, ".claude", "orchestra.yaml"), "isolation:\n  land: "+mode+"\n")
	t.Setenv("ORCHESTRA_CONFIG_ROOT", dir)
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

func TestLandRefusesAnIntegrationTree(t *testing.T) {
	repo := landTestRepo(t)
	res, err := isolate.Create(repo, "int", isolate.CreateOptions{Backend: isolate.BackendGit, Role: isolate.RoleIntegration})
	if err != nil || res.Status != "created" {
		t.Fatalf("create: %+v, %v", res, err)
	}
	out, code := runLand(t, "--task", "int", "--repo", repo, "-m", "subject")
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if got["status"] != "refused" {
		t.Errorf("status: %v", got)
	}
	if reason, _ := got["reason"].(string); !strings.Contains(reason, "is an integration tree with no assembly") {
		t.Errorf("reason %q", reason)
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
	want := "landed " + head[:8] + " on main: 2 files (from prog); assembly removed\n"
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

func TestLandAcceptsAtMostOneSource(t *testing.T) {
	const want = "isolate land: give at most one of --task, --request and --path"
	for _, args := range [][]string{
		{"--task", "prog", "--path", "/tmp/x", "-m", "subject"},
		{"--task", "a", "--request", "b", "-m", "subject"},
		{"--request", "a", "--path", "/tmp/x", "-m", "subject"},
	} {
		if msg, code := landUsageErr(t, args...); code != 2 || msg != want {
			t.Errorf("land %v: exit %d %q, want 2 %q", args, code, msg, want)
		}
	}
}

// landRequest makes the request assembly of repo for the given Claude Code
// session, the way the session's own commands do, and returns the request id.
func landRequest(t *testing.T, repo, session string) string {
	t.Helper()
	t.Setenv("CLAUDE_CODE_SESSION_ID", session)
	res, err := isolate.EnsureAssembly(repo, "")
	if err != nil || res.Status != "ready" {
		t.Fatalf("ensure assembly for %s: %+v, %v", session, res, err)
	}
	return res.Task
}

// landRequestChange commits a file in the request's assembly so it has
// something to land.
func landRequestChange(t *testing.T, repo, request string) {
	t.Helper()
	res, err := isolate.EnsureAssembly(repo, "")
	if err != nil || res.Task != request {
		t.Fatalf("assembly of %s: %+v, %v", request, res, err)
	}
	landWrite(t, filepath.Join(res.Assembly, "feature.txt"), "feature\n")
	if err := gitx.CommitAll(res.Assembly, "feat: x"); err != nil {
		t.Fatal(err)
	}
}

func TestLandWithoutRequestIsAbsent(t *testing.T) {
	repo := landTestRepo(t)
	out, code := runLand(t, "--repo", repo, "-m", "x")
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, out)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if res["status"] != "absent" || res["reason"] != "no open request has an assembly for this repository" {
		t.Errorf("result %v", res)
	}
}

func TestLandTakesTheSessionsRequestAssembly(t *testing.T) {
	repo := landTestRepo(t)
	request := landRequest(t, repo, "aaaaaaaa-1111-4111-8111-111111111111")
	landRequestChange(t, repo, request)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "aaaaaaaa-1111-4111-8111-111111111111")

	out, code := runLand(t, "--repo", repo, "-m", "feat: x", "--text")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !strings.HasSuffix(out, "; assembly removed; request "+request+" ended\n") {
		t.Errorf("text %q does not end with the removal and request end", out)
	}
	if got := strings.TrimSpace(landGit(t, repo, "log", "-1", "--format=%s")); got != "feat: x" {
		t.Errorf("landed subject %q", got)
	}
}

func TestLandRequestLandsItsAssemblyAsJSON(t *testing.T) {
	repo := landTestRepo(t)
	request := landRequest(t, repo, "aaaaaaaa-1111-4111-8111-111111111111")
	landRequestChange(t, repo, request)

	out, code := runLand(t, "--request", request, "--repo", repo, "-m", "feat: x")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if res["status"] != "landed" || res["removed"] != true || res["request"] != request || res["request_ended"] != true {
		t.Errorf("result %v", res)
	}
}

func TestLandRefusesSeveralRequestsAndRequestPicksOne(t *testing.T) {
	repo := landTestRepo(t)
	first := landRequest(t, repo, "aaaaaaaa-1111-4111-8111-111111111111")
	landRequestChange(t, repo, first)
	second := landRequest(t, repo, "bbbbbbbb-2222-4222-8222-222222222222")
	landRequestChange(t, repo, second)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "cccccccc-3333-4333-8333-333333333333")

	out, code := runLand(t, "--repo", repo, "-m", "feat: x")
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, out)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	reason, _ := res["reason"].(string)
	if res["status"] != "refused" || !strings.Contains(reason, "several requests have an assembly") ||
		!strings.Contains(reason, first) || !strings.Contains(reason, second) {
		t.Errorf("result %v", res)
	}

	out, code = runLand(t, "--request", first, "--repo", repo, "-m", "feat: x")
	if code != 0 {
		t.Fatalf("--request %s: exit %d: %s", first, code, out)
	}
	if !strings.Contains(out, `"request":"`+first+`"`) {
		t.Errorf("landed %q, want request %s", out, first)
	}
}

func TestLandBaselineWithRequestIsUsageError(t *testing.T) {
	const want = "isolate land: --baseline goes with --path"
	msg, code := landUsageErr(t, "--baseline", "x", "--request", "y", "-m", "subject")
	if code != 2 || msg != want {
		t.Errorf("exit %d %q, want 2 %q", code, msg, want)
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

func TestLandUnderAutoMakesFeatureBranchFromMain(t *testing.T) {
	repo := landTestRepo(t)
	pinLandMode(t, "auto")
	out, code := runLand(t, "--task", "prog", "--repo", repo, "-m", "feat: x", "--feature", "my-work")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if res["status"] != "landed" || res["branch"] != "feature/my-work" || res["created"] != true {
		t.Errorf("result %v", res)
	}
	if head := strings.TrimSpace(landGit(t, repo, "symbolic-ref", "--short", "HEAD")); head != "feature/my-work" {
		t.Errorf("HEAD %q, want feature/my-work", head)
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
