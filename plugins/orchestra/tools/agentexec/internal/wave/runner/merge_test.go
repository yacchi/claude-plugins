package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// newMergeRepo is a wave repo whose request workspace lives under the
// test's temp dir, never the real state dir. It lands in ask mode, so the
// merge tests do not move the checkout; the land tests set auto themselves.
func newMergeRepo(t *testing.T) *waveRepo {
	t.Helper()
	w := newWaveRepo(t)
	t.Setenv("ORCHESTRA_REQUEST_ROOT", filepath.Join(w.tmp, "requests"))
	setLandMode(t, w, "ask")
	return w
}

// setLandMode rewrites the test home's config to isolation.land = mode.
func setLandMode(t *testing.T, w *waveRepo, mode string) {
	t.Helper()
	writeFile(t, filepath.Join(w.home, ".claude", "orchestra.yaml"), "interactive_host: none\nisolation:\n  land: "+mode+"\n")
}

// withCommitMessage gives the written plan its top-level commit_message.
func withCommitMessage(t *testing.T, w *waveRepo, msg string) {
	t.Helper()
	b, err := os.ReadFile(w.planPath)
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	p["commit_message"] = msg
	out, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, w.planPath, string(out))
}

// mergeJSON is the report's JSON form.
func mergeJSON(t *testing.T, rep Report) string {
	t.Helper()
	b, err := rep.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// mergeRequests is every open request of the test's repo.
func mergeRequests(t *testing.T) []isolate.Request {
	t.Helper()
	reqs, err := isolate.ListRequests()
	if err != nil {
		t.Fatal(err)
	}
	return reqs
}

// mergeAssembly is the one assembly of the one request the run created.
func mergeAssembly(t *testing.T, w *waveRepo) string {
	t.Helper()
	reqs := mergeRequests(t)
	if len(reqs) != 1 || len(reqs[0].Repos) != 1 {
		t.Fatalf("requests %+v, want one request with one repo", reqs)
	}
	return paths.RealPath(reqs[0].Repos[0].Assembly)
}

func mergeSubject(t *testing.T, w *waveRepo, assembly string) string {
	t.Helper()
	return w.git(assembly, "log", "-1", "--format=%s")
}

func TestMergeDoneRunSquashesIntoAssembly(t *testing.T) {
	w := newMergeRepo(t)
	base := w.git(w.repo, "rev-parse", "HEAD")
	w.plan(pkgDef{id: "A", owned: []string{"a.txt"}}, pkgDef{id: "B", owned: []string{"b.txt"}, dependsOn: []string{"A"}})
	ex := newFake(t, map[string][]round{
		"A": {edit(map[string]string{"a.txt": "a\n"})}, "B": {edit(map[string]string{"b.txt": "b\n"})},
	})
	rep, code := w.run(ex, func(o *Options) { o.Merge = true; o.Title = "feat: ab" })
	eq(t, "exit", code, 0)
	eq(t, "status", rep.Status, "done")
	if rep.Merge == nil {
		t.Fatal("no merge in the report")
	}
	eq(t, "merge status", rep.Merge.Status, "merged")
	if !strings.HasPrefix(FormatText(rep), "wave run done") || !strings.Contains(FormatText(rep), "\n  merge: merged ") {
		t.Errorf("text report %q lacks the merge line", FormatText(rep))
	}

	assembly := mergeAssembly(t, w)
	eq(t, "subject", mergeSubject(t, w, assembly), "feat: ab")
	eq(t, "commits past the checkout", w.git(assembly, "rev-list", "--count", base+"..HEAD"), "1")
	for rel, want := range map[string]string{"a.txt": "a\n", "b.txt": "b\n"} {
		if got, _ := readFile(filepath.Join(assembly, rel)); got != want {
			t.Errorf("assembly %s = %q, want %q", rel, got, want)
		}
	}
	if _, err := os.Stat(w.intPath()); !os.IsNotExist(err) {
		t.Errorf("integration tree still present: %v", err)
	}
	merges := w.events("merge")
	if len(merges) != 1 {
		t.Fatalf("merge events %+v, want one", merges)
	}
	if got := w.details("merge", "*")[0]["status"]; got != "merged" {
		t.Errorf("merge event status %v, want merged", got)
	}
}

func TestMergeSubjectFallsBackToRunID(t *testing.T) {
	w := newMergeRepo(t)
	w.plan(pkgDef{id: "A"})
	ex := newFake(t, map[string][]round{"A": {edit(map[string]string{"a.txt": "a\n"})}})
	rep, code := w.run(ex, func(o *Options) { o.Merge = true; o.RunID = "run-7" })
	eq(t, "exit", code, 0)
	eq(t, "merge status", rep.Merge.Status, "merged")
	eq(t, "subject", mergeSubject(t, w, mergeAssembly(t, w)), "run-7")
}

func TestMergeSubjectFallsBackToIntoTask(t *testing.T) {
	w := newMergeRepo(t)
	w.plan(pkgDef{id: "A"})
	ex := newFake(t, map[string][]round{"A": {edit(map[string]string{"a.txt": "a\n"})}})
	rep, code := w.run(ex, func(o *Options) { o.Merge = true })
	eq(t, "exit", code, 0)
	eq(t, "merge status", rep.Merge.Status, "merged")
	eq(t, "subject", mergeSubject(t, w, mergeAssembly(t, w)), "wave-int")
}

func TestMergeNotRunWhenNeedsRemain(t *testing.T) {
	w := newMergeRepo(t)
	w.plan(pkgDef{id: "A"})
	ex := newFake(t, map[string][]round{"A": {edit(map[string]string{"a.txt": "a\n"}, editOpt{answer: "ESCALATE: spec unclear"})}})
	rep, code := w.run(ex, func(o *Options) { o.Merge = true })
	eq(t, "exit", code, 1)
	if rep.Merge != nil {
		t.Errorf("merge ran with a need: %+v", rep.Merge)
	}
	if !strings.Contains(mergeJSON(t, rep), `"merge":null`) {
		t.Errorf("report %s lacks null merge", mergeJSON(t, rep))
	}
	if _, err := os.Stat(w.intPath()); err != nil {
		t.Errorf("integration tree removed: %v", err)
	}
}

func TestMergeConflictBecomesMergeNeed(t *testing.T) {
	w := newMergeRepo(t)
	w.plan(pkgDef{id: "A"})
	ex := newFake(t, map[string][]round{"A": {edit(map[string]string{"a.txt": "a\n"})}})
	ex.onStart = func(pid string, _ spec) {
		// the assembly changes under the integration tree, so the squash cannot apply cleanly
		assembly := mergeAssembly(t, w)
		writeFile(t, filepath.Join(assembly, "a.txt"), "other\n")
		w.git(assembly, "add", "a.txt")
		w.git(assembly, "commit", "-q", "-m", "other")
	}
	rep, code := w.run(ex, func(o *Options) { o.Merge = true })
	eq(t, "exit", code, 1)
	if rep.Merge == nil {
		t.Fatal("no merge in the report")
	}
	eq(t, "merge status", rep.Merge.Status, "conflict")
	needs := w.state().Needs
	if len(needs) != 1 || needs[0].Kind != "merge" || needs[0].ID != nil {
		t.Fatalf("needs %+v, want one wave-level merge need", needs)
	}
	if !strings.HasPrefix(needs[0].Detail, "conflict:") {
		t.Errorf("need detail %q", needs[0].Detail)
	}
	if _, err := os.Stat(w.intPath()); err != nil {
		t.Errorf("integration tree removed: %v", err)
	}
}

func TestMergeOffLeavesMergeNull(t *testing.T) {
	w := newMergeRepo(t)
	w.plan(pkgDef{id: "A"})
	ex := newFake(t, map[string][]round{"A": {edit(map[string]string{"a.txt": "a\n"})}})
	rep, code := w.run(ex)
	eq(t, "exit", code, 0)
	if rep.Merge != nil {
		t.Errorf("merge ran without Options.Merge: %+v", rep.Merge)
	}
	if !strings.Contains(mergeJSON(t, rep), `"merge":null`) {
		t.Errorf("report %s lacks null merge", mergeJSON(t, rep))
	}
	if _, err := os.Stat(w.intPath()); err != nil {
		t.Errorf("integration tree removed without a merge: %v", err)
	}
}

func TestMergeSecondRunFindsNothingToMerge(t *testing.T) {
	w := newMergeRepo(t)
	w.plan(pkgDef{id: "A"})
	ex := newFake(t, map[string][]round{"A": {edit(map[string]string{"a.txt": "a\n"})}})
	_, code := w.run(ex, func(o *Options) { o.Merge = true })
	eq(t, "first exit", code, 0)
	rep, code := w.run(newFake(t, nil), func(o *Options) { o.Merge = true })
	eq(t, "second exit", code, 0)
	eq(t, "merge status", rep.Merge.Status, "empty")
	if !rep.Merge.Removed {
		t.Errorf("integration tree kept after an empty merge")
	}
	if _, err := os.Stat(w.intPath()); !os.IsNotExist(err) {
		t.Errorf("integration tree still present: %v", err)
	}
}

func TestLandAutoLandsOnFeatureBranch(t *testing.T) {
	w := newMergeRepo(t)
	setLandMode(t, w, "auto")
	w.plan(pkgDef{id: "A"})
	withCommitMessage(t, w, "feat: a")
	ex := newFake(t, map[string][]round{"A": {edit(map[string]string{"a.txt": "a\n"})}})
	rep, code := w.run(ex, func(o *Options) { o.Merge = true; o.RunID = "auto-land" })
	eq(t, "exit", code, 0)
	eq(t, "merge status", rep.Merge.Status, "merged")
	if rep.Merge.Land == nil {
		t.Fatal("no land in the report")
	}
	eq(t, "land status", rep.Merge.Land.Status, "landed")
	eq(t, "land branch", rep.Merge.Land.Branch, "feature/auto-land")
	eq(t, "land created", rep.Merge.Land.Created, true)
	eq(t, "checkout", w.git(w.repo, "symbolic-ref", "--short", "HEAD"), "feature/auto-land")
	eq(t, "land events", len(w.events("land")), 1)
}

func TestLandAutoWithoutMessageNeedsLand(t *testing.T) {
	w := newMergeRepo(t)
	setLandMode(t, w, "auto")
	w.plan(pkgDef{id: "A"})
	ex := newFake(t, map[string][]round{"A": {edit(map[string]string{"a.txt": "a\n"})}})
	rep, code := w.run(ex, func(o *Options) { o.Merge = true; o.RunID = "auto-land" })
	eq(t, "exit", code, 1)
	if rep.Merge == nil || rep.Merge.Land == nil {
		t.Fatalf("merge %+v has no land", rep.Merge)
	}
	eq(t, "land status", rep.Merge.Land.Status, "refused")
	needs := w.state().Needs
	if len(needs) != 1 || needs[0].Kind != "land" || needs[0].ID != nil {
		t.Fatalf("needs %+v, want one wave-level land need", needs)
	}
	for _, want := range []string{"no commit message to land with", "agent-exec isolate land --request ", "--feature auto-land"} {
		if !strings.Contains(needs[0].Detail, want) {
			t.Errorf("need detail %q lacks %q", needs[0].Detail, want)
		}
	}
	eq(t, "checkout", w.git(w.repo, "symbolic-ref", "--short", "HEAD"), "main")
	eq(t, "feature branches", w.git(w.repo, "branch", "--list", "feature/*"), "")
	if _, err := os.Stat(mergeAssembly(t, w)); err != nil {
		t.Errorf("assembly removed by a refused land: %v", err)
	}
}

func TestLandAskLeavesCheckout(t *testing.T) {
	w := newMergeRepo(t)
	w.plan(pkgDef{id: "A"})
	withCommitMessage(t, w, "feat: a")
	ex := newFake(t, map[string][]round{"A": {edit(map[string]string{"a.txt": "a\n"})}})
	rep, code := w.run(ex, func(o *Options) { o.Merge = true; o.RunID = "auto-land" })
	eq(t, "exit", code, 0)
	if rep.Merge == nil || rep.Merge.Land == nil {
		t.Fatalf("merge %+v has no land", rep.Merge)
	}
	eq(t, "land status", rep.Merge.Land.Status, "ask")
	eq(t, "needs", len(rep.Needs), 0)
	eq(t, "checkout", w.git(w.repo, "symbolic-ref", "--short", "HEAD"), "main")
	eq(t, "feature branches", w.git(w.repo, "branch", "--list", "feature/*"), "")
}
