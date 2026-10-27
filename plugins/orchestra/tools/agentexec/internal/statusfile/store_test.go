package statusfile

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
)

func TestRunKeyFollowsSymlinks(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state.json")
	if err := os.WriteFile(state, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(state, link); err != nil {
		t.Fatal(err)
	}

	key := RunKey(state)
	if got := RunKey(link); got != key {
		t.Errorf("RunKey(symlink) = %s, want %s", got, key)
	}
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(key) {
		t.Errorf("RunKey = %q, want 16 lowercase hex characters", key)
	}
	if other := RunKey(filepath.Join(dir, "other.json")); other == key {
		t.Errorf("RunKey of another path = %s, same as %s", other, key)
	}
}

func TestRebuildRoundTrip(t *testing.T) {
	s := Store{Root: t.TempDir()}
	now := time.Now()
	live := Run{Key: "0123456789abcdef", PID: os.Getpid(), Status: "running", UpdatedAt: now.Unix()}
	dead := Run{Key: "fedcba9876543210", PID: 0, Status: "running", UpdatedAt: now.Unix()}
	plan := Plan{PlanID: "plan-1", Status: "planning", UpdatedAt: now.Unix()}
	for _, r := range []Run{live, dead} {
		if err := s.WriteRun(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.WritePlan(plan); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Rebuild(now); err != nil {
		t.Fatal(err)
	}

	var doc Doc
	if err := fsutil.ReadJSONFile(s.Path(), &doc); err != nil {
		t.Fatal(err)
	}
	status := map[string]string{}
	for _, r := range doc.Runs {
		status[r.Key] = r.Status
	}
	if status[live.Key] != "running" {
		t.Errorf("live run status = %q, want running", status[live.Key])
	}
	if status[dead.Key] != "crashed" {
		t.Errorf("dead run status = %q, want crashed", status[dead.Key])
	}
	if len(doc.Plans) != 1 || doc.Plans[0].PlanID != plan.PlanID {
		t.Errorf("plans = %+v, want the one written plan", doc.Plans)
	}
}

func TestRebuildPrunes(t *testing.T) {
	s := Store{Root: t.TempDir()}
	now := time.Now()
	old := now.Add(-8 * 24 * time.Hour)
	if err := s.WriteRun(Run{Key: "aaaaaaaaaaaaaaaa", PID: os.Getpid(), Status: "done", UpdatedAt: old.Unix()}); err != nil {
		t.Fatal(err)
	}
	writeGarbage(t, s.RunsDir(), "xx.json", old)
	writeGarbage(t, s.RunsDir(), "yy.json", now)
	writeGarbage(t, s.RunsDir(), ".tmp.json", old)

	doc, err := s.Rebuild(now)
	if err != nil {
		t.Fatal(err)
	}

	if len(doc.Runs) != 0 {
		t.Errorf("runs = %+v, want the expired run dropped", doc.Runs)
	}
	for _, name := range []string{"aaaaaaaaaaaaaaaa.json", "xx.json"} {
		if _, err := os.Stat(filepath.Join(s.RunsDir(), name)); !os.IsNotExist(err) {
			t.Errorf("%s still exists after pruning (err %v)", name, err)
		}
	}
	for _, name := range []string{"yy.json", ".tmp.json"} {
		if _, err := os.Stat(filepath.Join(s.RunsDir(), name)); err != nil {
			t.Errorf("%s was removed: %v", name, err)
		}
	}
}

func TestWriteRejectsBadNames(t *testing.T) {
	s := Store{Root: t.TempDir()}
	if err := s.WriteRun(Run{Key: "../evil"}); err == nil {
		t.Error("WriteRun accepted key ../evil")
	}
	if err := s.WritePlan(Plan{PlanID: "a/b"}); err == nil {
		t.Error("WritePlan accepted id a/b")
	}
	for _, dir := range []string{s.RunsDir(), s.PlansDir()} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s was created by a rejected write (err %v)", dir, err)
		}
	}
}

func TestProjectFromRemote(t *testing.T) {
	cases := map[string]string{
		"git@github.com:yacchi/claude-plugins.git":      "yacchi/claude-plugins",
		"https://github.com/yacchi/claude-plugins":      "yacchi/claude-plugins",
		"https://github.com/yacchi/claude-plugins.git/": "yacchi/claude-plugins",
		"ssh://git@example.com:2222/group/sub/repo.git": "sub/repo",
	}
	for url, want := range cases {
		if got := projectFromRemote(url); got != want {
			t.Errorf("projectFromRemote(%q) = %q, want %q", url, got, want)
		}
	}
}

func TestProjectNamesRepo(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "my-repo")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	if got := Project(dir); got != "my-repo" {
		t.Errorf("Project without remote = %q, want my-repo", got)
	}
	git("remote", "add", "origin", "git@github.com:o/r.git")
	if got := Project(dir); got != "o/r" {
		t.Errorf("Project with remote = %q, want o/r", got)
	}
}

func writeGarbage(t *testing.T, dir, name string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, name)
	if err := os.WriteFile(file, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}
