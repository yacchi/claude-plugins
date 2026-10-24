package runner

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// `max_in_flight: auto` starts every startable package at once: six
// independent packages are all in flight together.
func TestAutoWidthStartsEveryStartablePackage(t *testing.T) {
	w := newWaveRepo(t)
	w.verifyMode = "final"
	ids := []string{"A", "B", "C", "D", "E", "F"}
	rounds := map[string][]round{}
	var defs []pkgDef
	for _, id := range ids {
		defs = append(defs, pkgDef{id: id})
		rounds[id] = []round{edit(map[string]string{strings.ToLower(id) + ".txt": id + "\n"})}
	}
	w.plan(defs...)
	var mu sync.Mutex
	active, peak := 0, 0
	ex := newFake(t, rounds)
	ex.onStart = func(string, spec) {
		mu.Lock()
		active++
		peak = max(peak, active)
		mu.Unlock()
	}
	ex.onReturn = func(string) {
		// Stay in flight until all six started (a capped width never gets
		// there and gives up after the deadline).
		deadline := time.Now().Add(5 * time.Second)
		for {
			mu.Lock()
			done := peak == len(ids) || time.Now().After(deadline)
			mu.Unlock()
			if done {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		mu.Lock()
		active--
		mu.Unlock()
	}
	rep, code := w.run(ex)
	eq(t, "exit", code, 0)
	eq(t, "integrated", len(rep.Integrated), len(ids))
	eq(t, "peak in flight", peak, len(ids))
}

// Memory pressure holds only new starts: the package in flight still
// integrates, the dependent waits, and starts once the pressure eases.
func TestMemoryGuardHoldsOnlyNewStarts(t *testing.T) {
	w := newWaveRepo(t)
	w.verifyMode = "final"
	w.plan(pkgDef{id: "A"}, pkgDef{id: "B", dependsOn: []string{"A"}})
	var pressure atomic.Bool
	defer func(prev func() (bool, string)) { memoryPressure = prev }(memoryPressure)
	memoryPressure = func() (bool, string) { return pressure.Load(), "test pressure" }
	ex := newFake(t, map[string][]round{
		"A": {func(t *testing.T, s spec, x []string) map[string]any {
			pressure.Store(true)
			return edit(map[string]string{"a.txt": "a\n"})(t, s, x)
		}},
		"B": {edit(map[string]string{"b.txt": "b\n"})},
	})
	var sleeps []float64
	var aWhileHeld string
	w.sleep = func(s float64) {
		sleeps = append(sleeps, s)
		aWhileHeld = w.status("A")
		pressure.Store(false)
	}
	rep, code := w.run(ex)
	eq(t, "exit", code, 0)
	eqJSON(t, "integrated", rep.Integrated, []string{"A", "B"})
	eq(t, "A integrated while held", aWhileHeld, "integrated")
	eqJSON(t, "re-checks", sleeps, []float64{memoryRecheck.Seconds()})
	eq(t, "held events", len(w.details("start-held", "")), 1)
	eq(t, "resumed events", len(w.details("start-resumed", "")), 1)
	eq(t, "held reason", w.details("start-held", "")[0]["reason"], any("test pressure"))
}

// The budget stops new starts only: the package in flight when the cost
// crosses it still integrates.
func TestBudgetStopsNewStartsOnly(t *testing.T) {
	w := newWaveRepo(t)
	w.verifyMode = "final"
	w.plan(pkgDef{id: "A"}, pkgDef{id: "B", dependsOn: []string{"A"}})
	var offset, usd atomic.Int64
	w.clock = func() float64 { return float64(time.Now().UnixNano())/1e9 + float64(offset.Load()) }
	ex := newFake(t, map[string][]round{
		"A": {func(t *testing.T, s spec, x []string) map[string]any {
			usd.Store(5)
			offset.Store(costRefreshSeconds + 1) // past the cached estimate
			return edit(map[string]string{"a.txt": "a\n"})(t, s, x)
		}},
	})
	d := Deps{Executor: ex, AgentExec: []string{agentExec(t)}, Stderr: w, Clock: w.clock,
		Cost: func(runID string) (float64, bool) { return float64(usd.Load()), runID == "run-1" }}
	o := w.opts(func(o *Options) { o.RunID, o.BudgetUSD = "run-1", ptr(1.0) })
	rep, code := Run(t.Context(), o, d)
	eq(t, "exit", code, 5)
	if rep.Reason == nil || !strings.HasPrefix(*rep.Reason, "budget reached") {
		t.Fatalf("reason = %v", rep.Reason)
	}
	eqJSON(t, "integrated", rep.Integrated, []string{"A"})
	eqJSON(t, "pending", rep.Pending, []string{"B"})
}

// A usage notice goes out every usage_every integrated packages, and a
// notify command that hangs does not hold the next start.
func TestUsageNoticeDoesNotWaitForTheNotifyCommand(t *testing.T) {
	w := newWaveRepo(t)
	w.verifyMode = "final"
	w.checks("wave:\n  usage_every: 1\n")
	w.plan(pkgDef{id: "A"}, pkgDef{id: "B", dependsOn: []string{"A"}})
	out := filepath.Join(w.tmp, "notify")
	done := filepath.Join(w.tmp, "notify-done")
	notifyCmd := `[ "$WAVE_EVENT" = usage ] || exit 0; cat > ` + out + `; sleep 3; touch ` + done
	var doneBeforeB bool
	ex := newFake(t, map[string][]round{
		"A": {edit(map[string]string{"a.txt": "a\n"})},
		"B": {edit(map[string]string{"b.txt": "b\n"})},
	})
	ex.onStart = func(pid string, _ spec) {
		if pid == "B" {
			_, err := os.Stat(done)
			doneBeforeB = err == nil
		}
	}
	rep, code := w.run(ex, func(o *Options) { o.NotifyCmd = notifyCmd })
	eq(t, "exit", code, 0)
	eqJSON(t, "integrated", rep.Integrated, []string{"A", "B"})
	eq(t, "B waited for the notify command", doneBeforeB, false)
	usage := w.details("usage", "")
	eq(t, "usage notices", len(usage), 2)
	eqJSON(t, "first notice", usage[0], map[string]any{"integrated": 1, "in_flight": 0, "pending": 1, "estimated_usd": nil})
	if text, ok := readFile(out); !ok || !strings.Contains(text, `"event":"usage"`) {
		t.Errorf("notify payload = %q", text)
	}
}
