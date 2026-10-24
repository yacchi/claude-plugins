package integrate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
)

// hook installs an executable hook into repo's shared hooks dir.
func hook(t *testing.T, repo, name, body string) {
	t.Helper()
	path := filepath.Join(repo, ".git", "hooks", name)
	writeFile(t, path, "#!/bin/sh\n"+body+"\n")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// failCommitOf puts a git in front of PATH that refuses the commit whose
// message is message (after the add staged it) and runs the real git
// otherwise.
func failCommitOf(t *testing.T, message string) {
	t.Helper()
	commitShim(t, message, "echo 'error: forced commit failure' >&2; exit 1")
}

// commitShim puts a git in front of PATH that runs action ($real is the
// real git) for the call carrying message, and the real git otherwise.
func commitShim(t *testing.T, message, action string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "git"), "#!/bin/sh\nreal='"+real+"'\nfor a in \"$@\"; do\n"+
		"  if [ \"$a\" = '"+message+"' ]; then "+action+"; fi\n"+
		"done\nexec \"$real\" \"$@\"\n")
	if err := os.Chmod(filepath.Join(dir, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// collected reports whether wt carries isolate's collected marker.
func collected(t *testing.T, wt string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(strings.TrimSpace(git(t, wt, "rev-parse", "--absolute-git-dir")), "orchestra-collected"))
	return err == nil
}

func integrateSubjects(t *testing.T, wt, since string) []string {
	t.Helper()
	return strings.Fields(strings.ReplaceAll(git(t, wt, "log", "--format=%s", since+"..HEAD"), "orchestra integrate ", ""))
}

func TestCommitsBypassTheRepositoryHooks(t *testing.T) {
	repo := newRepo(t)
	hook(t, repo, "pre-commit", "echo 'lint failed' >&2; exit 1")
	// lefthook's prepare-commit-msg runs despite --no-verify unless LEFTHOOK=0.
	hook(t, repo, "prepare-commit-msg", `[ "$LEFTHOOK" = 0 ] || exit 1`)
	task(t, repo, "t1", map[string]string{"b.txt": "b\n"})
	r, err := Integrate(repo, []string{"t1"}, Options{})
	if err != nil || r.Status != "ok" || r.Tasks[0].Status != "applied" {
		t.Fatalf("integrate: %+v %v", r, err)
	}
	wt := string(r.Integration.Path)
	if got := integrateSubjects(t, wt, r.Onto); len(got) != 1 || got[0] != "t1" {
		t.Errorf("integrate commits %v", got)
	}
}

func TestFailedCommitRollsBackAndTheNextTaskCommits(t *testing.T) {
	repo := newRepo(t)
	src := task(t, repo, "t1", map[string]string{"b.txt": "b\n"})
	src2 := task(t, repo, "t2", map[string]string{"c.txt": "c\n"})
	failCommitOf(t, "orchestra integrate t1")
	r, err := Integrate(repo, []string{"t1", "t2"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "commit-failed" || ExitCode(r) != 8 {
		t.Errorf("status %q exit %d", r.Status, ExitCode(r))
	}
	t1, t2 := r.Tasks[0], r.Tasks[1]
	if t1.Status != "commit-failed" || !t1.RolledBack || !strings.Contains(t1.Reason, "forced commit failure") {
		t.Errorf("t1 %+v", t1)
	}
	if t2.Status != "applied" {
		t.Errorf("t2 %+v", t2)
	}
	wt := string(r.Integration.Path)
	if got := integrateSubjects(t, wt, r.Onto); len(got) != 1 || got[0] != "t2" {
		t.Errorf("integrate commits %v", got)
	}
	if st := git(t, wt, "status", "--porcelain"); st != "" {
		t.Errorf("integration tree left dirty: %q", st)
	}
	if _, err := os.Stat(filepath.Join(wt, "b.txt")); err == nil {
		t.Error("t1's file stayed in the integration tree")
	}
	if collected(t, src) || !collected(t, src2) {
		t.Errorf("collected t1=%v t2=%v, want false, true", collected(t, src), collected(t, src2))
	}
}

func TestTimedOutCommitRemovesItsIndexLockAndRollsBack(t *testing.T) {
	repo := newRepo(t)
	task(t, repo, "t1", map[string]string{"b.txt": "b\n"})
	task(t, repo, "t2", map[string]string{"c.txt": "c\n"})
	// The commit takes the index lock, then hangs past the deadline.
	commitShim(t, "orchestra integrate t1",
		`touch "$("$real" rev-parse --absolute-git-dir)/index.lock"; exec sleep 30`)
	saved := gitx.CommitTimeout
	gitx.CommitTimeout = time.Second
	t.Cleanup(func() { gitx.CommitTimeout = saved })
	r, err := Integrate(repo, []string{"t1", "t2"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t1, t2 := r.Tasks[0], r.Tasks[1]
	if t1.Status != "commit-failed" || !t1.RolledBack || !strings.Contains(t1.Reason, "timed out") {
		t.Errorf("t1 %+v", t1)
	}
	if t2.Status != "applied" {
		t.Errorf("t2 %+v", t2)
	}
	wt := string(r.Integration.Path)
	if got := integrateSubjects(t, wt, r.Onto); len(got) != 1 || got[0] != "t2" {
		t.Errorf("integrate commits %v", got)
	}
	if st := git(t, wt, "status", "--porcelain"); st != "" {
		t.Errorf("integration tree left dirty: %q", st)
	}
}
