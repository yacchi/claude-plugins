package runner

import (
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
)

// An expired Claude login is a `login` need, not a delegate one: the session
// is closed, no new start runs while it is open, and a `wave mark` back to
// pending releases the hold so the package runs again in a fresh session.
func TestLoginNeedHoldsNewStartsUntilMarkedBack(t *testing.T) {
	w := newWaveRepo(t)
	w.verifyMode = "final"
	w.plan(pkgDef{id: "A"}, pkgDef{id: "B"})
	defer func(prev time.Duration) { memoryRecheck = prev }(memoryRecheck)
	memoryRecheck = 20 * time.Millisecond

	h := newFakeHost("herdr")
	h.work = func(pid, tree string) { writeFile(t, filepath.Join(tree, pid+".txt"), "done\n") }
	var broken atomic.Bool
	broken.Store(true)
	h.wait = func(pid string) (map[string]any, error) {
		if pid == "A" && broken.Load() {
			return nil, host.LoginNeed("t-A", "  ⎿  Not logged in · Please run /login")
		}
		return map[string]any{"status": "done", "summary": "ok"}, nil
	}
	ex := newFake(t, map[string][]round{
		"A": {result(delegated, map[string]string{}), result(delegated, map[string]string{})},
		"B": {result(delegated, map[string]string{})},
	})
	type outcome struct {
		rep  Report
		code int
	}
	done := make(chan outcome, 1)
	go func() {
		rep, code := w.runWithHost(ex, Deps{Host: h}, func(o *Options) { o.MaxInFlight = 1 })
		done <- outcome{rep, code}
	}()

	deadline := time.Now().Add(10 * time.Second)
	held := false
	for !held {
		if time.Now().After(deadline) {
			t.Fatal("no start-held event for the login need")
		}
		for _, d := range w.details("start-held", "") {
			reason, _ := d["reason"].(string)
			held = held || (strings.Contains(reason, "/login") && strings.Contains(reason, "--pkg A --status pending"))
		}
		time.Sleep(10 * time.Millisecond)
	}
	eqJSON(t, "needs while held", w.needKinds(), []kindOf{{"A", "login"}})
	detail := w.state().Needs[0].Detail
	if !strings.Contains(detail, "is not logged in") ||
		!strings.Contains(detail, "agent-exec wave mark --state "+w.statePath+" --pkg A --status pending") {
		t.Errorf("need detail %q", detail)
	}
	h.mu.Lock()
	closed, prompts := slices.Clone(h.closed), slices.Clone(h.prompts)
	h.mu.Unlock()
	if !slices.Contains(closed, "A") {
		t.Errorf("A's session was not closed: %v", closed)
	}
	if slices.Contains(prompts, "B") {
		t.Errorf("B started while the login need was open: %v", prompts)
	}

	broken.Store(false)
	if _, code := Mark(MarkOptions{State: w.statePath, Pkg: "A", Status: "pending"}); code != 0 {
		t.Fatalf("mark exit %d", code)
	}
	out := <-done
	eq(t, "exit", out.code, 0)
	eq(t, "integrated", len(out.rep.Integrated), 2)
	if !slices.Contains(out.rep.Integrated, "A") || !slices.Contains(out.rep.Integrated, "B") {
		t.Errorf("integrated %v, want A and B", out.rep.Integrated)
	}
	resumedAt, heldAt := -1, -1
	for i, ev := range w.events("") {
		switch {
		case ev.Event == "start-held" && heldAt < 0:
			heldAt = i
		case ev.Event == "start-resumed" && heldAt >= 0:
			resumedAt = i
		}
	}
	if resumedAt < 0 {
		t.Errorf("no start-resumed event after start-held (held at %d)", heldAt)
	}
}
