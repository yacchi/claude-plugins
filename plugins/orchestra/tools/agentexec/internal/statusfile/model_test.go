package statusfile

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func onlyPid1(pid int) bool { return pid == 1 }

func ptr[T any](v T) *T { return &v }

func TestBuildExample(t *testing.T) {
	const now = 100000
	runs := []Run{
		{Key: "a", Status: "running", PID: 1, UpdatedAt: 99990},
		{Key: "b", Status: "needs", PID: 1, UpdatedAt: 99995},
		{Key: "c", Status: "running", PID: 2, UpdatedAt: 99000},
		{Key: "d", Status: "done", EndedAt: ptr(int64(90000)), UpdatedAt: 90000},
		{Key: "e", Status: "stopped", UpdatedAt: 70000},
	}
	plans := []Plan{{PlanID: "p", Status: "planning", UpdatedAt: 99000}}

	doc := Build(runs, plans, now, onlyPid1)

	wantStatus := map[string]string{"a": "running", "b": "needs", "c": "crashed", "d": "done"}
	var gotKeys []string
	for _, r := range doc.Runs {
		gotKeys = append(gotKeys, r.Key)
		if r.Status != wantStatus[r.Key] {
			t.Errorf("run %s: status = %q, want %q", r.Key, r.Status, wantStatus[r.Key])
		}
	}
	if want := []string{"b", "a", "c", "d"}; !reflect.DeepEqual(gotKeys, want) {
		t.Errorf("run order = %v, want %v", gotKeys, want)
	}
	if len(doc.Plans) != 1 || doc.Plans[0].Status != "planning" {
		t.Errorf("plans = %+v, want one planning plan", doc.Plans)
	}

	want := Summary{
		Busy:      true,
		Running:   2,
		Planning:  1,
		Needs:     1,
		Crashed:   1,
		UpdatedAt: ptr(int64(99995)),
		Line:      "2 running · 1 planning · 1 needs · 1 crashed",
	}
	if !reflect.DeepEqual(doc.Summary, want) {
		t.Errorf("summary = %+v, want %+v", doc.Summary, want)
	}
	if doc.Version != Version || doc.GeneratedAt != now || doc.HeartbeatS != HeartbeatSeconds || doc.StaleAfterS != StaleAfterSeconds {
		t.Errorf("header = %+v", doc)
	}
}

func TestBuildLiveRunPastStaleBound(t *testing.T) {
	const now = 100000
	runs := []Run{{Key: "a", Status: "running", PID: 1, UpdatedAt: now - (StaleAfterSeconds + 1)}}

	doc := Build(runs, nil, now, onlyPid1)

	if len(doc.Runs) != 1 || doc.Runs[0].Status != "stale" {
		t.Fatalf("runs = %+v, want one stale run", doc.Runs)
	}
	if doc.Summary.Running != 0 || doc.Summary.Crashed != 1 || doc.Summary.Busy {
		t.Errorf("summary = %+v, want crashed 1, running 0, not busy", doc.Summary)
	}
}

func TestBuildEndedNeedsAndStalePlan(t *testing.T) {
	const now = 100000
	runs := []Run{{Key: "a", Status: "needs", EndedAt: ptr(int64(99000)), UpdatedAt: 99000}}
	plans := []Plan{
		{PlanID: "p1", Status: "needs-user", UpdatedAt: 99000},
		{PlanID: "p2", Status: "planning", UpdatedAt: now - (PlanStaleSeconds + 1)},
	}

	doc := Build(runs, plans, now, onlyPid1)

	if len(doc.Runs) != 1 || doc.Runs[0].Status != "needs" {
		t.Errorf("runs = %+v, want one needs run", doc.Runs)
	}
	if doc.Plans[1].PlanID != "p2" || doc.Plans[1].Status != "stale" {
		t.Errorf("plans = %+v, want p2 stale", doc.Plans)
	}
	s := doc.Summary
	if s.Running != 0 || s.Needs != 2 || s.Planning != 0 || s.Busy {
		t.Errorf("summary = %+v, want needs 2, nothing running or planning", s)
	}
}

func TestSummaryOmitsIdentifiers(t *testing.T) {
	const now = 100000
	runs := []Run{{
		Key:       "secret-key",
		RunID:     ptr("secret-run"),
		Project:   "acme/secret-repo",
		Title:     "Secret Title",
		Status:    "needs",
		State:     "/Users/x/secret/state.json",
		PID:       1,
		UpdatedAt: now,
		Packages:  []RunPackage{{ID: "pkg-secret"}},
		Needs:     []RunNeed{{ID: ptr("pkg-secret"), Kind: "login", Detail: "secret detail"}},
	}}
	plans := []Plan{{PlanID: "secret-plan", Project: "acme/secret-repo", Status: "needs-user", UpdatedAt: now}}

	doc := Build(runs, plans, now, onlyPid1)
	b, err := json.Marshal(doc.Summary)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"secret", "Secret", "acme", "/Users"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("summary JSON %s contains %q", b, bad)
		}
	}
}

func TestBuildEmpty(t *testing.T) {
	doc := Build(nil, nil, 100000, onlyPid1)

	if doc.Summary.Line != "idle" || doc.Summary.UpdatedAt != nil || doc.Summary.Busy {
		t.Errorf("summary = %+v, want idle with nil updated_at", doc.Summary)
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"runs":[]`, `"plans":[]`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("marshalled doc %s lacks %s", b, want)
		}
	}
}

func TestBuildDoesNotMutateInputs(t *testing.T) {
	runs := []Run{{Key: "a", Status: "running", PID: 2, UpdatedAt: 99000}}
	plans := []Plan{{PlanID: "p", Status: "planning", UpdatedAt: 1}}

	Build(runs, plans, 100000, onlyPid1)

	if runs[0].Status != "running" || runs[0].Packages != nil || runs[0].Needs != nil {
		t.Errorf("runs mutated: %+v", runs[0])
	}
	if plans[0].Status != "planning" {
		t.Errorf("plans mutated: %+v", plans[0])
	}
}
