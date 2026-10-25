package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// markDuringFirstFinalChecks returns a --full whose first run blocks until
// mark (called from the test) has handed a package back, then fails; every
// later run passes.
func markDuringFirstFinalChecks(t *testing.T, w *waveRepo, mark func()) string {
	started, release := filepath.Join(w.tmp, "started"), filepath.Join(w.tmp, "release")
	done := filepath.Join(w.tmp, "done")
	go func() {
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(started); err == nil {
				mark()
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		_ = os.WriteFile(release, nil, 0o644)
	}()
	return fmt.Sprintf(`if [ ! -f '%s' ]; then touch '%s'; i=0; `+
		`while [ ! -f '%s' ] && [ $i -lt 600 ]; do sleep 0.05; i=$((i+1)); done; `+
		`touch '%s'; echo boom; exit 1; fi`, done, started, release, done)
}

func TestPackageHandedBackDuringFinalChecksIsPickedUp(t *testing.T) {
	w := newWaveRepo(t)
	w.verifyMode = "final"
	w.plan(pkgDef{id: "A"}, pkgDef{id: "B"})
	ex := newFake(t, map[string][]round{
		"A": {edit(map[string]string{"a.txt": "a\n"})},
		"B": {edit(map[string]string{"b.txt": "b\n"}, editOpt{answer: "ESCALATE: need a decision"})},
	})
	marked := make(chan map[string]any, 1)
	full := markDuringFirstFinalChecks(t, w, func() {
		if st := w.state(); st.Packages["B"].Status != "needs" {
			t.Errorf("B is %q while the final checks run", st.Packages["B"].Status)
		}
		res, _ := Mark(MarkOptions{State: w.statePath, Pkg: "B", Status: "ready"})
		marked <- res
	})
	rep, code := w.run(ex, func(o *Options) { o.Full, o.ConvergeRounds = full, ptr(0) })
	select {
	case res := <-marked:
		eqJSON(t, "mark", res, map[string]any{"pkg": "B", "status": "verifying"})
	default:
		t.Fatal("B was not marked during the final checks")
	}
	eq(t, "exit", code, 0)
	if rep.Reason != nil {
		t.Errorf("stopped: %s", *rep.Reason)
	}
	eq(t, "B", w.status("B"), "integrated")
	eq(t, "needs", len(w.needKinds()), 0)
	eq(t, "full runs", len(w.events("full-start")), 2)
	eq(t, "fixups", len(w.events("fixup")), 0)
}

// runBounded fails the test when the run does not end (a converge loop
// that keeps rescheduling).
func runBounded(t *testing.T, w *waveRepo, ex Executor, mod func(*Options)) (Report, int) {
	t.Helper()
	done := make(chan struct{})
	var rep Report
	var code int
	go func() {
		defer close(done)
		rep, code = w.run(ex, mod)
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("the run did not end")
	}
	return rep, code
}

func TestHandedBackPackageBlockedBehindNeedFallsThrough(t *testing.T) {
	w := newWaveRepo(t)
	w.verifyMode = "final"
	w.plan(pkgDef{id: "A"}, pkgDef{id: "B", dependsOn: []string{"A"}})
	ex := newFake(t, map[string][]round{
		"A": {edit(map[string]string{"a.txt": "a\n"}, editOpt{answer: "ESCALATE: need a decision"})},
	})
	// B handed back as pending cannot start while A stays in needs.
	full := markDuringFirstFinalChecks(t, w, func() {
		_, _ = Mark(MarkOptions{State: w.statePath, Pkg: "B", Status: "pending"})
	})
	rep, code := runBounded(t, w, ex, func(o *Options) { o.Full, o.ConvergeRounds = full, ptr(0) })
	eq(t, "exit", code, 5)
	if rep.Reason == nil || !strings.Contains(*rep.Reason, "full checks red after 0") {
		t.Errorf("reason %v", deref(rep.Reason))
	}
	eq(t, "B", w.status("B"), "pending")
	eq(t, "full runs", len(w.events("full-start")), 1)
}

func TestStopDuringHandedBackPickupEndsTheRun(t *testing.T) {
	w := newWaveRepo(t)
	w.verifyMode = "final"
	w.plan(pkgDef{id: "A"})
	ex := newFake(t, map[string][]round{
		"A": {edit(map[string]string{"a.txt": "a\n"}, editOpt{answer: "ESCALATE: need a decision"})},
	})
	// --max-packages 1 is spent on A's first start: picked up again, A stops the run.
	full := markDuringFirstFinalChecks(t, w, func() {
		_, _ = Mark(MarkOptions{State: w.statePath, Pkg: "A", Status: "pending"})
	})
	rep, code := runBounded(t, w, ex, func(o *Options) { o.Full, o.ConvergeRounds, o.MaxPackages = full, ptr(0), ptr(1) })
	eq(t, "exit", code, 5)
	if rep.Reason == nil || !strings.Contains(*rep.Reason, "max packages") {
		t.Errorf("reason %v", deref(rep.Reason))
	}
	eq(t, "needs", len(w.needKinds()), 0)
	eq(t, "A", w.status("A"), "pending")
	eq(t, "full runs", len(w.events("full-start")), 1)
}
