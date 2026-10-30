package planning

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/dispatch"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
)

// reviewTestConfig is the frozen planning configuration, built by hand so a
// change to config.Defaults() cannot move these tests.
func reviewTestConfig(plannerEffort string) *config.Resolved {
	return &config.Resolved{Values: map[string]any{
		"tiers": map[string]any{"planner": map[string]any{"model": "opus", "effort": plannerEffort}},
		"planning": map[string]any{
			"redo": map[string]any{"model": "opus", "effort": "max"},
			"spec_writers": map[string]any{
				"light":    map[string]any{"model": "sonnet", "effort": "medium"},
				"standard": map[string]any{"model": "opus", "effort": "high"},
				"deep":     map[string]any{"model": "opus", "effort": "max"},
			},
			"writers_concurrent": int64(8),
			"review":             true,
			"redo_when": map[string]any{
				"structural_findings": int64(1), "deep_packages": int64(3), "interface_users": int64(5),
			},
		},
	}}
}

// reviewTestPlan writes a two-package skeleton with a preamble and returns
// the plan path.
func reviewTestPlan(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "preamble.md"), []byte("# Preamble\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	skeleton := `{"preamble": ["preamble.md"], "packages": [
  {"id": "a", "spec": "specs/a.md", "cls": "standard", "files_owned": ["a.go"], "est_seconds": 120,
   "verification": "go test ./a/...", "brief": "do a", "risks": []},
  {"id": "b", "spec": "specs/b.md", "cls": "light", "depends_on": ["a"], "files_owned": ["b.go"], "est_seconds": 60,
   "verification": "go test ./b/...", "brief": "do b", "risks": []}
]}`
	planPath := filepath.Join(dir, "plan.json")
	if err := os.WriteFile(planPath, []byte(skeleton), 0o644); err != nil {
		t.Fatal(err)
	}
	return planPath
}

// reviewFakes replaces the dispatch seams; wait answers every wait. It
// returns a counter of started reviews and the removed task names.
func reviewFakes(t *testing.T, wait func(token string) map[string]any) (*int, *[]string) {
	t.Helper()
	oldStart, oldWait, oldRemove := startReview, waitReview, removeReviewTree
	t.Cleanup(func() { startReview, waitReview, removeReviewTree = oldStart, oldWait, oldRemove })
	starts, removed := 0, []string{}
	startReview = func(_ *config.Resolved, spec dispatch.PrepareSpec, req dispatch.Request) (string, error) {
		starts++
		if req.Task == "" || spec.Task == nil || *spec.Task != req.Task {
			t.Errorf("review started without its task: spec %+v, req %+v", spec, req)
		}
		return "dsp-000000000001", nil
	}
	waitReview = func(_ *config.Resolved, token string, _ time.Duration) map[string]any { return wait(token) }
	removeReviewTree = func(_, task string) { removed = append(removed, task) }
	return &starts, &removed
}

func reviewOK(answer string) func(string) map[string]any {
	return func(string) map[string]any {
		return map[string]any{"status": "ok", "answer": answer, "executor": "pi", "model": "gpt-6.1-sol"}
	}
}

func TestRunReviewWaitsOutsideTheLockAndAttachesToTheRunningReview(t *testing.T) {
	planPath := reviewTestPlan(t)
	cfg := reviewTestConfig("xhigh")
	var tokens []string
	starts, _ := reviewFakes(t, func(token string) map[string]any {
		tokens = append(tokens, token)
		unlock, err := fsutil.Lock(StatePath(planPath)+".lock", fsutil.Exclusive, 200*time.Millisecond)
		if err != nil {
			t.Errorf("the state lock is held while waiting: %v", err)
		} else {
			_ = unlock()
		}
		return map[string]any{"status": "running", "token": token, "waited_s": int64(0)}
	})
	now := time.Unix(1_000_000, 0)

	for i := range 2 {
		out, err := RunReview(cfg, planPath, ReviewOptions{Repo: t.TempDir()}, now)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if out["status"] != "running" || out["token"] != "dsp-000000000001" || out["round"] != 1 {
			t.Fatalf("call %d printed %v, want running round 1 on the started token", i, out)
		}
	}
	if *starts != 1 || len(tokens) != 2 || tokens[1] != "dsp-000000000001" {
		t.Fatalf("starts = %d, waited on %v; want one dispatch, both calls attached to it", *starts, tokens)
	}

	// Past the timeout a still-running review no longer blocks planning.
	out, err := RunReview(cfg, planPath, ReviewOptions{}, now.Add(ReviewTimeout))
	if err != nil || out["status"] != "skipped" {
		t.Fatalf("after the timeout: %v, %v; want skipped", out, err)
	}
}

func TestRunReviewParsesProseAndNormalizesSeverity(t *testing.T) {
	planPath := reviewTestPlan(t)
	answer := `Here you go: {"findings": [
  {"severity": "BLOCKER", "kind": "split", "packages": ["a"], "detail": "unknown severity"},
  {"severity": "Structural", "kind": "weird", "packages": ["a", 3], "evidence": "a.go:1", "detail": "real", "fix": "split a"},
  {"severity": "structural", "detail": ""}
]} Hope this helps.`
	_, removed := reviewFakes(t, reviewOK(answer))

	out, err := RunReview(reviewTestConfig("xhigh"), planPath, ReviewOptions{Repo: t.TempDir()}, time.Unix(1_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if out["status"] != "done" || out["structural"] != 1 || out["findings"] != 2 || out["decision"] != "redo" {
		t.Fatalf("printed %v, want done, 1 structural of 2 findings, redo", out)
	}
	var file reviewFile
	if err := fsutil.ReadJSONFile(ReviewPath(planPath), &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Findings) != 2 || file.Findings[0].Severity != "minor" || file.Findings[1].Kind != "other" ||
		len(file.Findings[1].Packages) != 1 || file.Executor != "pi" {
		t.Fatalf("review.json = %+v", file)
	}
	if len(*removed) != 1 {
		t.Fatalf("removed trees %v, want the review's task", *removed)
	}

	// The finished round prints the same object again without a new dispatch.
	again, err := RunReview(reviewTestConfig("xhigh"), planPath, ReviewOptions{}, time.Unix(1_000_100, 0))
	if err != nil || again["decision"] != "redo" || again["findings"] != 2 {
		t.Fatalf("second call: %v, %v", again, err)
	}

	_, err = ResolveReview(reviewTestConfig("xhigh"), planPath)
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("ResolveReview of a redo decision: %v, want *RefusedError", err)
	}
}

func TestRunReviewNoRedoWhenNotPossible(t *testing.T) {
	structural := `{"findings": [{"severity": "structural", "kind": "split", "detail": "too big"}]}`
	cases := []struct {
		name          string
		round         int
		plannerEffort string
	}{
		{"round 2", 2, "xhigh"},
		{"round tier is already the redo tier", 0, "max"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			planPath := reviewTestPlan(t)
			if tc.round > 0 {
				st, _ := Load(planPath)
				st.Round, st.Model, st.Effort = tc.round, "opus", "max"
				if err := Save(planPath, st); err != nil {
					t.Fatal(err)
				}
			}
			reviewFakes(t, reviewOK(structural))

			cfg := reviewTestConfig(tc.plannerEffort)
			out, err := RunReview(cfg, planPath, ReviewOptions{Repo: t.TempDir()}, time.Unix(1_000_000, 0))
			if err != nil {
				t.Fatal(err)
			}
			if out["decision"] != "accept" || out["structural"] != 1 {
				t.Fatalf("printed %v, want accept with the structural finding still counted", out)
			}
			if _, err := ResolveReview(cfg, planPath); err != nil {
				t.Fatalf("ResolveReview of an accept: %v", err)
			}
		})
	}
}

func TestRunReviewSkipsBadAnswers(t *testing.T) {
	cases := map[string]map[string]any{
		"prose only":      {"status": "ok", "answer": "Looks fine to me."},
		"no findings key": {"status": "ok", "answer": `{"verdict": "ok"}`},
		"unavailable":     {"status": "unavailable", "reason": "pi is logged out"},
		"relay failure":   {"status": "relay-failed", "raw": "boom"},
	}
	for name, res := range cases {
		t.Run(name, func(t *testing.T) {
			planPath := reviewTestPlan(t)
			reviewFakes(t, func(string) map[string]any { return res })

			out, err := RunReview(reviewTestConfig("xhigh"), planPath, ReviewOptions{Repo: t.TempDir()}, time.Unix(1_000_000, 0))
			if err != nil {
				t.Fatal(err)
			}
			if out["status"] != "skipped" || out["decision"] != "accept" || out["note"] == "" {
				t.Fatalf("printed %v, want skipped/accept with a note", out)
			}
		})
	}
}
