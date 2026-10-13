package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/approvals"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
)

func TestPlanWarningsAreEmittedAndRunContinues(t *testing.T) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A", owned: []string{"a.txt"}})
	writeFile(t, filepath.Join(w.waveDir, "specs", "A.md"), "削除 old implementation\n")
	ex := newFake(t, map[string][]round{"A": {edit(map[string]string{"a.txt": "a\n"})}})
	rep, code := w.run(ex)
	eq(t, "exit", code, 0)
	if !strings.Contains(w.stderr.String(), "plan-warning: A:") {
		t.Errorf("stderr %q", w.stderr.String())
	}
	warns := w.events("plan-warning")
	if len(warns) != 1 || warns[0].Pkg != "A" || !strings.Contains(warns[0].Detail, "owns only literal paths") {
		t.Errorf("warnings %+v (report %+v)", warns, rep)
	}
}

func TestTwoIndependentPackagesOneWave(t *testing.T) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A"}, pkgDef{id: "B"})
	ex := newFake(t, map[string][]round{
		"A": {edit(map[string]string{"a.txt": "a\n"})}, "B": {edit(map[string]string{"b.txt": "b\n"})},
	})
	rep, code := w.run(ex)
	eq(t, "exit", code, 0)
	eq(t, "status", rep.Status, "done")
	eq(t, "waves", rep.Waves, 1)
	eqJSON(t, "integrated", rep.Integrated, []string{"A", "B"})
	if got, _ := w.intFile("a.txt"); got != "a\n" {
		t.Errorf("a.txt %q", got)
	}
	if got, _ := w.intFile("b.txt"); got != "b\n" {
		t.Errorf("b.txt %q", got)
	}
	if c := w.state().Packages["A"].Commit; c == nil || *c == "" {
		t.Error("no commit recorded")
	}
	if _, err := os.Stat(filepath.Join(w.repo, "a.txt")); err == nil {
		t.Error("the user's tree was touched")
	}
	files := ex.calls[0].PromptFiles
	if len(files) != 3 || !strings.HasSuffix(files[0], "preamble.md") ||
		!strings.Contains(files[1], filepath.Join("context", ex.calls[0].Pkg+".md")) ||
		!strings.Contains(files[2], "specs") {
		t.Fatalf("prompt files %v", files)
	}
	ctx, _ := readFile(files[1])
	for _, want := range []string{"ESCALATE", "shelf", "carry"} {
		if !strings.Contains(ctx, want) {
			t.Errorf("context lacks %q", want)
		}
	}
}

func TestDependencyChainTwoWaves(t *testing.T) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A"}, pkgDef{id: "B", dependsOn: []string{"A"}})
	bRound := func(t *testing.T, s spec, ex []string) map[string]any {
		if got, _ := readFile(filepath.Join(createTree(t, s), "a.txt")); got != "a\n" {
			t.Errorf("B does not start from A's integrated code: %q", got)
		}
		return edit(map[string]string{"b.txt": "b\n"})(t, s, ex)
	}
	ex := newFake(t, map[string][]round{"A": {edit(map[string]string{"a.txt": "a\n"})}, "B": {bRound}})
	rep, code := w.run(ex)
	eq(t, "exit", code, 0)
	eq(t, "waves", rep.Waves, 2)
	eqJSON(t, "dispatched", ex.dispatched(), []string{"A", "B"})
}

func TestOverlappingFilesOwnedSerialize(t *testing.T) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A", owned: []string{"src/**"}}, pkgDef{id: "B", owned: []string{"src/b.txt"}})
	ex := newFake(t, map[string][]round{
		"A": {edit(map[string]string{"src/a.txt": "a\n"})}, "B": {edit(map[string]string{"src/b.txt": "b\n"})},
	})
	rep, code := w.run(ex)
	eq(t, "exit", code, 0)
	eq(t, "waves", rep.Waves, 2)
	eqJSON(t, "dispatched", ex.dispatched(), []string{"A", "B"})
}

func TestMaxPackagesRespected(t *testing.T) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A"}, pkgDef{id: "B"}, pkgDef{id: "C"})
	ex := newFake(t, map[string][]round{
		"A": {edit(map[string]string{"a.txt": "a\n"})}, "B": {edit(map[string]string{"b.txt": "b\n"})},
		"C": {edit(map[string]string{"c.txt": "c\n"})},
	})
	rep, code := w.run(ex, func(o *Options) { o.MaxPackages = ptr(2) })
	eq(t, "exit", code, 5)
	got := ex.dispatched()
	slices.Sort(got)
	eqJSON(t, "dispatched", got, []string{"A", "B"})
	eqJSON(t, "pending", rep.Pending, []string{"C"})
}

// --- self-verify (gated) ------------------------------------------------------------

func TestFixViaCorrectionDeltaWithSession(t *testing.T) {
	w := newWaveRepo(t)
	w.checks(noBadCheck)
	w.plan(pkgDef{id: "A"})
	ex := newFake(t, map[string][]round{"A": {
		edit(map[string]string{"a.txt": "BAD\n"}, editOpt{session: "s-1"}),
		edit(map[string]string{"a.txt": "good\n"}, editOpt{session: "s-1"}),
	}})
	_, code := w.run(ex)
	eq(t, "exit", code, 0)
	if len(ex.calls) != 2 {
		t.Fatalf("calls %d", len(ex.calls))
	}
	corr := ex.calls[1].PromptFiles
	if len(corr) != 1 || !strings.HasSuffix(corr[0], filepath.Join("corrections", "A.md")) {
		t.Fatalf("correction prompt %v", corr)
	}
	text, _ := readFile(corr[0])
	if !strings.Contains(text, "nobad") || !strings.Contains(text, "do not commit") {
		t.Errorf("correction %q", text)
	}
	eq(t, "attempts", w.state().Packages["A"].Attempts, 1)
	if got, _ := w.intFile("a.txt"); got != "good\n" {
		t.Errorf("a.txt %q", got)
	}
	starts := w.details("dispatch-start", "A")
	if len(starts) != 2 || starts[1]["kind"] != "correction" || starts[1]["attempt"] != 2.0 {
		t.Errorf("dispatch-start %v", starts)
	}
	ends := w.details("check-end", "A")
	if len(ends) != 2 || ends[0]["status"] != "fail" || ends[1]["status"] != "pass" {
		t.Errorf("check-end %v", ends)
	}
}

func TestFixViaCorrectionFullWithoutSession(t *testing.T) {
	w := newWaveRepo(t)
	w.checks(noBadCheck)
	w.plan(pkgDef{id: "A"})
	ex := newFake(t, map[string][]round{"A": {
		edit(map[string]string{"a.txt": "BAD\n"}), edit(map[string]string{"a.txt": "good\n"}),
	}})
	_, code := w.run(ex)
	eq(t, "exit", code, 0)
	files := ex.calls[1].PromptFiles
	if len(files) != 4 || !strings.HasSuffix(files[3], filepath.Join("corrections", "A.md")) || !ex.calls[1].NoResume {
		t.Errorf("correction call %+v", ex.calls[1])
	}
}

func TestTwoFailuresNeedSelfVerifyAndBlockDependents(t *testing.T) {
	w := newWaveRepo(t)
	w.checks(noBadCheck)
	w.plan(pkgDef{id: "A"}, pkgDef{id: "B", dependsOn: []string{"A"}})
	ex := newFake(t, map[string][]round{"A": {
		edit(map[string]string{"a.txt": "BAD\n"}), edit(map[string]string{"a.txt": "BAD again\n"}),
	}})
	rep, code := w.run(ex)
	eq(t, "exit", code, 1)
	eq(t, "status", rep.Status, "done")
	eqJSON(t, "needs", rep.Needs, []map[string]any{{"id": "A", "kind": "self-verify"}})
	eqJSON(t, "blocked", rep.Blocked, []string{"B"})
	eqJSON(t, "dispatched", ex.dispatched(), []string{"A", "A"})
}

func TestEscalateInCorrectionAnswerIsANeed(t *testing.T) {
	w := newWaveRepo(t)
	w.checks(noBadCheck)
	w.plan(pkgDef{id: "A"})
	ex := newFake(t, map[string][]round{"A": {
		edit(map[string]string{"a.txt": "BAD\n"}),
		edit(map[string]string{"a.txt": "BAD\n"}, editOpt{answer: "ESCALATE: cannot fix this"}),
	}})
	_, code := w.run(ex)
	eq(t, "exit", code, 1)
	eqJSON(t, "needs", w.needKinds(), []kindOf{{"A", "escalate"}})
	if !strings.Contains(w.state().Needs[0].Detail, "cannot fix this") {
		t.Error("detail lost the answer")
	}
	eq(t, "check-starts", len(w.details("check-start", "A")), 1)
}

// --- needs -----------------------------------------------------------------------------

func TestEscalate(t *testing.T) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A"})
	ex := newFake(t, map[string][]round{"A": {edit(map[string]string{"a.txt": "a\n"}, editOpt{answer: "\n  ESCALATE: spec unclear\n"})}})
	_, code := w.run(ex)
	eq(t, "exit", code, 1)
	eqJSON(t, "needs", w.needKinds(), []kindOf{{"A", "escalate"}})
	if !strings.Contains(w.state().Needs[0].Detail, "spec unclear") {
		t.Error("detail")
	}
}

func TestNetworkEscalateIsANetworkNeedAndResumesWithTheDecision(t *testing.T) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A"}, pkgDef{id: "B"})
	ex := newFake(t, map[string][]round{
		"A": {edit(map[string]string{"a.txt": "a\n"}, editOpt{answer: "ESCALATE: network files.example.com\nwhy"}),
			edit(map[string]string{"a.txt": "a2\n"})},
		"B": {edit(map[string]string{"b.txt": "b\n"}, editOpt{answer: "ESCALATE: networking spec unclear"})},
	})
	rep, code := w.run(ex)
	eq(t, "exit", code, 1)
	kinds := w.needKinds()
	slices.SortFunc(kinds, func(a, b kindOf) int { return strings.Compare(a.ID, b.ID) })
	eqJSON(t, "needs", kinds, []kindOf{{"A", "network"}, {"B", "escalate"}})
	eqJSON(t, "denials", w.state().Packages["A"].NetworkDenials, []any{map[string]any{"host": "files.example.com", "port": 443}})
	eq(t, "approvals", rep.Approvals, 1)

	if _, err := approvals.Decide(w.statePath, "net:files.example.com", "deny", "unrelated to the spec", "instructor"); err != nil {
		t.Fatal(err)
	}
	eq(t, "A", w.status("A"), "pending")
	w.run(ex)
	last := ex.calls[len(ex.calls)-1].PromptFiles
	notes := 0
	for _, f := range last {
		if strings.HasSuffix(f, ".approvals.md") {
			notes++
		}
	}
	eq(t, "notes sent", notes, 1)
	eq(t, "A", w.status("A"), "integrated")
}

func TestDelegateThenMarkReadyIntegrates(t *testing.T) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A"})
	payload := map[string]any{"status": "delegate", "executor": "claude", "model": "sonnet", "effort": "high", "agent_type": "general-purpose"}
	ex := newFake(t, map[string][]round{"A": {result(payload, map[string]string{"a.txt": "by instructor\n"})}})
	_, code := w.run(ex)
	eq(t, "exit", code, 1)
	eqJSON(t, "needs", w.needKinds(), []kindOf{{"A", "delegate"}})
	var detail map[string]any
	_ = json.Unmarshal([]byte(w.state().Needs[0].Detail), &detail)
	if detail["token"] != "tok-0" || detail["agent_type"] != "general-purpose" || detail["tree"] == "" {
		t.Errorf("detail %v", detail)
	}
	res, mcode := Mark(MarkOptions{State: w.statePath, Pkg: "A", Status: "ready"})
	eq(t, "mark", mcode, 0)
	eqJSON(t, "mark result", res, map[string]any{"pkg": "A", "status": "verifying"})
	eq(t, "needs", len(w.state().Needs), 0)
	eq(t, "A", w.status("A"), "verifying")
	rep, code := w.run(newFake(t, nil))
	eq(t, "exit", code, 0)
	eqJSON(t, "integrated", rep.Integrated, []string{"A"})
	if got, _ := w.intFile("a.txt"); got != "by instructor\n" {
		t.Errorf("a.txt %q", got)
	}
}

func TestNeedsPermissionAndRunawayAreNeedsNotFallbacks(t *testing.T) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A"}, pkgDef{id: "B"})
	perm := map[string]any{"status": "needs-permission", "reason": "sandbox", "executor": "pi", "session_id": "s1",
		"sandbox": map[string]any{"denials": []any{map[string]any{"path": "/h/.config/x", "op": "write", "auto": false,
			"grant_candidate": "/h/.config/x", "why": "not cache"}}}}
	runaway := map[string]any{"status": "runaway", "reason": "tool-idle", "executor": "pi",
		"runaway": map[string]any{"reason": "tool-idle", "elapsed_s": 5.1, "last_tool": map[string]any{"name": "bash", "args_preview": "{}"}}}
	ex := newFake(t, map[string][]round{"A": {result(perm, nil)}, "B": {result(runaway, nil)}})
	_, code := w.run(ex)
	eq(t, "exit", code, 1)
	eq(t, "calls", len(ex.calls), 2)
	for _, n := range w.state().Needs {
		var d map[string]any
		_ = json.Unmarshal([]byte(n.Detail), &d)
		switch *n.ID {
		case "A":
			eq(t, "A kind", n.Kind, "permission")
			eqJSON(t, "A detail", d, map[string]any{"denials": []any{map[string]any{
				"path": "/h/.config/x", "grant_candidate": "/h/.config/x", "why": "not cache"}}})
		case "B":
			eq(t, "B kind", n.Kind, "runaway")
			eqJSON(t, "B detail", d, map[string]any{"reason": "tool-idle", "last_tool": "bash"})
		}
	}
}

func TestUnavailableRetriesWithExhausted(t *testing.T) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A"})
	ex := newFake(t, map[string][]round{"A": {
		result(map[string]any{"status": "unavailable", "executor": "pi"}, nil),
		edit(map[string]string{"a.txt": "a\n"}),
	}})
	_, code := w.run(ex)
	eq(t, "exit", code, 0)
	eqJSON(t, "first exhausted", ex.calls[0].Exhausted, []string{})
	eqJSON(t, "second exhausted", ex.calls[1].Exhausted, []string{"pi"})
	eq(t, "executor", *w.state().Packages["A"].Executor, "pi")
}

func TestAllUnavailableStopsWithPackagePending(t *testing.T) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A"})
	ex := newFake(t, map[string][]round{"A": {
		result(map[string]any{"status": "unavailable", "executor": "pi"}, nil),
		result(map[string]any{"status": "unroutable", "route": map[string]any{}}, nil),
	}})
	rep, code := w.run(ex)
	eq(t, "exit", code, 5)
	eq(t, "status", rep.Status, "stopped")
	if rep.Reason == nil || !strings.Contains(*rep.Reason, "executor unavailable") {
		t.Errorf("reason %v", rep.Reason)
	}
	eq(t, "A", w.status("A"), "pending")
	eqJSON(t, "pending", rep.Pending, []string{"A"})
}

func TestEscalateCarriesContextAndMarksScope(t *testing.T) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A", owned: []string{"a.txt"}})
	ex := newFake(t, map[string][]round{"A": {edit(map[string]string{"a.txt": "a\n"}, editOpt{answer: "ESCALATE: blocked by src/other.py"})}})
	ex.onStart = func(pid string, _ spec) {
		writeFile(t, filepath.Join(w.waveDir, "carry", "A.carry.md"), "please update src/shared.py\n")
	}
	_, code := w.run(ex)
	eq(t, "exit", code, 1)
	eqJSON(t, "needs", w.needKinds(), []kindOf{{"A", "scope"}})
	d := w.state().Needs[0].Detail
	for _, want := range []string{"--- carry ---", "src/shared.py", "--- outside files_owned ---"} {
		if !strings.Contains(d, want) {
			t.Errorf("detail lacks %q: %q", want, d)
		}
	}
}

func TestEscalateProseWithDottedWordsIsNotScope(t *testing.T) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A", owned: []string{"a.txt"}})
	ex := newFake(t, map[string][]round{"A": {edit(map[string]string{"a.txt": "a\n"}, editOpt{answer: "ESCALATE: need a decision on naming. Thanks, e.g. later. Keep a.txt as is."})}})
	_, code := w.run(ex)
	eq(t, "exit", code, 1)
	eqJSON(t, "needs", w.needKinds(), []kindOf{{"A", "escalate"}})
}

func TestMarkWidenAndRecheck(t *testing.T) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A"}, pkgDef{id: "B"})
	del := map[string]any{"status": "delegate", "executor": "claude"}
	ex := newFake(t, map[string][]round{
		"A": {result(del, map[string]string{"a.txt": "a\n"})}, "B": {result(del, map[string]string{"b.txt": "b\n"})},
	})
	_, code := w.run(ex)
	eq(t, "exit", code, 1)
	before := w.state()
	if _, c := Mark(MarkOptions{State: w.statePath, Recheck: []string{"A", "ZZ"}}); c != 2 {
		t.Errorf("unknown recheck id: exit %d", c)
	}
	eqJSON(t, "needs untouched", w.state().Needs, before.Needs)
	if _, c := Mark(MarkOptions{State: w.statePath, Pkg: "A", Widen: []string{"src/*.py", "docs/*.md"}}); c != 0 {
		t.Fatal("widen")
	}
	raw, _ := readFile(filepath.Join(w.waveDir, "plan-overrides.json"))
	var ov map[string]map[string][]string
	_ = json.Unmarshal([]byte(raw), &ov)
	eqJSON(t, "overrides", ov["A"]["files_owned_add"], []string{"src/*.py", "docs/*.md"})
	eq(t, "A", w.status("A"), "pending")
	if _, c := Mark(MarkOptions{State: w.statePath, Recheck: []string{"B"}}); c != 0 {
		t.Fatal("recheck")
	}
	eq(t, "B", w.status("B"), "verifying")
	eq(t, "needs", len(w.state().Needs), 0)
	nothing := newFake(t, map[string][]round{"A": {edit(map[string]string{"a.txt": "a\n", "src/x.py": "x\n"})}})
	rep, code := w.run(nothing)
	eq(t, "exit", code, 0)
	eqJSON(t, "integrated", rep.Integrated, []string{"A", "B"})
	ctx, _ := readFile(nothing.calls[0].PromptFiles[1])
	if !strings.Contains(ctx, "src/*.py") {
		t.Error("the widened glob is not in the context")
	}
}

func TestEmptyAfterRefreshBecomesNeed(t *testing.T) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A", owned: []string{"a.txt"}}, pkgDef{id: "B", dependsOn: []string{"A"}, owned: []string{"a.txt", "b.txt"}})
	w.staleB(map[string]string{"a.txt": "a\n"})
	ex := newFake(t, map[string][]round{"A": {edit(map[string]string{"a.txt": "a\n"})}, "B": {edit(nil)}})
	_, code := w.run(ex)
	eq(t, "exit", code, 1)
	eqJSON(t, "needs", w.needKinds(), []kindOf{{"B", "empty-after-refresh"}})
	eq(t, "A", w.status("A"), "integrated")
}

// staleB leaves a pkg-B tree from an earlier attempt on the old base.
func (w *waveRepo) staleB(files map[string]string) {
	head := w.git(w.repo, "rev-parse", "HEAD")
	created, err := isolate.Create(w.repo, "wave-int", isolate.CreateOptions{Backend: isolate.BackendGit, Onto: head})
	if err != nil {
		w.t.Fatal(err)
	}
	_ = isolate.WriteRole(created.Path, isolate.RoleIntegration)
	stale, err := isolate.Create(created.Path, "pkg-B", isolate.CreateOptions{Backend: isolate.BackendGit})
	if err != nil {
		w.t.Fatal(err)
	}
	for rel, text := range files {
		writeFile(w.t, filepath.Join(stale.Path, rel), text)
	}
}

func TestStaleTreeRefreshedBeforeDispatch(t *testing.T) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A"}, pkgDef{id: "B", dependsOn: []string{"A"}})
	w.staleB(map[string]string{"b.txt": "b from before\n"})
	bRound := func(t *testing.T, s spec, ex []string) map[string]any {
		path := createTree(t, s)
		if got, _ := readFile(filepath.Join(path, "a.txt")); got != "a\n" {
			t.Errorf("a.txt in B %q", got)
		}
		if got, _ := readFile(filepath.Join(path, "b.txt")); got != "b from before\n" {
			t.Errorf("b.txt in B %q", got)
		}
		return edit(map[string]string{"b2.txt": "b2\n"})(t, s, ex)
	}
	ex := newFake(t, map[string][]round{"A": {edit(map[string]string{"a.txt": "a\n"})}, "B": {bRound}})
	_, code := w.run(ex)
	eq(t, "exit", code, 0)
	ctx, _ := readFile(ex.calls[1].PromptFiles[1])
	if !strings.Contains(ctx, "a.txt") || !ex.calls[1].NoResume {
		t.Errorf("refresh note / no_resume: %v %q", ex.calls[1].NoResume, ctx)
	}
	if got, _ := w.intFile("b.txt"); got != "b from before\n" {
		t.Errorf("b.txt %q", got)
	}
	if len(w.details("refresh", "B")) == 0 {
		t.Error("no refresh event")
	}
}
