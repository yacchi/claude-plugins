package orcanest

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestParentIsTheBaseBranchTreeButNeverTheChild(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	tmp, _ := filepath.EvalSymlinks(t.TempDir())
	repo := filepath.Join(tmp, "repo")
	_ = os.MkdirAll(repo, 0o755)
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "-c", "user.email=t@example.com", "-c", "user.name=T", "commit", "-q", "--allow-empty", "-m", "init")
	integ, child := filepath.Join(tmp, "integ"), filepath.Join(tmp, "child")
	git(t, repo, "worktree", "add", "-q", "-b", "integ", integ)
	git(t, repo, "worktree", "add", "-q", "-b", "pkg", child)
	if got := Parent(repo, "refs/heads/integ", child); got != integ {
		t.Errorf("lineage broken: %s", got)
	}
	if got := Parent(repo, "pkg", child); got != repo {
		t.Errorf("nested under itself: %s", got)
	}
}

func TestAHangingOrcaIsCutShortAndWarnedOnce(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "orca")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ResetWarning(t)
	var stderr bytes.Buffer
	n := &Nester{Binary: bin, Stderr: &stderr, Timeout: 100 * time.Millisecond}
	start := time.Now()
	n.Nest(t.TempDir(), "", "/x/one")
	n.Nest(t.TempDir(), "", "/x/two")
	if time.Since(start) > 10*time.Second {
		t.Error("the timeout did not cut orca short")
	}
	if got := bytes.Count(stderr.Bytes(), []byte("could not nest")); got != 1 {
		t.Errorf("want one warning, got %d: %s", got, stderr.String())
	}
}
