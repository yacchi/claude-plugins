package watch

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/approvals"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/slot"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/sysload"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/state"
)

func TestUntilChangeAnswersOnlyForEventsAfterSince(t *testing.T) {
	store := newStore(t, "a", "b")
	liveRunner(t, store.Path)
	if err := store.SetStatus("a", "implementing", "", nil); err != nil {
		t.Fatal(err)
	}
	seq := lines(t, store.EventsPath())
	wait := func(since int, d time.Duration) Answer {
		return Wait(WaitOptions{State: store.Path, Since: since, MaxWait: d, Poll: 10 * time.Millisecond, UntilChange: true})
	}
	if a := wait(seq, 200*time.Millisecond); a.Status != "running" || a.Seq != seq {
		t.Fatalf("a change present at --since is not new: %s seq=%d (want running seq=%d)", a.Status, a.Seq, seq)
	}
	if a := wait(0, 200*time.Millisecond); a.Status != "changed" {
		t.Fatalf("since 0: the status event is new, got %s", a.Status)
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = store.SetStatus("b", "implementing", "", nil)
	}()
	if a := wait(seq, 5*time.Second); a.Status != "changed" || a.Seq != lines(t, store.EventsPath()) {
		t.Fatalf("a status change after --since: %s seq=%d", a.Status, a.Seq)
	}

	// Without --until-change the same event does not end the wait.
	a := Wait(WaitOptions{State: store.Path, Since: seq, MaxWait: 200 * time.Millisecond, Poll: 10 * time.Millisecond})
	if a.Status != "running" {
		t.Fatalf("without --until-change: got %s", a.Status)
	}
}

func TestUntilChangeANewNeedStillAnswersNeeds(t *testing.T) {
	store := newStore(t, "a")
	liveRunner(t, store.Path)
	seq := lines(t, store.EventsPath())
	if err := store.AddNeed("a", "escalate", "help"); err != nil {
		t.Fatal(err)
	}
	a := Wait(WaitOptions{State: store.Path, Since: seq, MaxWait: time.Second, Poll: 10 * time.Millisecond, UntilChange: true})
	if a.Status != "needs" {
		t.Fatalf("a new need with --until-change: got %s, want needs", a.Status)
	}
}

func TestApprovalsLeaveOutDecidedItems(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", filepath.Join(tmp, "home"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "xs"))
	store := newStore(t, "A")
	err := store.UpdatePackage("A", func(p *state.Package) {
		p.NetworkDenials = []any{
			map[string]any{"host": "decided.example.com", "port": int64(443)},
			map[string]any{"host": "open.example.com", "port": int64(443)},
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	decided := `{"allowed": {}, "declined": {"decided.example.com": {"reason": "no"}}}`
	if err := os.WriteFile(filepath.Join(filepath.Dir(store.Path), approvals.ApprovalsName), []byte(decided), 0o644); err != nil {
		t.Fatal(err)
	}
	items, err := approvals.PendingIn(store.Path, &config.Resolved{Values: config.Defaults()})
	if err != nil {
		t.Fatal(err)
	}
	got := ApprovalsOf(items)
	if len(got) != 1 || got[0].ID != "net:open.example.com" || got[0].Kind != "network" || got[0].Pkg != nil {
		t.Fatalf("approvals = %+v, want only the undecided network item", got)
	}
	if got[0].Detail == "" || len(got[0].Detail) > approvalDetailCh {
		t.Fatalf("detail = %q", got[0].Detail)
	}
}

func TestMachineFieldsDegradeAlone(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ORCHESTRA_SLOT_DIR", dir)
	// A pool dir that is a file: its leases cannot be read.
	broken := filepath.Join(dir, "broken")
	if err := os.WriteFile(broken, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	p := probes{
		load:     func() (float64, float64, bool) { return 0, 0, false },
		pressure: func() string { return sysload.Warn },
		gitProcs: func() int { return 3 },
	}
	m := p.machine(slot.Pool{Name: "heavy", Dir: broken, Size: 4}, slot.New(slot.Test, 2))
	if m.Load1 != -1 || m.Load5 != -1 {
		t.Fatalf("unreadable load: %v %v", m.Load1, m.Load5)
	}
	if m.MemPressure != sysload.Warn || m.GitProcs != 3 || m.CPUs <= 0 {
		t.Fatalf("other probes must still answer: %+v", m)
	}
	if m.Heavy != (PoolUse{Used: -1, Size: 4}) || m.Test != (PoolUse{Used: 0, Size: 2}) {
		t.Fatalf("pools: heavy %+v test %+v", m.Heavy, m.Test)
	}
}
