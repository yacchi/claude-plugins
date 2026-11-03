package cli

import (
	"strings"
	"testing"
)

// dispatchWaitHopMsg is the refusal Claude Code's 180 s background move
// makes necessary; a longer hop would lose its answer.
const dispatchWaitHopMsg = "dispatch wait: --max-wait must be at most 170 seconds (Claude Code moves a foreground Bash call to the background after 180 s); use 150, the default"

func TestDispatchWaitHopRefusesAbove170(t *testing.T) {
	hookCmdIsolate(t)
	_, stderr, code := planDirRun(t, "dispatch", "wait", "--token", "dsp-000000000000", "--max-wait", "171")
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, dispatchWaitHopMsg) {
		t.Errorf("stderr = %q, want it to contain %q", stderr, dispatchWaitHopMsg)
	}
}

func TestDispatchWaitHopAccepts170(t *testing.T) {
	// An unknown token returns before any polling, so a 170 s window passes
	// the check and finishes at once.
	hookCmdIsolate(t)
	_, stderr, _ := planDirRun(t, "dispatch", "wait", "--token", "dsp-000000000000", "--max-wait", "170")
	if strings.Contains(stderr, "must be at most 170") {
		t.Fatalf("--max-wait 170 refused, want it accepted: %q", stderr)
	}
}
