package integrate

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
)

// newRepo is a throwaway repository under a hermetic git config, with a
// nine-line a.txt and node_modules/ ignored.
func newRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "T")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "T")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORCHESTRA_ALIVE_DIR", filepath.Join(tmp, "alive"))
	repo := filepath.Join(tmp, "repo")
	writeFile(t, filepath.Join(repo, "a.txt"), "1\n2\n3\n4\n5\n6\n7\n8\n9\n")
	writeFile(t, filepath.Join(repo, ".gitignore"), "node_modules/\n")
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "init")
	return repo
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// task creates a task worktree and writes files into it.
func task(t *testing.T, repo, id string, files map[string]string) string {
	t.Helper()
	res, err := isolate.Create(repo, id, isolate.CreateOptions{Backend: isolate.BackendGit})
	if err != nil || res.Status != "created" {
		t.Fatalf("create %s: %+v %v", id, res, err)
	}
	for name, content := range files {
		writeFile(t, filepath.Join(res.Path, name), content)
	}
	return res.Path
}

func TestResultNeverCarriesPatchText(t *testing.T) {
	repo := newRepo(t)
	task(t, repo, "t1", map[string]string{"a.txt": "1\nSECRET-LINE\n3\n4\n5\n6\n7\n8\n9\n"})
	task(t, repo, "t2", map[string]string{"a.txt": "1\nOTHER-SECRET\n3\n4\n5\n6\n7\n8\n9\n"})
	before := git(t, repo, "status", "--porcelain")
	r, err := Integrate(repo, []string{"t1", "t2"}, Options{})
	if err != nil || r.Status != "conflicted" {
		t.Fatalf("integrate: %+v %v", r, err)
	}
	b, _ := json.Marshal(r)
	for _, out := range []string{string(b), FormatText(r)} {
		if strings.Contains(out, "SECRET") || strings.Contains(out, "@@") {
			t.Errorf("output carries diff text: %s", out)
		}
	}
	if after := git(t, repo, "status", "--porcelain"); after != before {
		t.Errorf("user tree changed: %q -> %q", before, after)
	}
}

type recorder struct{ lines []string }

func (r *recorder) Write(b []byte) (int, error) {
	r.lines = append(r.lines, string(b))
	return len(b), nil
}

type failing struct{}

func (failing) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestProgressEventsAndAFailingWriter(t *testing.T) {
	repo := newRepo(t)
	task(t, repo, "t1", map[string]string{"b.txt": "b\n"})
	rec := &recorder{}
	r, err := Integrate(repo, []string{"t1", "ghost"}, Options{Verify: "true", Progress: rec})
	if err != nil || r.Status != "ok" {
		t.Fatalf("integrate: %+v %v", r, err)
	}
	var events []string
	for _, line := range rec.lines {
		if !strings.HasSuffix(line, "\n") || strings.Count(line, "\n") != 1 {
			t.Fatalf("not one line per write: %q", line)
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatal(err)
		}
		events = append(events, ev["event"].(string))
	}
	want := "integrate-start integrate-task integrate-task verify-start verify-end integrate-end"
	if got := strings.Join(events, " "); got != want {
		t.Errorf("events %q, want %q", got, want)
	}

	r2, err := Integrate(repo, []string{"t1"}, Options{Into: "other", Verify: "true", Progress: failing{}})
	if err != nil || r2.Status != "ok" || r2.Verify == nil || r2.Verify.Status != "pass" {
		t.Errorf("a failing progress writer changed the result: %+v %v", r2, err)
	}
}

func TestVerifyResetsTrackedAndUntrackedButKeepsIgnored(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, filepath.Join(repo, "node_modules", "dep.js"), "dep\n")
	v := RunVerify(repo, "echo x >> a.txt; echo y > junk.txt; echo z > node_modules/built.js", 0)
	if v.Status != "pass" || v.Dirtied != 2 || v.Exit == nil || *v.Exit != 0 {
		t.Fatalf("verify: %+v", v)
	}
	if s := git(t, repo, "status", "--porcelain"); s != "" {
		t.Errorf("tree left dirty: %q", s)
	}
	if _, err := os.Stat(filepath.Join(repo, "node_modules", "built.js")); err != nil {
		t.Errorf("ignored output removed: %v", err)
	}
}

func TestVerifyTimeoutKillsTheProcessGroup(t *testing.T) {
	repo := newRepo(t)
	start := time.Now()
	v := RunVerify(repo, "sleep 30 & sleep 30", 200*time.Millisecond)
	if !v.TimedOut || v.Exit != nil || v.Status != "fail" {
		t.Fatalf("verify: %+v", v)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("waited %s for a killed group", time.Since(start))
	}
}

// TestBisectAbortsAConflictingRevert: the culprit's file was changed again
// by a later commit, so reverting it conflicts; the revert is aborted, the
// task flagged, and the tree left clean on its branch.
func TestBisectAbortsAConflictingRevert(t *testing.T) {
	repo := newRepo(t)
	git(t, repo, "checkout", "-q", "-b", "int")
	base := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))
	writeFile(t, filepath.Join(repo, "bad.txt"), "bad\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "bad")
	bad := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))
	writeFile(t, filepath.Join(repo, "bad.txt"), "changed later\n")
	git(t, repo, "commit", "-qam", "later")
	later := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))

	var progress bytes.Buffer
	res, err := Bisect(BisectInput{
		Root: repo, Worktree: repo, Branch: "int", BaseSHA: base,
		Applied: []AppliedCommit{{"bad", bad}, {"later", later}},
		Verify:  "test ! -f bad.txt", Progress: &progress,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.RevertConflicted != "bad" || len(res.Reverted) != 0 ||
		res.Verify.Status != "fail" || res.Verify.Baseline != "pass" {
		t.Fatalf("bisect: %+v", res)
	}
	if s := git(t, repo, "status", "--porcelain"); s != "" {
		t.Errorf("tree left mid-revert: %q", s)
	}
	if ref := strings.TrimSpace(git(t, repo, "rev-parse", "--abbrev-ref", "HEAD")); ref != "int" {
		t.Errorf("left on %q, want the branch", ref)
	}
	if !strings.Contains(progress.String(), `"result":"conflicted"`) {
		t.Errorf("no revert-conflict event: %s", progress.String())
	}
	if _, err := Bisect(BisectInput{Worktree: repo}); err == nil {
		t.Error("bisect without a branch and base accepted")
	}
}

// A task adding more new files than the filter allows is refused, not
// applied; the others still land and the run exits 7.
func TestOversizedTaskIsRefused(t *testing.T) {
	repo := newRepo(t)
	task(t, repo, "big", map[string]string{"n1": "1\n", "n2": "2\n", "n3": "3\n"})
	task(t, repo, "fits", map[string]string{"m1": "1\n", "m2": "2\n"})
	r, err := Integrate(repo, []string{"big", "fits"}, Options{Filter: &isolate.ChangeFilter{MaxAdded: 2}})
	if err != nil || r.Status != "refused" || ExitCode(r) != 7 {
		t.Fatalf("integrate: %+v %v", r, err)
	}
	if got := r.Tasks[0]; got.Status != "refused" || got.Reason != "adds 3 new files (isolation.max_added_files 2)" {
		t.Errorf("big: %+v", got)
	}
	if got := r.Tasks[1]; got.Status != "applied" {
		t.Errorf("fits: %+v", got)
	}
	wt := string(r.Integration.Path)
	if _, err := os.Stat(filepath.Join(wt, "n1")); err == nil {
		t.Error("the refused task was applied")
	}
	if _, err := os.Stat(filepath.Join(wt, "m1")); err != nil {
		t.Errorf("the fitting task is missing: %v", err)
	}
}
