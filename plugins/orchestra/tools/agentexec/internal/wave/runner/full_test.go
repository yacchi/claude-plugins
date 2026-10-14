package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// poisonFull is red when any tracked .txt holds POISON and names a failing
// test file like vitest.
const poisonFull = "if grep -rqs POISON --include=*.txt .; then " +
	"echo ' FAIL  tests/poison.test.ts > poisoned'; exit 1; fi"

func (w *waveRepo) assertIntClean() {
	w.t.Helper()
	path := w.intPath()
	if _, err := gitRun(path, "symbolic-ref", "-q", "HEAD"); err != nil {
		w.t.Error("integration tree is detached")
	}
	if out := w.git(path, "status", "--porcelain"); out != "" {
		w.t.Errorf("integration tree dirty: %q", out)
	}
	gitdir := w.git(path, "rev-parse", "--absolute-git-dir")
	for _, name := range []string{"REVERT_HEAD", "sequencer"} {
		if _, err := os.Stat(filepath.Join(gitdir, name)); err == nil {
			w.t.Errorf("%s left behind", name)
		}
	}
}

func TestCulpritRevertedAndRedispatchedFreshWithCorrection(t *testing.T) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A"}, pkgDef{id: "B"})
	correction := filepath.Join(w.waveDir, "corrections", "B.md")
	seen := map[string]any{}
	first := true
	ex := newFake(t, map[string][]round{
		"A": {edit(map[string]string{"a.txt": "a\n"})},
		"B": {edit(map[string]string{"b.txt": "POISON\n"}), edit(map[string]string{"b.txt": "fixed\n"})},
	})
	ex.onStart = func(pid string, s spec) {
		if pid != "B" {
			return
		}
		if first {
			first = false
			return
		}
		seen["files"] = s.PromptFiles
		seen["correction"], _ = readFile(correction)
		_, seen["b_in_int"] = w.intFile("b.txt")
	}
	ex.onReturn = finishAfter(t, w, "A", "B")
	rep, code := w.run(ex, func(o *Options) { o.Gate, o.Full = "true", poisonFull })
	eq(t, "exit", code, 0)
	eqJSON(t, "integrated", rep.Integrated, []string{"A", "B"})
	b := ex.callsOf("B")
	if len(b) != 2 || !b[1].NoResume {
		t.Fatalf("B calls %+v", b)
	}
	eqJSON(t, "files", seen["files"], []string{filepath.Join(w.waveDir, "preamble.md"),
		filepath.Join(w.waveDir, "context", "B.md"), filepath.Join(w.waveDir, "specs", "B.md"), correction})
	corr := seen["correction"].(string)
	for _, want := range []string{"broke the full verification after integration", "tests/poison.test.ts", "files_owned"} {
		if !strings.Contains(corr, want) {
			t.Errorf("correction lacks %q", want)
		}
	}
	eq(t, "b reverted from int", seen["b_in_int"], any(false))
	if _, err := os.Stat(correction); err == nil {
		t.Error("the sent correction was not set aside")
	}
	var revertPkgs []string
	for _, e := range w.events("revert") {
		revertPkgs = append(revertPkgs, e.Pkg)
	}
	eqJSON(t, "reverts", revertPkgs, []string{"B"})
	var pendingDetails []string
	for _, e := range w.events("status") {
		if e.Pkg == "B" && e.To == "pending" {
			pendingDetails = append(pendingDetails, e.Detail)
		}
	}
	eqJSON(t, "pending details", pendingDetails, []string{"post-full revert"})
	if got, _ := w.intFile("b.txt"); got != "fixed\n" {
		t.Errorf("b.txt %q", got)
	}
	eq(t, "green = head", w.lastGreenSHA(), w.git(w.intPath(), "rev-parse", "HEAD"))
	w.assertIntClean()
}

func TestFlakyFullRescuedByFullRetry(t *testing.T) {
	w := newWaveRepo(t)
	counter := filepath.Join(w.tmp, "runs")
	marker := filepath.Join(w.tmp, "on-green")
	full := fmt.Sprintf(`n=$(cat '%s' 2>/dev/null || echo 0); echo $((n+1)) > '%s'; `+
		`if [ "$n" = 0 ]; then echo ' FAIL  tests/flaky.test.ts'; exit 1; fi`, counter, counter)
	w.plan(pkgDef{id: "A"})
	ex := newFake(t, map[string][]round{"A": {edit(map[string]string{"a.txt": "a\n"})}})
	rep, code := w.run(ex, func(o *Options) {
		o.Gate, o.Full, o.FullRetry, o.OnGreen = "true", full, "test -n {failed}", "touch '"+marker+"'"
	})
	eq(t, "exit", code, 0)
	eqJSON(t, "integrated", rep.Integrated, []string{"A"})
	eq(t, "reverts", len(w.events("revert")), 0)
	eq(t, "probes", len(w.events("bisect-probe")), 0)
	flaky := w.events("flaky")
	if len(flaky) != 1 || flaky[0].Pkg != "" || flaky[0].Detail != `{"check":"full","files":["tests/flaky.test.ts"]}` {
		t.Errorf("flaky %+v", flaky)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("on-green did not run")
	}
	w.assertIntClean()
}

func TestEnsureFailureAndRedAtLastGreenIsEnvironmentNeed(t *testing.T) {
	w := newWaveRepo(t)
	w.checks("checks:\n  ensure:\n    - exists: generated.txt\n      run: \"echo ensure-boom; exit 1\"\n")
	w.plan(pkgDef{id: "A"})
	ex := newFake(t, map[string][]round{"A": {edit(map[string]string{"a.txt": "a\n"})}})
	rep, code := w.run(ex, func(o *Options) { o.Gate, o.Full = "true", "true" })
	eq(t, "exit", code, 5)
	if !strings.Contains(*rep.Reason, "environment") {
		t.Errorf("reason %q", *rep.Reason)
	}
	eqJSON(t, "needs", w.needKinds(), []kindOf{{"", "environment"}})
	if !strings.Contains(w.state().Needs[0].Detail, "ensure-boom") {
		t.Errorf("detail %q", w.state().Needs[0].Detail)
	}
	eq(t, "reverts", len(w.events("revert")), 0)
	eq(t, "A", w.status("A"), "integrated")
	eq(t, "environment events", len(w.events("environment")), 1)
	w.assertIntClean()

	// Clearing the environment and fixing the config lets a rerun finish.
	w.checks("checks:\n  ensure:\n    - exists: generated.txt\n      run: \"echo gen > generated.txt\"\n")
	res, mcode := Mark(MarkOptions{State: w.statePath, ClearEnvironment: true})
	eq(t, "clear", mcode, 0)
	eqJSON(t, "clear result", res, map[string]any{"environment_cleared": true})
	rep, code = w.run(newFake(t, nil), func(o *Options) { o.Gate, o.Full = "true", "test -f generated.txt" })
	eq(t, "rerun exit", code, 0)
	eq(t, "rerun status", rep.Status, "done")
	if w.state().Stopped != nil {
		t.Error("stopped record survived")
	}
}

func TestSecondRevertOfSamePackageIsPostIntegrationNeed(t *testing.T) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A"}, pkgDef{id: "B"})
	ex := newFake(t, map[string][]round{
		"A": {edit(map[string]string{"a.txt": "a\n"})},
		"B": {edit(map[string]string{"b.txt": "POISON\n"}), edit(map[string]string{"b.txt": "POISON again\n"})},
	})
	ex.onReturn = finishAfter(t, w, "A", "B")
	rep, code := w.run(ex, func(o *Options) { o.Gate, o.Full = "true", poisonFull })
	eq(t, "exit", code, 1)
	eqJSON(t, "integrated", rep.Integrated, []string{"A"})
	eqJSON(t, "needs", w.needKinds(), []kindOf{{"B", "post-integration"}})
	if !strings.Contains(w.state().Needs[0].Detail, "tests/poison.test.ts") {
		t.Error("detail lacks the failing test")
	}
	eq(t, "reverts", len(w.events("revert")), 2)
	w.assertIntClean()
}

// --- rolling ---------------------------------------------------------------------------

func TestDependentStartsWhileUnrelatedPackageStillRuns(t *testing.T) {
	w := newWaveRepo(t)
	w.verifyMode = "final"
	w.plan(pkgDef{id: "A"}, pkgDef{id: "B", dependsOn: []string{"A"}}, pkgDef{id: "C"})
	bStarted := make(chan struct{})
	var once sync.Once
	var bBeforeC bool
	ex := newFake(t, map[string][]round{
		"A": {edit(map[string]string{"a.txt": "a\n"})}, "B": {edit(map[string]string{"b.txt": "b\n"})},
		"C": {edit(map[string]string{"c.txt": "c\n"})},
	})
	ex.onStart = func(pid string, _ spec) {
		if pid == "B" {
			once.Do(func() { close(bStarted) })
		}
	}
	ex.onReturn = func(pid string) {
		if pid == "C" {
			select {
			case <-bStarted:
				bBeforeC = true
			case <-time.After(20 * time.Second):
			}
		}
	}
	rep, code := w.run(ex)
	eq(t, "exit", code, 0)
	if !bBeforeC {
		t.Error("B did not start while C was still running")
	}
	got := append([]string{}, rep.Integrated...)
	slices.Sort(got)
	eqJSON(t, "integrated", got, []string{"A", "B", "C"})
	subjects := strings.Split(w.git(w.intPath(), "log", "--reverse", "--format=%s"), "\n")
	if slices.Index(subjects, "orchestra integrate pkg-A") > slices.Index(subjects, "orchestra integrate pkg-B") {
		t.Errorf("A integrated after B: %v", subjects)
	}
}

type concurrency struct {
	mu       sync.Mutex
	active   map[string]bool
	peak     int
	together map[[2]string]bool
}

func (c *concurrency) enter(pid string, _ spec) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for other := range c.active {
		pair := [2]string{min(pid, other), max(pid, other)}
		c.together[pair] = true
	}
	c.active[pid] = true
	c.peak = max(c.peak, len(c.active))
}

func (c *concurrency) leave(pid string) {
	time.Sleep(150 * time.Millisecond)
	c.mu.Lock()
	delete(c.active, pid)
	c.mu.Unlock()
}

func newConcurrency() *concurrency {
	return &concurrency{active: map[string]bool{}, together: map[[2]string]bool{}}
}

func TestWidthAndOverlapHoldWhileRolling(t *testing.T) {
	w := newWaveRepo(t)
	w.verifyMode = "final"
	w.plan(pkgDef{id: "A", owned: []string{"src/**"}}, pkgDef{id: "B", owned: []string{"src/b.txt"}},
		pkgDef{id: "C"}, pkgDef{id: "D"}, pkgDef{id: "E"})
	conc := newConcurrency()
	ex := newFake(t, map[string][]round{
		"A": {edit(map[string]string{"src/a.txt": "a\n"})}, "B": {edit(map[string]string{"src/b.txt": "b\n"})},
		"C": {edit(map[string]string{"c.txt": "c\n"})}, "D": {edit(map[string]string{"d.txt": "d\n"})},
		"E": {edit(map[string]string{"e.txt": "e\n"})},
	})
	ex.onStart, ex.onReturn = conc.enter, conc.leave
	rep, code := w.run(ex, func(o *Options) { o.MaxInFlight = 2 })
	eq(t, "exit", code, 0)
	eq(t, "integrated", len(rep.Integrated), 5)
	if conc.peak > 2 || conc.together[[2]string{"A", "B"}] {
		t.Errorf("peak %d, together %v", conc.peak, conc.together)
	}
}

func TestGatedRedFullStartsNothingMore(t *testing.T) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A"}, pkgDef{id: "B"}, pkgDef{id: "C"})
	ex := newFake(t, map[string][]round{
		"A": {edit(map[string]string{"a.txt": "a\n"})}, "B": {edit(map[string]string{"b.txt": "b\n"})},
		"C": {edit(map[string]string{"c.txt": "c\n"})},
	})
	ex.onReturn = func(pid string) {
		if pid == "B" {
			waitUntil(t, func() bool { return w.state().Stopped != nil })
		}
	}
	rep, code := w.run(ex, func(o *Options) { o.Gate, o.Full, o.MaxInFlight = "true", "false", 2 })
	eq(t, "exit", code, 5)
	eq(t, "reason", *rep.Reason, "full verification red at the last green SHA")
	got := ex.dispatched()
	slices.Sort(got)
	eqJSON(t, "dispatched", got, []string{"A", "B"})
	eqJSON(t, "pending", rep.Pending, []string{"C"})
}

// --- verify_mode final -------------------------------------------------------------------

const fastNoBad = "checks:\n  items:\n    - name: nobad\n      fast: true\n      run: \"! grep -rqs BAD --include=*.txt .\"\n"

const fullNoPoison = "checks:\n  items:\n    - name: nopoison\n      run: \"! grep -ls POISON *.txt\"\n"

func TestRedFastCheckIsRecordedAndBlocksNothing(t *testing.T) {
	w := newWaveRepo(t)
	w.verifyMode = "final"
	w.checks(fastNoBad)
	w.plan(pkgDef{id: "A"}, pkgDef{id: "B", dependsOn: []string{"A"}})
	ex := newFake(t, map[string][]round{"A": {edit(map[string]string{"a.txt": "BAD\n"})}, "B": {edit(map[string]string{"b.txt": "b\n"})}})
	rep, code := w.run(ex)
	eq(t, "exit", code, 0)
	eqJSON(t, "integrated", rep.Integrated, []string{"A", "B"})
	eq(t, "needs", len(w.needKinds()), 0)
	eq(t, "detail", w.state().Packages["A"].Detail, "fast check failed: nobad")
}

func TestRedFullCheckConvergesThroughFixupForTheChangingPackage(t *testing.T) {
	w := newWaveRepo(t)
	w.verifyMode = "final"
	w.checks(fullNoPoison)
	w.plan(pkgDef{id: "A"}, pkgDef{id: "B", dependsOn: []string{"A"}}, pkgDef{id: "C"})
	ex := newFake(t, map[string][]round{
		"A": {edit(map[string]string{"a.txt": "a\n"})}, "B": {edit(map[string]string{"b.txt": "POISON\n"})},
		"C": {edit(map[string]string{"c.txt": "c\n"})}, "fix-1": {edit(map[string]string{"b.txt": "fixed\n"})},
	})
	rep, code := w.run(ex)
	eq(t, "exit", code, 0)
	fix := ex.callsOf("fix-1")
	if len(fix) != 1 || ex.tokens[fix[0].Token].Class != "standard" {
		t.Errorf("fix-1 calls %+v", fix)
	}
	eqJSON(t, "fixup for", w.details("fixup", "*"), []any{map[string]any{"round": 1, "for": "B", "cls": "standard", "checks": []any{"nopoison"}}})
	got := append([]string{}, rep.Integrated...)
	slices.Sort(got)
	eqJSON(t, "integrated", got, []string{"A", "B", "C", "fix-1"})
	eq(t, "full runs", len(w.events("full-start")), 2)
}

func TestUnmatchedFailureGoesToCatchAllAndRoundsEndInNeed(t *testing.T) {
	w := newWaveRepo(t)
	w.verifyMode = "final"
	w.checks(fullNoPoison)
	w.plan(pkgDef{id: "A"}, pkgDef{id: "B"})
	conc := newConcurrency()
	ex := newFake(t, map[string][]round{
		"A": {edit(map[string]string{"a.txt": "a\n"})}, "B": {edit(map[string]string{"b.txt": "POISON\n"})},
		"fix-1": {edit(map[string]string{"b.txt": "fixed\n"})}, "fix-2": {edit(map[string]string{"x.txt": "x\n"})},
	})
	ex.onStart, ex.onReturn = conc.enter, conc.leave
	_, code := w.run(ex, func(o *Options) { o.Full, o.ConvergeRounds = "echo boom; exit 1", ptr(1) })
	eq(t, "exit", code, 5)
	eqJSON(t, "needs", w.needKinds(), []kindOf{{"", "post-integration"}})
	if !strings.Contains(w.state().Needs[0].Detail, "boom") {
		t.Error("detail lacks the failure")
	}
	var fixups []string
	for _, d := range w.details("fixup", "*") {
		fixups = append(fixups, fmt.Sprintf("%v/%v", d["for"], d["cls"]))
	}
	eqJSON(t, "fixups", fixups, []string{"B/standard", "<nil>/deep"})
	if conc.together[[2]string{"fix-1", "fix-2"}] {
		t.Error("the catch-all ran alongside another fix-up")
	}
}

// --- max_in_flight -----------------------------------------------------------------------

func TestAutoWidthFormula(t *testing.T) {
	for _, c := range []struct {
		cpus      int
		gib, want int
	}{{10, 64, 8}, {64, 128, 16}, {12, 8, 4}, {3, 2, 2}, {0, 8, 4}} {
		eq(t, fmt.Sprintf("autoWidth(%d, %dGiB)", c.cpus, c.gib), autoWidth(c.cpus, uint64(c.gib)<<30), c.want)
	}
	eq(t, "unreadable memory", autoWidth(8, 0), defaultMaxInFlight)
}
