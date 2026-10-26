package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/quota"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/state"
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

// injectClaudeUsage makes the Claude 5-hour window read as *pct percent (a
// fixture reading, so its age is not checked) and turns memory pressure off;
// both are restored when the test ends.
func injectClaudeUsage(t *testing.T, pct *atomic.Int64) {
	t.Helper()
	prevUsage, prevMemory := claudeUsage, memoryPressure
	t.Cleanup(func() { claudeUsage, memoryPressure = prevUsage, prevMemory })
	claudeUsage = func() (quota.Entry, bool) {
		return quota.Entry{Source: quota.SourceFixture, FiveHour: &quota.Window{UsedPercent: float64(pct.Load())}}, true
	}
	memoryPressure = func() (bool, string) { return false, "" }
}

// usageHoldRun runs A, then B (which depends on A). A's round pushes the
// window to 96%; the first re-check brings it back to 50%.
func usageHoldRun(t *testing.T, w *waveRepo) (rep Report, code int, sleeps []float64, aWhileHeld string) {
	t.Helper()
	w.verifyMode = "final"
	w.plan(pkgDef{id: "A"}, pkgDef{id: "B", dependsOn: []string{"A"}})
	var pct atomic.Int64
	pct.Store(50)
	injectClaudeUsage(t, &pct)
	ex := newFake(t, map[string][]round{
		"A": {func(t *testing.T, s spec, x []string) map[string]any {
			pct.Store(96)
			return edit(map[string]string{"a.txt": "a\n"})(t, s, x)
		}},
		"B": {edit(map[string]string{"b.txt": "b\n"})},
	})
	w.sleep = func(s float64) {
		sleeps = append(sleeps, s)
		aWhileHeld = w.status("A")
		pct.Store(50)
	}
	rep, code = w.run(ex)
	return rep, code, sleeps, aWhileHeld
}

// Claude plan usage at the stop level holds only new starts: the package in
// flight integrates, the dependent waits, and starts once the reading drops.
func TestUsageHoldOnlyNewStarts(t *testing.T) {
	w := newWaveRepo(t)
	rep, code, sleeps, aWhileHeld := usageHoldRun(t, w)
	eq(t, "exit", code, 0)
	eqJSON(t, "integrated", rep.Integrated, []string{"A", "B"})
	eq(t, "A integrated while held", aWhileHeld, "integrated")
	eqJSON(t, "re-checks", sleeps, []float64{memoryRecheck.Seconds()})
	held := w.details("start-held", "")
	eq(t, "held events", len(held), 1)
	eq(t, "held reason", held[0]["reason"], any("Claude plan usage is 96% of the 5-hour window (threshold 95%, resets unknown)"))
	eq(t, "resumed events", len(w.details("start-resumed", "")), 1)
}

// enforcement hold applies whether or not memory_guard is on.
func TestMemoryGuardOffStillHoldsOnUsage(t *testing.T) {
	w := newWaveRepo(t)
	w.checks("wave:\n  memory_guard: false\n")
	rep, code, _, _ := usageHoldRun(t, w)
	eq(t, "exit", code, 0)
	eqJSON(t, "integrated", rep.Integrated, []string{"A", "B"})
	eq(t, "held events", len(w.details("start-held", "")), 1)
}

// Memory pressure and the usage hold together emit one start-held, with the
// memory reason.
func TestMemoryAndUsageHoldEmitOneStartHeld(t *testing.T) {
	w := newWaveRepo(t)
	w.verifyMode = "final"
	w.plan(pkgDef{id: "A"}, pkgDef{id: "B", dependsOn: []string{"A"}})
	var pct atomic.Int64
	pct.Store(50)
	injectClaudeUsage(t, &pct)
	var pressure atomic.Bool
	memoryPressure = func() (bool, string) { return pressure.Load(), "test pressure" }
	ex := newFake(t, map[string][]round{
		"A": {func(t *testing.T, s spec, x []string) map[string]any {
			pressure.Store(true)
			pct.Store(96)
			return edit(map[string]string{"a.txt": "a\n"})(t, s, x)
		}},
		"B": {edit(map[string]string{"b.txt": "b\n"})},
	})
	w.sleep = func(float64) {
		pressure.Store(false)
		pct.Store(50)
	}
	rep, code := w.run(ex)
	eq(t, "exit", code, 0)
	eqJSON(t, "integrated", rep.Integrated, []string{"A", "B"})
	held := w.details("start-held", "")
	eq(t, "held events", len(held), 1)
	eq(t, "held reason", held[0]["reason"], any("test pressure"))
	eq(t, "resumed events", len(w.details("start-resumed", "")), 1)
}

// usageDeps is a runner's Deps whose config carries the given enforcement
// values (nil = no config keys).
func usageDeps(enforcement map[string]any) Deps {
	return Deps{Config: func() (*config.Resolved, error) {
		if enforcement == nil {
			return &config.Resolved{}, nil
		}
		return &config.Resolved{Values: map[string]any{"enforcement": enforcement}}, nil
	}}
}

// The hold boundaries: a stop level of max(usage_guard, 95), stale, reset
// and missing readings fail open, and the off switches win.
func TestUsageHeldBoundaries(t *testing.T) {
	const now = 1_000_000.0
	fresh := now - 10
	cases := []struct {
		name     string
		pct      float64
		resets   *float64
		fetched  float64
		source   string
		values   map[string]any
		env      string
		cfgErr   bool
		noWindow bool
		missing  bool
		held     bool
	}{
		{name: "89.9 not held", pct: 89.9, fetched: fresh},
		{name: "90 not held", pct: 90, fetched: fresh},
		{name: "95 held", pct: 95, fetched: fresh, held: true},
		{name: "100 held", pct: 100, fetched: fresh, held: true},
		{name: "stale cache not held", pct: 99, fetched: now - 901},
		{name: "unfetched cache not held", pct: 99},
		{name: "fixture ignores age", pct: 99, source: quota.SourceFixture, held: true},
		{name: "reset in the past not held", pct: 99, fetched: fresh, resets: ptr(now - 1)},
		{name: "reset at now not held", pct: 99, fetched: fresh, resets: ptr(now)},
		{name: "resets unknown held", pct: 96, fetched: fresh, held: true},
		{name: "no five-hour window not held", pct: 99, fetched: fresh, noWindow: true},
		{name: "no reading not held", missing: true},
		{name: "usage_guard 75 keeps stop at 95", pct: 80, fetched: fresh, values: map[string]any{"usage_guard": int64(75)}},
		{name: "usage_guard 98 raises stop", pct: 96, fetched: fresh, values: map[string]any{"usage_guard": int64(98)}},
		{name: "usage_guard off", pct: 99, fetched: fresh, values: map[string]any{"usage_guard": "off"}},
		{name: "usage_guard false", pct: 99, fetched: fresh, values: map[string]any{"usage_guard": false}},
		{name: "env off", pct: 99, fetched: fresh, env: "off"},
		{name: "usage_guard 0 falls back to 90", pct: 94, fetched: fresh, values: map[string]any{"usage_guard": int64(0)}},
		{name: "usage_guard text falls back to 90", pct: 96, fetched: fresh, values: map[string]any{"usage_guard": "x"}, held: true},
		{name: "usage_guard 150 falls back to 90", pct: 96, fetched: fresh, values: map[string]any{"usage_guard": int64(150)}, held: true},
		{name: "config error fails open at 95", pct: 94, fetched: fresh, cfgErr: true},
		{name: "config error still holds at 95", pct: 96, fetched: fresh, cfgErr: true, held: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ORCHESTRA_USAGE_GUARD", tc.env)
			prev := claudeUsage
			defer func() { claudeUsage = prev }()
			claudeUsage = func() (quota.Entry, bool) {
				if tc.missing {
					return quota.Entry{}, false
				}
				source := tc.source
				if source == "" {
					source = quota.SourceCache
				}
				e := quota.Entry{Source: source, FetchedAt: tc.fetched}
				if !tc.noWindow {
					e.FiveHour = &quota.Window{UsedPercent: tc.pct, ResetsAt: tc.resets}
				}
				return e, true
			}
			d := usageDeps(tc.values)
			if tc.cfgErr {
				d.Config = func() (*config.Resolved, error) { return nil, errors.New("bad config") }
			}
			r := &runner{deps: d, clock: func() float64 { return now }}
			held, reason := r.usageHeld()
			eq(t, "held", held, tc.held)
			if held && !strings.HasPrefix(reason, "Claude plan usage is ") {
				t.Errorf("reason = %q", reason)
			}
		})
	}
}

// The default reader takes the snapshot fixture named by the environment and
// fails open on a garbled file.
func TestClaudeUsageReadsSnapshotFixture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.json")
	t.Setenv(quota.SnapshotEnv, path)
	r := &runner{deps: usageDeps(nil), clock: func() float64 { return 1_000_000 }}
	writeFile(t, path, `{"claude":{"five_hour":{"used_percent":96,"resets_at":1003600}}}`)
	held, _ := r.usageHeld()
	eq(t, "fixture held", held, true)
	writeFile(t, path, "{not json")
	held, _ = r.usageHeld()
	eq(t, "garbled not held", held, false)
}

// Between usage_advise and the stop level, paceStarts emits one usage-advice
// per 5-point step and lets one new start through per round; it re-arms when
// the reading falls back below usage_advise.
func TestPaceStartsAdvisesPerStepAndPacesDown(t *testing.T) {
	var pct atomic.Int64
	injectClaudeUsage(t, &pct)
	const now = 1_000_000.0
	statePath := filepath.Join(t.TempDir(), "state.json")
	r := &runner{deps: usageDeps(nil), clock: func() float64 { return now },
		store: usageStore(t, statePath)}
	three := []string{"A", "B", "C"}
	advised := func() int {
		n := 0
		for _, ev := range state.ReadEvents(statePath, -1) {
			if ev["event"] == "usage-advice" {
				n++
			}
		}
		return n
	}
	step := func(p int64, want []string, events int) {
		t.Helper()
		pct.Store(p)
		eqJSON(t, fmt.Sprintf("starts at %d%%", p), r.paceStarts(three), want)
		eq(t, fmt.Sprintf("advice events at %d%%", p), advised(), events)
	}
	step(70, three, 0)
	step(76, []string{"A"}, 1)
	step(77, []string{"A"}, 1)
	step(81, []string{"A"}, 2)
	step(60, three, 2)
	step(76, []string{"A"}, 3)
	step(96, three, 3)

	off := &runner{deps: usageDeps(map[string]any{"usage_advise": "off"}), clock: func() float64 { return now },
		store: usageStore(t, filepath.Join(t.TempDir(), "state.json"))}
	pct.Store(80)
	eqJSON(t, "advice off starts all", off.paceStarts(three), three)
}

// usageStore is an initialised state store at path, so events land in it.
func usageStore(t *testing.T, path string) *state.Store {
	t.Helper()
	st := &state.Store{Path: path, Clock: func() float64 { return 1_000_000 }}
	if _, err := st.Init(filepath.Join(filepath.Dir(path), "plan.json"), nil, "wave-int"); err != nil {
		t.Fatal(err)
	}
	return st
}
