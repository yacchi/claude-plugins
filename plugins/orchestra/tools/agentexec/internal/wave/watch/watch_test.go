package watch

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/plan"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/state"
)

// liveRunner starts a process standing in for the runner of statePath and
// records it in runner.pid.
func liveRunner(t *testing.T, statePath string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "sleep 30", "wave", "run", "--state="+statePath)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	writeRecord(t, statePath, Record{PID: cmd.Process.Pid, StartedAt: time.Now().Unix(), Argv: cmd.Args})
	return cmd
}

func writeRecord(t *testing.T, statePath string, r Record) {
	t.Helper()
	data, err := fsutil.MarshalJSON(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(PIDPath(statePath), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func newStore(t *testing.T, ids ...string) *state.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	store, _ := state.Open(path)
	if _, err := store.Init("", ids, "int"); err != nil {
		t.Fatal(err)
	}
	return store
}

func lines(t *testing.T, path string) int {
	t.Helper()
	data, _ := os.ReadFile(path)
	n := 0
	for _, b := range data {
		if b == '\n' {
			n++
		}
	}
	return n
}

func TestDetachRefusesASecondRunnerWhileOneLives(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	args := []string{"-c", "sleep 30", "wave", "run", "--state=" + statePath}
	first, err := Detach(statePath, "/bin/sh", args)
	if err != nil || first["status"] != "detached" {
		t.Fatalf("first detach: %v %v", first, err)
	}
	pid := first["pid"].(int)
	t.Cleanup(func() { _ = syscallKill(pid) })
	second, err := Detach(statePath, "/bin/sh", args)
	if err != nil || second["status"] != "running" || second["pid"] != pid {
		t.Fatalf("second detach must find the live runner %d: %v %v", pid, second, err)
	}
	if rec, _ := ReadRecord(statePath); rec.PID != pid {
		t.Fatalf("runner.pid rewritten: %+v", rec)
	}

	// A runner.pid whose pid is alive but runs another state is no runner.
	other := filepath.Join(t.TempDir(), "state.json")
	liveRunner(t, other)
	writeRecord(t, statePath, mustRecord(t, other))
	third, err := Detach(statePath, "/bin/sh", args)
	if err != nil || third["status"] != "detached" {
		t.Fatalf("a live process of another state must not block: %v %v", third, err)
	}
	_ = syscallKill(third["pid"].(int))
}

func mustRecord(t *testing.T, statePath string) Record {
	t.Helper()
	r, ok := ReadRecord(statePath)
	if !ok {
		t.Fatal("no runner.pid")
	}
	return r
}

func syscallKill(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

func TestWaitReportsOnlyNeedsAddedAfterSince(t *testing.T) {
	store := newStore(t, "a", "b")
	liveRunner(t, store.Path)
	if err := store.AddNeed("a", "escalate", "old"); err != nil {
		t.Fatal(err)
	}
	seq := lines(t, store.EventsPath())
	wait := func(since int) Answer {
		return Wait(WaitOptions{State: store.Path, Since: since, MaxWait: 200 * time.Millisecond, Poll: 20 * time.Millisecond})
	}
	if a := wait(seq); a.Status != "running" || a.Seq != seq {
		t.Fatalf("a need present at --since is not new: %s seq=%d (want running seq=%d)", a.Status, a.Seq, seq)
	}
	if a := wait(0); a.Status != "needs" {
		t.Fatalf("since 0: every need is new, got %s", a.Status)
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = store.AddNeed("", "environment", "wave level")
	}()
	a := Wait(WaitOptions{State: store.Path, Since: seq, MaxWait: 5 * time.Second, Poll: 10 * time.Millisecond})
	if a.Status != "needs" || a.Seq != lines(t, store.EventsPath()) || len(a.Progress.Needs) != 2 {
		t.Fatalf("a need added after --since: %+v", a)
	}
}

func TestWaitIsRunningWhileTheRunnerLivesAndEndsWithIt(t *testing.T) {
	store := newStore(t, "a")
	runner := liveRunner(t, store.Path)
	started := time.Now()
	a := Wait(WaitOptions{State: store.Path, MaxWait: 300 * time.Millisecond, Poll: 50 * time.Millisecond})
	if a.Status != "running" {
		t.Fatalf("runner alive: got %s", a.Status)
	}
	if took := time.Since(started); took > 3*time.Second {
		t.Fatalf("wait overran --max-wait: %s", took)
	}

	if err := store.MarkStopped("by test"); err != nil {
		t.Fatal(err)
	}
	// The state says stopped while the process still runs: still running.
	if a := Wait(WaitOptions{State: store.Path, MaxWait: 100 * time.Millisecond, Poll: 20 * time.Millisecond}); a.Status != "running" {
		t.Fatalf("runner alive after a stop record: got %s", a.Status)
	}
	if err := os.WriteFile(LogPath(store.Path), []byte("max-in-flight: auto\n{\"status\":\"stopped\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = runner.Process.Kill()
		_ = runner.Wait()
	}()
	if a := Wait(WaitOptions{State: store.Path, MaxWait: 5 * time.Second, Poll: 10 * time.Millisecond}); a.Status != "stopped" {
		t.Fatalf("runner gone with a stopped report: got %s", a.Status)
	}
	if err := os.WriteFile(LogPath(store.Path), []byte("panic: boom\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if a := Wait(WaitOptions{State: store.Path, MaxWait: time.Second}); a.Status != "crashed" {
		t.Fatalf("runner gone without a report: got %s", a.Status)
	}
}

// etaFixture: now = 1000; events give each package's start.
func etaFixture(pkgs []plan.Package, statuses map[string]*state.Package, started map[string]float64) inputs {
	st := &state.State{Packages: statuses}
	var events []map[string]any
	for id, at := range started {
		events = append(events, map[string]any{"event": "status", "pkg": id, "to": "implementing", "at": at})
	}
	return inputs{plan: &plan.Plan{Packages: pkgs}, state: st, events: events, now: 1000}
}

func pkg(id string, est float64, deps ...string) plan.Package {
	return plan.Package{ID: id, Cls: "light", EstSeconds: est, DependsOn: deps, FilesOwned: []string{id + "/**"}}
}

func status(s string, since float64) *state.Package { return &state.Package{Status: s, Since: since} }

func eta(t *testing.T, p Progress) int {
	t.Helper()
	if p.EtaS == nil || p.EtaBasis != "critical-path" {
		t.Fatalf("eta missing: %+v", p)
	}
	return *p.EtaS
}

func TestETACountsWhatIsLeftOfARunningPackage(t *testing.T) {
	// a has run 400 s of its 600: 200 left, then b (100).
	in := etaFixture(
		[]plan.Package{pkg("a", 600), pkg("b", 100, "a")},
		map[string]*state.Package{"a": status("implementing", 600), "b": status("pending", 0)},
		map[string]float64{"a": 600},
	)
	if got := eta(t, build(in)); got != 300 {
		t.Fatalf("eta = %d, want 300", got)
	}
	// Overrun: never below zero.
	in.now = 5000
	if got := eta(t, build(in)); got != 100 {
		t.Fatalf("overrun eta = %d, want 100", got)
	}
}

func TestETARatioNeedsTwoEstimatedFinishedPackages(t *testing.T) {
	pkgs := []plan.Package{pkg("x", 100), pkg("y", 100), pkg("c", 100)}
	one := etaFixture(pkgs,
		map[string]*state.Package{"x": status("integrated", 400), "y": status("pending", 0), "c": status("pending", 0)},
		map[string]float64{"x": 100}, // took 300 for 100
	)
	if got := eta(t, build(one)); got != 100 {
		t.Fatalf("one sample must leave the ratio at 1: eta = %d, want 100", got)
	}
	two := etaFixture(pkgs,
		map[string]*state.Package{"x": status("integrated", 400), "y": status("integrated", 600), "c": status("pending", 0)},
		map[string]float64{"x": 100, "y": 500}, // 300 + 100 for 200 estimated: ratio 2
	)
	if got := eta(t, build(two)); got != 200 {
		t.Fatalf("two samples: eta = %d, want 200", got)
	}
}

func TestETAIsNullWithoutAnEstimateLeft(t *testing.T) {
	in := etaFixture(
		[]plan.Package{pkg("x", 100), pkg("c", 0, "x")},
		map[string]*state.Package{"x": status("integrated", 400), "c": status("pending", 0)},
		map[string]float64{"x": 100},
	)
	p := build(in)
	if p.EtaS != nil || p.EtaBasis != "none" {
		t.Fatalf("no unfinished estimate: eta %v basis %s", p.EtaS, p.EtaBasis)
	}
	if len(p.Next) != 1 || p.Next[0] != "c" {
		t.Fatalf("next = %v", p.Next)
	}
}

// progressFor is the progress of a one-package wave whose package is
// implementing.
func progressFor(t *testing.T, store *state.Store) Progress {
	t.Helper()
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	st.Packages["a"] = &state.Package{Status: "implementing", Since: 500}
	return Compute(store.Path, &st, 1000, nil)
}

func TestProgressMovesInFlightToInterruptedWhenTheRunnerIsGone(t *testing.T) {
	store := newStore(t, "a")
	dead := exec.Command("/bin/sh", "-c", "exit 0", "wave", "run", "--state="+store.Path)
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	writeRecord(t, store.Path, Record{PID: dead.Process.Pid, StartedAt: time.Now().Unix(), Argv: dead.Args})

	p := progressFor(t, store)
	if len(p.Running) != 0 || len(p.Interrupted) != 1 {
		t.Fatalf("running=%v interrupted=%v", p.Running, p.Interrupted)
	}
	if i := p.Interrupted[0]; i.ID != "a" || i.Phase != "implementing" || i.Since != 500 {
		t.Fatalf("interrupted: %+v", i)
	}
	if l := Line(p); !strings.Contains(l, "0 running · 1 interrupted · 0 needs") {
		t.Fatalf("line: %s", l)
	}
}

func TestProgressKeepsInFlightRunningWhileTheRunnerLivesOrIsUnknown(t *testing.T) {
	alive := newStore(t, "a")
	liveRunner(t, alive.Path)
	unknown := newStore(t, "a")
	for name, store := range map[string]*state.Store{"alive": alive, "no record": unknown} {
		p := progressFor(t, store)
		if len(p.Running) != 1 || len(p.Interrupted) != 0 {
			t.Fatalf("%s: running=%v interrupted=%v", name, p.Running, p.Interrupted)
		}
		if strings.Contains(Line(p), "interrupted") {
			t.Fatalf("%s: line %s", name, Line(p))
		}
	}
}
