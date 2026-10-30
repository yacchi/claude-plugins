package planning

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
)

var nextNow = time.Unix(1_800_000_000, 0)

func nextTestConfig(concurrent int) *config.Resolved {
	tier := func(model, effort string) map[string]any { return map[string]any{"model": model, "effort": effort} }
	return &config.Resolved{Values: map[string]any{
		"tiers": map[string]any{"planner": tier("opus", "xhigh")},
		"planning": map[string]any{
			"redo": tier("opus", "max"),
			"spec_writers": map[string]any{
				"light":    tier("sonnet", "medium"),
				"standard": tier("opus", "high"),
				"deep":     tier("opus", "max"),
			},
			"writers_concurrent": int64(concurrent),
			"review":             true,
			"redo_when": map[string]any{
				"structural_findings": int64(1),
				"deep_packages":       int64(3),
				"interface_users":     int64(5),
			},
		},
	}}
}

// nextTestPlan writes a skeleton with one light package per id, all using
// interface "i", and returns the plan path.
func nextTestPlan(t *testing.T, ids ...string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "specs"), 0o755); err != nil {
		t.Fatal(err)
	}
	nextWrite(t, filepath.Join(dir, "i.txt"), "func I() int")
	pkgs := make([]string, 0, len(ids))
	for _, id := range ids {
		pkgs = append(pkgs, fmt.Sprintf(`{"id":%q,"spec":"specs/%s.md","cls":"light","brief":"b","risks":[],`+
			`"est_seconds":60,"files_owned":["%s.go"],"verification":"go test ./%s/...","interfaces":["i"]}`, id, id, id, id))
	}
	planPath := filepath.Join(dir, "plan.json")
	nextWrite(t, planPath, `{"interfaces":[{"id":"i","path":"i.txt"}],"packages":[`+strings.Join(pkgs, ",")+`]}`)
	return planPath
}

// nextReviewed saves a state whose round-1 skeleton was reviewed with decision.
func nextReviewed(t *testing.T, planPath string, round int, decision string) {
	t.Helper()
	st, err := Load(planPath)
	if err != nil {
		t.Fatal(err)
	}
	st.Round, st.Model, st.Effort, st.SkeletonSteps = round, "opus", "xhigh", 1
	st.Review = &Review{Round: round, Status: "done", Decision: decision, Reasons: []string{"too many deep packages"}, Resolved: true}
	if err := Save(planPath, st); err != nil {
		t.Fatal(err)
	}
}

func nextWrite(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func nextSpec(t *testing.T, planPath, id string) {
	t.Helper()
	nextWrite(t, filepath.Join(filepath.Dir(planPath), "specs", id+".md"), "# "+id+"\n\nfunc I() int\n")
}

func nextRun(t *testing.T, cfg *config.Resolved, planPath string, in NextInput, now time.Time) (Step, State) {
	t.Helper()
	step, st, err := Next(cfg, planPath, in, now)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	return step, st
}

func nextWriterIDs(step Step) []string {
	ids := []string{}
	for _, w := range step.Writers {
		ids = append(ids, w.ID)
	}
	return ids
}

func TestNextInFlightWritersAreNotReissuedUntilTimeout(t *testing.T) {
	cfg := nextTestConfig(8)
	planPath := nextTestPlan(t, "a", "b")
	nextReviewed(t, planPath, 1, "accept")

	step, _ := nextRun(t, cfg, planPath, NextInput{}, nextNow)
	if step.Step != "specs" || !slices.Equal(nextWriterIDs(step), []string{"a", "b"}) {
		t.Fatalf("first call = %s %v, want specs [a b]", step.Step, nextWriterIDs(step))
	}
	if w := step.Writers[0]; w.Model != "sonnet" || w.Effort != "medium" {
		t.Errorf("light writer at %s/%s, want sonnet/medium", w.Model, w.Effort)
	}

	step, _ = nextRun(t, cfg, planPath, NextInput{}, nextNow.Add(time.Minute))
	if step.Step != "wait" || len(step.Writers) != 0 {
		t.Fatalf("second call = %s %v, want wait with no writers", step.Step, nextWriterIDs(step))
	}

	step, st := nextRun(t, cfg, planPath, NextInput{}, nextNow.Add(WriterTimeout+time.Second))
	if step.Step != "specs" || !slices.Equal(nextWriterIDs(step), []string{"a", "b"}) {
		t.Fatalf("after WriterTimeout = %s %v, want specs [a b]", step.Step, nextWriterIDs(step))
	}
	if st.Specs["a"].Attempts != 2 {
		t.Errorf("attempts after timeout = %d, want 2", st.Specs["a"].Attempts)
	}
}

func TestNextWritersConcurrentCountsOnlyRunningWriters(t *testing.T) {
	cfg := nextTestConfig(2)
	planPath := nextTestPlan(t, "a", "b", "c")
	nextReviewed(t, planPath, 1, "accept")

	step, _ := nextRun(t, cfg, planPath, NextInput{}, nextNow)
	if !slices.Equal(nextWriterIDs(step), []string{"a", "b"}) {
		t.Fatalf("first call writers = %v, want [a b] under a cap of 2", nextWriterIDs(step))
	}

	nextSpec(t, planPath, "a")
	step, _ = nextRun(t, cfg, planPath, NextInput{Returned: []string{"a"}}, nextNow.Add(time.Minute))
	if step.Step != "specs" || !slices.Equal(nextWriterIDs(step), []string{"c"}) {
		t.Fatalf("after a returned = %s %v, want specs [c]", step.Step, nextWriterIDs(step))
	}
}

func TestNextStaleHashInvalidatesSpec(t *testing.T) {
	cfg := nextTestConfig(8)
	planPath := nextTestPlan(t, "a", "b")
	nextReviewed(t, planPath, 1, "accept")
	nextRun(t, cfg, planPath, NextInput{}, nextNow)
	nextSpec(t, planPath, "a")
	nextSpec(t, planPath, "b")

	step, _ := nextRun(t, cfg, planPath, NextInput{Returned: []string{"a", "b"}}, nextNow.Add(time.Minute))
	if step.Step != "done" {
		t.Fatalf("all specs written = %s %v, want done", step.Step, step.Reasons)
	}

	// A changed interface file invalidates its users even though each spec
	// already pastes the new text verbatim.
	nextWrite(t, filepath.Join(filepath.Dir(planPath), "i.txt"), "func J() int")
	for _, id := range []string{"a", "b"} {
		nextWrite(t, filepath.Join(filepath.Dir(planPath), "specs", id+".md"), "func J() int\n")
	}
	step, st := nextRun(t, cfg, planPath, NextInput{}, nextNow.Add(2*time.Minute))
	if step.Step != "specs" || !slices.Equal(nextWriterIDs(step), []string{"a", "b"}) {
		t.Fatalf("after interface change = %s %v, want specs [a b]", step.Step, nextWriterIDs(step))
	}
	if !strings.Contains(step.Writers[0].Prompt, "\nFix: the package changed since its spec was written") {
		t.Errorf("prompt %q lacks the changed-package reason", step.Writers[0].Prompt)
	}
	if st.Specs["a"].Attempts != 1 {
		t.Errorf("attempts after hash change = %d, want restart at 1", st.Specs["a"].Attempts)
	}
}

func TestNextRedoOnlyFromRoundOne(t *testing.T) {
	cfg := nextTestConfig(8)
	planPath := nextTestPlan(t, "a")
	nextReviewed(t, planPath, 1, "redo")

	step, _ := nextRun(t, cfg, planPath, NextInput{}, nextNow)
	if step.Step != "skeleton" || step.Round != 2 || step.Model != "opus" || step.Effort != "max" {
		t.Fatalf("round-1 redo = %+v, want skeleton round 2 at opus/max", step)
	}
	if !slices.Equal(step.Reasons, []string{"too many deep packages"}) {
		t.Errorf("redo reasons = %v, want the review's", step.Reasons)
	}

	nextReviewed(t, planPath, 2, "redo")
	step, _ = nextRun(t, cfg, planPath, NextInput{}, nextNow)
	if step.Step != "specs" {
		t.Fatalf("round-2 redo decision = %s round %d, want specs (no second redo)", step.Step, step.Round)
	}
}

func TestNextEscalationsRenamedAndCapped(t *testing.T) {
	cfg := nextTestConfig(8)
	planPath := nextTestPlan(t, "a")
	nextReviewed(t, planPath, 1, "accept")
	esc := filepath.Join(filepath.Dir(planPath), "specs", "a.escalate.md")

	for rev := 1; rev <= MaxRevisions; rev++ {
		nextWrite(t, esc, "split it")
		step, st := nextRun(t, cfg, planPath, NextInput{Returned: []string{"a"}}, nextNow)
		renamed := filepath.Join(filepath.Dir(planPath), "specs", fmt.Sprintf("a.escalated-%d.md", rev))
		if step.Step != "skeleton" || !slices.Equal(step.Reasons, []string{"a: " + renamed}) || st.Revisions != rev {
			t.Fatalf("revision %d = %s %v (revisions %d), want skeleton [a: %s]", rev, step.Step, step.Reasons, st.Revisions, renamed)
		}
		step, _ = nextRun(t, cfg, planPath, NextInput{}, nextNow)
		if step.Step != "specs" {
			t.Fatalf("after revision %d = %s %v, want specs (escalation not re-processed)", rev, step.Step, step.Reasons)
		}
	}

	nextWrite(t, esc, "split it")
	step, _ := nextRun(t, cfg, planPath, NextInput{Returned: []string{"a"}}, nextNow)
	if step.Step != "stuck" || !slices.Equal(step.Reasons, []string{"a: escalated again: " + esc}) {
		t.Fatalf("past MaxRevisions = %s %v, want stuck", step.Step, step.Reasons)
	}
}

func TestNextAnswersResetCurrentReview(t *testing.T) {
	cfg := nextTestConfig(8)
	planPath := nextTestPlan(t, "a")
	nextReviewed(t, planPath, 1, "accept")

	step, st := nextRun(t, cfg, planPath, NextInput{Answers: "answers.md"}, nextNow)
	if step.Step != "skeleton" || st.Review != nil {
		t.Fatalf("with answers = %s, review %+v; want skeleton and no review", step.Step, st.Review)
	}
	if !strings.HasPrefix(step.Note, "The user answered your open questions in /") {
		t.Errorf("answers note = %q", step.Note)
	}

	step, _ = nextRun(t, cfg, planPath, NextInput{}, nextNow)
	if step.Step != "skeleton" || !slices.Equal(step.Reasons, []string{"the round-1 skeleton has not been reviewed"}) {
		t.Fatalf("after answers = %s %v, want skeleton awaiting review", step.Step, step.Reasons)
	}
}

func TestNextContinueSkeletonStopsAtMax(t *testing.T) {
	cfg := nextTestConfig(8)
	planPath := filepath.Join(t.TempDir(), "plan.json")

	for i := 1; i <= MaxSkeletonSteps; i++ {
		step, st := nextRun(t, cfg, planPath, NextInput{}, nextNow)
		if step.Step != "skeleton" || st.SkeletonSteps != i {
			t.Fatalf("call %d = %s (steps %d), want skeleton", i, step.Step, st.SkeletonSteps)
		}
	}
	step, _ := nextRun(t, cfg, planPath, NextInput{}, nextNow)
	if step.Step != "stuck" || len(step.Reasons) != 1 ||
		!strings.HasPrefix(step.Reasons[0], fmt.Sprintf("the round-1 skeleton is still unfinished after %d planner runs: ", MaxSkeletonSteps)) {
		t.Fatalf("past MaxSkeletonSteps = %s %v, want stuck", step.Step, step.Reasons)
	}
}
