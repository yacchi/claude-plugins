package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/state"
)

// finishAfter: package order[i] finishes only once order[i-1] left the
// in-flight statuses, so independent packages integrate in a fixed order.
func finishAfter(t *testing.T, w *waveRepo, order ...string) func(string) {
	return func(pid string) {
		i := slices.Index(order, pid)
		if i <= 0 {
			return
		}
		before := order[i-1]
		waitUntil(t, func() bool {
			p := w.state().Packages[before]
			return p != nil && !slices.Contains([]string{"pending", "implementing", "verifying", "fixing", "ready"}, p.Status)
		})
	}
}

func TestConflictInOneWaveRollsBackLater(t *testing.T) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A"}, pkgDef{id: "B"})
	linesA := strings.Replace(baseLines, "line 05\n", "A's line\n", 1)
	linesB := strings.Replace(baseLines, "line 05\n", "B's line\n", 1)
	ex := newFake(t, map[string][]round{
		"A": {edit(map[string]string{"shared.txt": linesA})}, "B": {edit(map[string]string{"shared.txt": linesB})},
	})
	ex.onReturn = finishAfter(t, w, "A", "B")
	rep, code := w.run(ex)
	eq(t, "exit", code, 1)
	eqJSON(t, "integrated", rep.Integrated, []string{"A"})
	eqJSON(t, "needs", w.needKinds(), []kindOf{{"B", "conflict"}})
	eq(t, "detail", w.state().Needs[0].Detail, `{"files": ["shared.txt"], "stage": "integrate"}`)
	got, _ := w.intFile("shared.txt")
	if !strings.Contains(got, "A's line") || strings.Contains(got, "<<<<<<<") {
		t.Errorf("shared.txt %q", got)
	}
}

func TestPostIntegrationFailureRevertsCulprit(t *testing.T) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A"}, pkgDef{id: "B"})
	ex := newFake(t, map[string][]round{
		"A": {edit(map[string]string{"a.txt": "a\n"})}, "B": {edit(map[string]string{"b.txt": "POISON\n"})},
	})
	ex.onReturn = finishAfter(t, w, "A", "B")
	rep, code := w.run(ex, func(o *Options) { o.Gate = "! grep -rqs POISON --include=*.txt ." })
	eq(t, "exit", code, 1)
	eqJSON(t, "integrated", rep.Integrated, []string{"A"})
	eqJSON(t, "needs", w.needKinds(), []kindOf{{"B", "post-integration"}})
	if _, ok := w.intFile("b.txt"); ok {
		t.Error("b.txt stayed integrated")
	}
	reverts := w.events("revert")
	if len(reverts) != 1 || reverts[0].Pkg != "B" || reverts[0].Detail != `{"result": "reverted"}` {
		t.Errorf("revert events %+v", reverts)
	}
	for _, e := range w.events("") {
		if strings.HasPrefix(e.Pkg, "pkg-") {
			t.Errorf("event %s names a task, not a package: %s", e.Event, e.Pkg)
		}
	}
	eqJSON(t, "integrate-start", w.details("integrate-start", "*"), []any{
		map[string]any{"tasks": []any{"A"}}, map[string]any{"tasks": []any{"B"}}})
}

func TestDefaultGateRunsCheckOverTheWave(t *testing.T) {
	w := newWaveRepo(t)
	w.checks("checks:\n  items:\n    - name: notboth\n      run: \"! ( test -f a.txt && test -f b.txt )\"\n")
	w.plan(pkgDef{id: "A"}, pkgDef{id: "B"})
	ex := newFake(t, map[string][]round{
		"A": {edit(map[string]string{"a.txt": "a\n"})}, "B": {edit(map[string]string{"b.txt": "b\n"})},
	})
	ex.onReturn = finishAfter(t, w, "A", "B")
	rep, code := w.run(ex)
	eq(t, "exit", code, 1)
	eqJSON(t, "integrated", rep.Integrated, []string{"A"})
	eqJSON(t, "needs", w.needKinds(), []kindOf{{"B", "post-integration"}})
}

func TestFullRedStopsAndOnGreenSkipped(t *testing.T) {
	w := newWaveRepo(t)
	marker := filepath.Join(w.tmp, "on-green")
	w.plan(pkgDef{id: "A"}, pkgDef{id: "B", dependsOn: []string{"A"}})
	ex := newFake(t, map[string][]round{
		"A": {edit(map[string]string{"a.txt": "a\n"})}, "B": {edit(map[string]string{"b.txt": "b\n"})},
	})
	rep, code := w.run(ex, func(o *Options) { o.Full, o.OnGreen = "false", "touch '"+marker+"'" })
	eq(t, "exit", code, 5)
	eq(t, "reason", *rep.Reason, "full verification red at the last green SHA")
	eqJSON(t, "dispatched", ex.dispatched(), []string{"A"})
	if _, err := os.Stat(marker); err == nil {
		t.Error("on-green ran")
	}
	eq(t, "green = base", w.lastGreenSHA(), *w.state().Integration.Base)
}

func (w *waveRepo) lastGreenSHA() string {
	r := &runner{stateDir: w.waveDir}
	return r.lastGreen()
}

func TestOnGreenRunsInGreenTreeAtIntegratedSHAAndIsRemovedOnDone(t *testing.T) {
	w := newWaveRepo(t)
	log := filepath.Join(w.tmp, "green.log")
	w.plan(pkgDef{id: "A"}, pkgDef{id: "B", dependsOn: []string{"A"}})
	ex := newFake(t, map[string][]round{
		"A": {edit(map[string]string{"a.txt": "a\n"})}, "B": {edit(map[string]string{"b.txt": "b\n"})},
	})
	cmd := fmt.Sprintf(`echo "{sha} $WAVE_GREEN_SHA $(pwd -P)" >> '%s'; exit 3`, log)
	var greenSeen bool
	ex.onStart = func(pid string, _ spec) {
		if pid == "B" {
			greenSeen = isDir(filepath.Join(w.waveDir, "green-tree"))
		}
	}
	rep, code := w.run(ex, func(o *Options) { o.Full, o.OnGreen = "true", cmd })
	eq(t, "exit", code, 0)
	eqJSON(t, "integrated", rep.Integrated, []string{"A", "B"})
	text, _ := readFile(log)
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) != 2 {
		t.Fatalf("on-green ran %d times: %q", len(lines), text)
	}
	head := w.git(w.intPath(), "rev-parse", "HEAD")
	f := strings.Fields(lines[1])
	eq(t, "{sha}", f[0], head)
	eq(t, "WAVE_GREEN_SHA", f[1], head)
	eq(t, "cwd", f[2], filepath.Join(w.waveDir, "green-tree"))
	eq(t, "same tree reused", strings.Fields(lines[0])[2], f[2])
	if !greenSeen {
		t.Error("the green tree was not there between greens")
	}
	if isDir(filepath.Join(w.waveDir, "green-tree")) {
		t.Error("the green tree survived a done run")
	}
	for _, d := range w.details("on-green", "*") {
		eq(t, "on-green exit", d["exit"], any(3.0))
	}
	eq(t, "green = head", w.lastGreenSHA(), head)
}

func TestStageEvents(t *testing.T) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A"}, pkgDef{id: "B"})
	ex := newFake(t, map[string][]round{
		"A": {edit(map[string]string{"a.txt": "a\n"})}, "B": {edit(map[string]string{"b.txt": "b\n"})},
	})
	_, code := w.run(ex, func(o *Options) { o.Gate = "true" })
	eq(t, "exit", code, 0)
	for _, pid := range []string{"A", "B"} {
		eqJSON(t, "dispatch-start "+pid, w.details("dispatch-start", pid),
			[]any{map[string]any{"attempt": 1, "cls": "light", "kind": "implement"}})
		end := w.details("dispatch-end", pid)
		if len(end) != 1 || end[0]["status"] != "ok" || end[0]["executor"] != "pi" {
			t.Errorf("dispatch-end %v", end)
		}
		eq(t, "check-start "+pid, len(w.details("check-start", pid)), 1)
	}
	var tasks []string
	for _, d := range w.details("integrate-start", "*") {
		for _, x := range d["tasks"].([]any) {
			tasks = append(tasks, x.(string))
		}
	}
	slices.Sort(tasks)
	eqJSON(t, "integrated tasks", tasks, []string{"A", "B"})
	for _, e := range w.events("") {
		if e.Event == "dispatch-start" && e.Detail != `{"attempt": 1, "cls": "light", "kind": "implement"}` {
			t.Errorf("detail not sorted-key JSON: %q", e.Detail)
		}
	}
	reg, _ := readFile(os.Getenv("ORCHESTRA_WAVE_REGISTRY"))
	if !strings.Contains(reg, `"into": "wave-int"`) || !strings.Contains(reg, w.statePath) {
		t.Errorf("registry %q", reg)
	}
}

func TestDirtyIntegrationTreeEmitsIntegrationDirty(t *testing.T) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A"})
	created, _ := isolate.Create(w.repo, "wave-int", isolate.CreateOptions{Backend: isolate.BackendGit})
	writeFile(t, filepath.Join(created.Path, "pre-dispatch.txt"), "dirty\n")
	ex := newFake(t, map[string][]round{"A": {edit(map[string]string{"a.txt": "a\n"})}})
	w.run(ex, func(o *Options) { o.Full = "echo changed > a.txt; echo extra > full.txt" })
	var after []any
	for _, d := range w.details("integration-dirty", "") {
		after = append(after, d["after"])
	}
	if !slices.Contains(after, any("pre-dispatch")) || !slices.Contains(after, any("full")) {
		t.Errorf("integration-dirty after %v", after)
	}
}

// --- stop / resume ---------------------------------------------------------------------

func TestStopFileMidRun(t *testing.T) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A"}, pkgDef{id: "B", dependsOn: []string{"A"}})
	ex := newFake(t, map[string][]round{
		"A": {edit(map[string]string{"a.txt": "a\n"})}, "B": {edit(map[string]string{"b.txt": "b\n"})},
	})
	ex.onStart = func(pid string, _ spec) {
		if pid == "A" {
			_ = state.RequestStop(w.statePath, "enough")
		}
	}
	rep, code := w.run(ex, func(o *Options) { o.Full, o.OnGreen = "true", "true" })
	eq(t, "exit", code, 5)
	eqJSON(t, "dispatched", ex.dispatched(), []string{"A"})
	eqJSON(t, "integrated", rep.Integrated, []string{"A"})
	eqJSON(t, "pending", rep.Pending, []string{"B"})
	if !isDir(filepath.Join(w.waveDir, "green-tree")) {
		t.Error("the green tree is kept on stop")
	}
}

func TestResumeAfterCrashVerifiesWithoutRedispatch(t *testing.T) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A"})
	head := w.git(w.repo, "rev-parse", "HEAD")
	created, _ := isolate.Create(w.repo, "wave-int", isolate.CreateOptions{Backend: isolate.BackendGit, Onto: head})
	_ = isolate.WriteRole(created.Path, isolate.RoleIntegration)
	pkg, _ := isolate.Create(created.Path, "pkg-A", isolate.CreateOptions{Backend: isolate.BackendGit})
	writeFile(t, filepath.Join(pkg.Path, "a.txt"), "survived\n")
	store := &state.Store{Path: w.statePath, Clock: wallClock}
	if _, err := store.Init(w.planPath, []string{"A"}, "wave-int"); err != nil {
		t.Fatal(err)
	}
	_ = store.SetStatus("A", "implementing", "", func(p *state.Package) { p.Tree = &pkg.Path })
	rep, code := w.run(newFake(t, nil))
	eq(t, "exit", code, 0)
	eqJSON(t, "integrated", rep.Integrated, []string{"A"})
	if got, _ := w.intFile("a.txt"); got != "survived\n" {
		t.Errorf("a.txt %q", got)
	}
}

func TestResumeWithoutChangesGoesBackToPending(t *testing.T) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A"})
	store := &state.Store{Path: w.statePath, Clock: wallClock}
	_, _ = store.Init(w.planPath, []string{"A"}, "wave-int")
	_ = store.SetStatus("A", "implementing", "", nil)
	ex := newFake(t, map[string][]round{"A": {edit(map[string]string{"a.txt": "a\n"})}})
	_, code := w.run(ex)
	eq(t, "exit", code, 0)
	eqJSON(t, "dispatched", ex.dispatched(), []string{"A"})
}

type fakeClock struct {
	now    float64
	sleeps []float64
	after  func()
}

func (c *fakeClock) clock() float64 { return c.now }
func (c *fakeClock) sleep(s float64) {
	c.sleeps = append(c.sleeps, s)
	c.now += s
	if c.after != nil {
		c.after()
	}
}

func (w *waveRepo) cooldownUntil(until float64) {
	dir := filepath.Join(w.home, ".claude", "orchestra")
	writeFile(w.t, filepath.Join(dir, "executor-state.json"),
		fmt.Sprintf(`{"pi": {"reason": "quota", "until": %v}}`, until))
}

func TestResumeOnResetWaitsUntilCooldownExpiry(t *testing.T) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A"})
	unavailable := result(map[string]any{"status": "unavailable", "executor": "pi"}, nil)
	ex := newFake(t, map[string][]round{"A": {unavailable, unavailable, edit(map[string]string{"a.txt": "a\n"})}})
	c := &fakeClock{now: 1000}
	w.clock, w.sleep = c.clock, c.sleep
	w.cooldownUntil(1100)
	rep, code := w.run(ex, func(o *Options) { o.ResumeOnReset = true })
	eq(t, "exit", code, 0)
	eqJSON(t, "integrated", rep.Integrated, []string{"A"})
	sum := 0.0
	for _, s := range c.sleeps {
		sum += s
		if s < 0 || s > 30 {
			t.Errorf("sleep %v", s)
		}
	}
	if c.now < 1100 || sum != 100 {
		t.Errorf("now %v slept %v", c.now, sum)
	}
	eqJSON(t, "resume-wait", w.details("resume-wait", ""), []any{map[string]any{"until": 1100.0, "reason": "executor unavailable"}})
}

func TestResumeOnResetStopsOnStopFileOrStopAt(t *testing.T) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A"})
	unavailable := result(map[string]any{"status": "unavailable", "executor": "pi"}, nil)
	w.cooldownUntil(1100)
	c := &fakeClock{now: 1000, after: func() { _ = state.RequestStop(w.statePath, "enough") }}
	w.clock, w.sleep = c.clock, c.sleep
	rep, code := w.run(newFake(t, map[string][]round{"A": {unavailable, unavailable}}), func(o *Options) { o.ResumeOnReset = true })
	eq(t, "exit", code, 5)
	eq(t, "reason", *rep.Reason, "stop requested")
	eq(t, "sleeps", len(c.sleeps), 1)

	w2 := newWaveRepo(t)
	w2.plan(pkgDef{id: "A"})
	w2.cooldownUntil(1100)
	c2 := &fakeClock{now: 1000}
	w2.clock, w2.sleep = c2.clock, c2.sleep
	rep, code = w2.run(newFake(t, map[string][]round{"A": {unavailable, unavailable}}),
		func(o *Options) { o.ResumeOnReset, o.StopAt = true, ptr(1050.0) })
	eq(t, "exit", code, 5)
	eq(t, "reason", *rep.Reason, "stop-at reached")
	eq(t, "sleeps", len(c2.sleeps), 0)
}

func TestUIStartAndFailure(t *testing.T) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A"})
	_, code := w.run(newFake(t, map[string][]round{"A": {edit(map[string]string{"a.txt": "a\n"})}}))
	eq(t, "exit", code, 0)
	if !strings.Contains(w.stderr.String(), "ui: http://127.0.0.1:1/") {
		t.Errorf("stderr %q", w.stderr.String())
	}
	eqJSON(t, "ui event", w.details("ui", ""), []any{map[string]any{"url": "http://127.0.0.1:1/"}})

	w2 := newWaveRepo(t)
	w2.plan(pkgDef{id: "A"})
	w2.uiErr = fmt.Errorf("boom")
	_, code = w2.run(newFake(t, map[string][]round{"A": {edit(map[string]string{"a.txt": "a\n"})}}))
	eq(t, "exit", code, 0)
	if !strings.Contains(w2.stderr.String(), "warning: could not start ui: boom") {
		t.Errorf("stderr %q", w2.stderr.String())
	}

	w3 := newWaveRepo(t)
	w3.plan(pkgDef{id: "A"})
	_, code = w3.run(newFake(t, map[string][]round{"A": {edit(map[string]string{"a.txt": "a\n"})}}), func(o *Options) { o.NoUI = true })
	eq(t, "exit", code, 0)
	eq(t, "ui calls", w3.uiCalls, 0)
	w4 := newWaveRepo(t)
	t.Setenv("ORCHESTRA_WAVE_NO_UI", "1")
	w4.plan(pkgDef{id: "A"})
	w4.run(newFake(t, map[string][]round{"A": {edit(map[string]string{"a.txt": "a\n"})}}))
	eq(t, "ui calls with ORCHESTRA_WAVE_NO_UI", w4.uiCalls, 0)
}

func TestNotifyCmdReceivesNeedAndDone(t *testing.T) {
	w := newWaveRepo(t)
	log := filepath.Join(w.tmp, "notify.log")
	w.plan(pkgDef{id: "A"}, pkgDef{id: "B"})
	ex := newFake(t, map[string][]round{
		"A": {edit(map[string]string{"a.txt": "a\n"}, editOpt{answer: "ESCALATE: no"})},
		"B": {edit(map[string]string{"b.txt": "b\n"})},
	})
	cmd := fmt.Sprintf(`printf '%%s ' "$WAVE_EVENT" >> '%s'; cat >> '%s'; echo >> '%s'`, log, log, log)
	_, code := w.run(ex, func(o *Options) { o.NotifyCmd = cmd })
	eq(t, "exit", code, 1)
	text, _ := readFile(log)
	var kinds []string
	for _, l := range nonBlank(strings.Split(text, "\n")) {
		kinds = append(kinds, strings.SplitN(l, " ", 2)[0])
		if strings.HasPrefix(l, "need ") && (!strings.Contains(l, `"pkg": "A"`) || !strings.Contains(l, `"kind": "escalate"`)) {
			t.Errorf("need payload %q", l)
		}
	}
	slices.Sort(kinds)
	eqJSON(t, "events", kinds, []string{"done", "need"})
}
