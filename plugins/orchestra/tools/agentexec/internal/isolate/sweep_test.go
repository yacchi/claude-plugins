package isolate

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
)

func sweep(t *testing.T, repo string, o SweepOptions) SweepResult {
	t.Helper()
	if o.LiveWindow == 0 {
		o.LiveWindow = DefaultLiveWindow
	}
	r, err := Sweep(repo, o)
	if err != nil || r.Status != "ok" {
		t.Fatalf("sweep: %+v %v", r, err)
	}
	return r
}

func byTask(r SweepResult) map[string]SweepWorktree {
	m := map[string]SweepWorktree{}
	for _, w := range r.Worktrees {
		m[w.Task] = w
	}
	return m
}

func TestSweepKeepsUncollectedWorkAndLeavesOtherTreesAlone(t *testing.T) {
	repo := newRepo(t)
	clean := create(t, repo, "clean", "")
	dirty := create(t, repo, "dirty", "")
	writeFile(t, filepath.Join(dirty, "worker.txt"), "w\n")
	other := filepath.Join(filepath.Dir(repo), "mine")
	git(t, repo, "worktree", "add", "-q", "-b", "feature/mine", other)

	r := sweep(t, repo, SweepOptions{})
	got := byTask(r)
	if got["clean"].Status != "removed" || exists(clean) {
		t.Errorf("clean: %+v", got["clean"])
	}
	if w := got["dirty"]; w.Status != "dirty" || !slices.Equal(w.Files, []string{"worker.txt"}) || !exists(dirty) {
		t.Errorf("dirty: %+v", w)
	}
	if !exists(other) || len(r.Worktrees) != 2 {
		t.Errorf("a non-orchestra worktree was considered: %+v", r.Worktrees)
	}
	if r.Summary.Removed != 1 || r.Summary.Dirty != 1 {
		t.Errorf("summary %+v", r.Summary)
	}
	r = sweep(t, repo, SweepOptions{Force: true})
	if byTask(r)["dirty"].Status != "removed" || exists(dirty) {
		t.Errorf("--force: %+v", r.Worktrees)
	}
}

func TestSweepDryRunPreviewMatchesTheRealSweep(t *testing.T) {
	repo := newRepo(t)
	create(t, repo, "a", "")
	b := create(t, repo, "b", "")
	writeFile(t, filepath.Join(b, "x.txt"), "x")
	c := create(t, repo, "c", "")
	writeFile(t, filepath.Join(c, ".env"), "new ignored\n")
	before := git(t, repo, "worktree", "list")

	preview := sweep(t, repo, SweepOptions{DryRun: true})
	if git(t, repo, "worktree", "list") != before || preview.Pruned {
		t.Fatal("a dry run changed something")
	}
	real := sweep(t, repo, SweepOptions{})
	want := map[string]string{"a": "removed", "b": "dirty", "c": "dirty"}
	for task, w := range byTask(preview) {
		expect := want[task]
		if expect == "removed" {
			expect = "would-remove"
		}
		if w.Status != expect || byTask(real)[task].Status != want[task] {
			t.Errorf("%s: preview %s, real %s", task, w.Status, byTask(real)[task].Status)
		}
	}
	forced := sweep(t, repo, SweepOptions{DryRun: true, Force: true})
	for _, w := range forced.Worktrees {
		if w.Status != "would-remove" {
			t.Errorf("--dry-run --force: %+v", w)
		}
	}
}

func TestSweepSessionsAndLiveness(t *testing.T) {
	repo := newRepo(t)
	mine := create(t, repo, "mine", sessA)
	theirs := create(t, repo, "theirs", sessB)
	legacy := create(t, repo, "legacy", "")
	HeartbeatTouch(sessB)
	t.Setenv("CLAUDE_CODE_SESSION_ID", sessA)

	r := byTask(sweep(t, repo, SweepOptions{}))
	if r["mine"].Status != "skipped" || !r["mine"].Current || !exists(mine) {
		t.Errorf("current session: %+v", r["mine"])
	}
	if w := r["theirs"]; w.Status != "skipped" || !strings.Contains(w.Reason, "pass --include-live") ||
		w.SessionIdleMinutes == nil || !exists(theirs) {
		t.Errorf("live session: %+v", w)
	}
	if r["legacy"].Status != "removed" || exists(legacy) {
		t.Errorf("legacy: %+v", r["legacy"])
	}
	// A sessionless tree is never the current session's.
	if r["legacy"].Current {
		t.Error("legacy tree read as current")
	}

	// Past the window (backdate the heartbeat): swept.
	old := time.Now().Add(-3 * time.Hour)
	_ = os.Chtimes(filepath.Join(heartbeatDir(), "bbbbbbbb"), old, old)
	r = byTask(sweep(t, repo, SweepOptions{DryRun: true}))
	if r["theirs"].Status != "would-remove" {
		t.Errorf("stale session: %+v", r["theirs"])
	}
	r = byTask(sweep(t, repo, SweepOptions{DryRun: true, LiveWindow: 4 * time.Hour}))
	if r["theirs"].Status != "skipped" {
		t.Errorf("configurable window: %+v", r["theirs"])
	}
	r = byTask(sweep(t, repo, SweepOptions{IncludeCurrent: true, IncludeLive: true}))
	if r["mine"].Status != "removed" || r["theirs"].Status != "removed" {
		t.Errorf("include flags: %+v", r)
	}
}

func TestSweepZeroLiveWindowDisablesLiveness(t *testing.T) {
	repo := newRepo(t)
	create(t, repo, "theirs", sessB)
	HeartbeatTouch(sessB)
	r, err := Sweep(repo, SweepOptions{DryRun: true, LiveWindow: 0})
	if err != nil || r.Worktrees[0].Status != "would-remove" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestSweepPrunesHeartbeatsButNotOnADryRun(t *testing.T) {
	repo := newRepo(t)
	HeartbeatTouch(sessB)
	stale := filepath.Join(heartbeatDir(), "bbbbbbbb")
	old := time.Now().Add(-40 * 24 * time.Hour)
	_ = os.Chtimes(stale, old, old)
	sweep(t, repo, SweepOptions{DryRun: true})
	if !exists(stale) {
		t.Fatal("a dry run pruned a heartbeat")
	}
	sweep(t, repo, SweepOptions{})
	if exists(stale) {
		t.Fatal("a 40-day-old heartbeat survived")
	}
}

func TestSweepPrunesAHandDeletedWorktree(t *testing.T) {
	repo := newRepo(t)
	path := create(t, repo, "gone", "")
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	r := sweep(t, repo, SweepOptions{})
	if !r.Pruned || len(r.Worktrees) != 0 {
		t.Fatalf("%+v", r)
	}
	// Its branch is now an orphan: reported, deleted only on --branches.
	if len(r.Branches) != 1 || r.Branches[0].Status != "orphan" || r.Branches[0].Branch != "orchestra/gone" {
		t.Fatalf("branches %+v", r.Branches)
	}
	r = sweep(t, repo, SweepOptions{Branches: true, DryRun: true})
	if r.Branches[0].Status != "would-delete" || !strings.Contains(branches(t, repo), "orchestra/gone") {
		t.Fatalf("dry run %+v", r.Branches)
	}
	r = sweep(t, repo, SweepOptions{Branches: true})
	if r.Branches[0].Status != "deleted" || strings.Contains(branches(t, repo), "orchestra/gone") {
		t.Fatalf("delete %+v", r.Branches)
	}
}

func TestSweepOlderThan(t *testing.T) {
	repo := newRepo(t)
	path := create(t, repo, "fresh", "")
	days := 1.0
	r := sweep(t, repo, SweepOptions{OlderThanDays: &days})
	w := r.Worktrees[0]
	if w.Status != "skipped" || w.Reason != "newer than --older-than 1 day(s)" || w.AgeDays == nil || !exists(path) {
		t.Fatalf("%+v", w)
	}
	zero := 0.0
	if r := sweep(t, repo, SweepOptions{OlderThanDays: &zero}); r.Worktrees[0].Status != "removed" {
		t.Fatalf("--older-than 0: %+v", r.Worktrees)
	}
}

func TestSweepOutsideARepository(t *testing.T) {
	r, err := Sweep(t.TempDir(), SweepOptions{})
	if err != nil || r.Status != "error" || FormatSweepText(r) != "sweep error  not a git repository" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestHeartbeatTouch(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "alive")
	SetAliveDir(dir)
	t.Cleanup(func() { SetAliveDir("") })
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	HeartbeatTouch("")
	if exists(dir) {
		t.Fatal("touched without a session")
	}
	HeartbeatTouch("no!")
	if exists(dir) {
		t.Fatal("touched for an unusable session id")
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "ABCDEF12-3456")
	HeartbeatTouch("")
	path := filepath.Join(dir, "abcdef12")
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(path, old, old)
	HeartbeatTouch("")
	if st, err := os.Stat(path); err != nil || time.Since(st.ModTime()) > time.Minute {
		t.Fatalf("not refreshed: %v", err)
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", st.Mode().Perm())
	}
	// An unwritable location is silently skipped.
	SetAliveDir("/dev/null/alive")
	HeartbeatTouch("")
}

func TestAliveDirFollowsTheLedger(t *testing.T) {
	t.Setenv("ORCHESTRA_ALIVE_DIR", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := &config.Resolved{Values: map[string]any{"ledger": map[string]any{"dir": "~/elsewhere/runs"}}}
	if got := AliveDirFor(cfg); got != filepath.Join(home, "elsewhere", "alive") {
		t.Errorf("relocated ledger: %s", got)
	}
	cfg = &config.Resolved{Values: map[string]any{"telemetry": map[string]any{"dir": "/data/telemetry"}}}
	if got := AliveDirFor(cfg); got != "/data/alive" {
		t.Errorf("ledger beside telemetry: %s", got)
	}
	if got := AliveDirFor(&config.Resolved{Values: config.Defaults()}); got != filepath.Join(home, ".claude", "orchestra", "alive") {
		t.Errorf("defaults: %s", got)
	}
	t.Setenv("ORCHESTRA_ALIVE_DIR", "/override")
	if got := AliveDirFor(cfg); got != "/override" {
		t.Errorf("env override: %s", got)
	}
}
