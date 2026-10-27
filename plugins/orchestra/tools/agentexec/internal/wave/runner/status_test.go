package runner

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/statusfile"
)

// showsPhase reports whether run lists pkg in phase.
func showsPhase(run statusfile.Run, pkg, phase string) bool {
	for _, p := range run.Packages {
		if p.ID == pkg && p.Phase == phase {
			return true
		}
	}
	return false
}

func TestStatusEntryFollowsTheRun(t *testing.T) {
	defer func(d time.Duration) { statusPoll = d }(statusPoll)
	statusPoll = 10 * time.Millisecond
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A"})
	store := statusfile.Default()
	key := statusfile.RunKey(w.statePath)
	ex := newFake(t, map[string][]round{"A": {edit(map[string]string{"a.txt": "a\n"})}})
	sawLive := false
	ex.onStart = func(string, spec) {
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if run, ok := store.ReadRun(key); ok && run.Status == "running" && showsPhase(run, "A", "implementing") {
				sawLive = true
				return
			}
		}
	}
	_, code := w.run(ex, func(o *Options) { o.RunID = "rid"; o.Title = "My title" })
	if !sawLive {
		t.Error("the live entry never showed A implementing")
	}
	eq(t, "exit", code, 0)

	run, ok := store.ReadRun(key)
	if !ok {
		t.Fatal("no run file")
	}
	eq(t, "status", run.Status, "done")
	if run.EndedAt == nil {
		t.Error("EndedAt not set")
	}
	eq(t, "title", run.Title, "My title")
	if run.RunID == nil || *run.RunID != "rid" {
		t.Errorf("run id %v", run.RunID)
	}
	eq(t, "integrated", run.Counts.Integrated, 1)
	eq(t, "pid", run.PID, os.Getpid())

	var doc statusfile.Doc
	if err := fsutil.ReadJSONFile(store.Path(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Runs) != 1 || doc.Runs[0].Key != key {
		t.Errorf("status.json runs %+v", doc.Runs)
	}
	eq(t, "busy", doc.Summary.Busy, false)
}

func TestStatusEntryRecordsTheAccount(t *testing.T) {
	defer func(d time.Duration) { statusPoll = d }(statusPoll)
	statusPoll = 10 * time.Millisecond
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A"})
	writeFile(t, filepath.Join(w.home, ".claude", ".claude.json"), `{"oauthAccount":{"organizationName":"Acme","emailAddress":"a@example.com","organizationUuid":"00000000-0000-0000-0000-000000000000"}}`)
	ex := newFake(t, map[string][]round{"A": {edit(map[string]string{"a.txt": "a\n"})}})
	_, code := w.run(ex)
	eq(t, "exit", code, 0)

	store := statusfile.Default()
	run, ok := store.ReadRun(statusfile.RunKey(w.statePath))
	if !ok {
		t.Fatal("no run file")
	}
	if run.Account == nil || *run.Account != "Acme" {
		t.Errorf("account %v, want Acme", run.Account)
	}

	var doc statusfile.Doc
	if err := fsutil.ReadJSONFile(store.Path(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Runs) != 1 || doc.Runs[0].Account == nil || *doc.Runs[0].Account != "Acme" {
		t.Errorf("status.json account %+v, want Acme", doc.Runs)
	}
}

func TestStatusEntryEndsNeedsWhenANeedIsOpen(t *testing.T) {
	w := newWaveRepo(t)
	w.plan(pkgDef{id: "A"})
	ex := newFake(t, map[string][]round{"A": {edit(map[string]string{"a.txt": "a\n"}, editOpt{answer: "\n  ESCALATE: spec unclear\n"})}})
	_, code := w.run(ex)
	eq(t, "exit", code, 1)

	key := statusfile.RunKey(w.statePath)
	run, ok := statusfile.Default().ReadRun(key)
	if !ok {
		t.Fatal("no run file")
	}
	eq(t, "status", run.Status, "needs")
	if len(run.Needs) != 1 {
		t.Fatalf("needs %+v", run.Needs)
	}
	eq(t, "need kind", run.Needs[0].Kind, "escalate")
	eq(t, "title defaults to the run key", run.Title, key)
}
