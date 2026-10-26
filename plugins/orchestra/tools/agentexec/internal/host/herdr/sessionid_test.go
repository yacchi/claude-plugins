package herdr

import (
	"context"
	"regexp"
	"slices"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/transcript"
)

var sessionIDUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestSessionIDFreshStartIsRecordedForItsRun(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{"running": true}, true)
	x := e.executor("")
	s, err := x.Start(context.Background(), "a", e.repo, "main", "standard", "opus", "run-1", e.state, nil)
	if err != nil {
		t.Fatal(err)
	}
	sid := argAfter(e.starts()[0], "--session-id")
	if !sessionIDUUID.MatchString(sid) || s.Str("claude_session_id") != sid {
		t.Fatalf("session id %q, start %v", sid, e.starts()[0])
	}
	if !slices.Contains(transcript.HostedSessions("run-1"), sid) {
		t.Errorf("run index %v lacks %s", transcript.HostedSessions("run-1"), sid)
	}
}

func TestSessionIDReopenGetsANewID(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{"running": true}, true)
	x := e.executor("")
	for range 2 {
		if _, err := x.Start(context.Background(), "a", e.repo, "main", "standard", "opus", "run-1", e.state, nil); err != nil {
			t.Fatal(err)
		}
	}
	first, second := argAfter(e.starts()[0], "--session-id"), argAfter(e.starts()[1], "--session-id")
	if first == second || !sessionIDUUID.MatchString(second) {
		t.Fatalf("ids %q and %q", first, second)
	}
	hosted := transcript.HostedSessions("run-1")
	if !slices.Contains(hosted, first) || !slices.Contains(hosted, second) {
		t.Errorf("run index %v lacks one of %s, %s", hosted, first, second)
	}
}

func TestSessionIDForkGetsANewIDAndWarmReaderKeepsItsPresetID(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{"running": true}, true)
	x := e.warmExecutor("", nil)
	e.warmStart(x, "a", "opus")
	starts := e.starts()
	warmSID := argAfter(starts[0], "--session-id")
	recorded := x.warmRecords(e.state)["opus"].(map[string]any)["session_id"]
	if warmSID != recorded {
		t.Errorf("warm reader start %s, record %v", warmSID, recorded)
	}
	forks := e.forks()
	if len(forks) != 1 {
		t.Fatalf("forks %v", forks)
	}
	forkSID := argAfter(forks[0], "--session-id")
	if !sessionIDUUID.MatchString(forkSID) || forkSID == warmSID || argAfter(forks[0], "--resume") != warmSID {
		t.Fatalf("fork %v", forks[0])
	}
	hosted := transcript.HostedSessions("run-1")
	if !slices.Contains(hosted, warmSID) || !slices.Contains(hosted, forkSID) {
		t.Errorf("run index %v lacks warm %s or fork %s", hosted, warmSID, forkSID)
	}
}
