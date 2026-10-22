package herdr

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
)

const (
	unsentScreen = "❯ old message\n  ⎿ done\n" + "────────────────────────────────────────\n" +
		"❯ Read the worker prompt from /x/p.md and carry it out.\n  in summary.\n" +
		"────────────────────────────────────────\n  -- INSERT -- ⏵⏵ auto mode on"
	pastedScreen = "❯ old message\n────────────────────────────────────────\n❯ [Pasted text #1 +1 lines]\n" +
		"────────────────────────────────────────\n  -- INSERT --"
	emptyScreen = "❯ old message\n  ⎿ done\n────────────────────────────────────────\n❯\n" +
		"────────────────────────────────────────\n  -- INSERT --"
)

func TestPendingInput(t *testing.T) {
	for _, c := range []struct{ name, screen, want string }{
		{"unsent prompt", unsentScreen, "Read the worker prompt from /x/p.md and carry it out. in summary."},
		{"paste placeholder", pastedScreen, "[Pasted text #1 +1 lines]"},
		{"empty box", emptyScreen, ""},
	} {
		got, ok := host.PendingInput(host.SplitLines(c.screen))
		if !ok || got != c.want {
			t.Errorf("%s: %q %v, want %q", c.name, got, ok, c.want)
		}
	}
	if _, ok := host.PendingInput([]string{"❯ hi", strings.Repeat("─", 40)}); ok {
		t.Error("one rule line is not an input box")
	}
}

func idleExecutor(e *env) *Executor { return e.executor(`{"herdr": {"idle_grace": 10}}`) }

func TestIdleWithUnsentPromptPressesEnterOnceThenStalls(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{"running": true, "statuses": []any{"idle"}, "screen": unsentScreen}, true)
	_, err := idleExecutor(e).WaitResult(context.Background(), e.manualSession(), e.result, 3600e9)
	n := needOf(t, err)
	if n.Kind != host.KindStalled || !strings.Contains(n.Detail, "after a nudge") {
		t.Errorf("%v", n)
	}
	if got := e.count("agent send-keys"); got != 1 {
		t.Errorf("send-keys %d, want 1", got)
	}
	if e.has("agent prompt") {
		t.Error("an unsent prompt must not be resent")
	}
}

func TestIdleWithEmptyBoxResendsThePrompt(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{"running": true, "statuses": []any{"idle"}, "screen": emptyScreen}, true)
	s := e.manualSession()
	s.Set("last_prompt", "ORIGINAL PROMPT LINE")
	_, err := idleExecutor(e).WaitResult(context.Background(), s, e.result, 3600e9)
	if n := needOf(t, err); n.Kind != host.KindStalled {
		t.Errorf("%v", n)
	}
	prompts := e.callsOf("agent prompt")
	if len(prompts) != 1 {
		t.Fatalf("prompts %v", prompts)
	}
	sent := prompts[0].Argv[len(prompts[0].Argv)-1]
	if containsAll(sent, "ORIGINAL PROMPT LINE", e.result) != nil {
		t.Errorf("sent %q", sent)
	}
}

func TestAChangingScreenNeverNudges(t *testing.T) {
	e := newEnv(t)
	var screens []any
	for i := 0; i < 40; i++ {
		screens = append(screens, emptyScreen+strings.Repeat(".", i))
	}
	e.scenario(map[string]any{"running": true, "statuses": []any{"idle"}, "screens": screens}, true)
	_, err := idleExecutor(e).WaitResult(context.Background(), e.manualSession(), e.result, 30e9)
	if n := needOf(t, err); n.Kind != host.KindTimeout {
		t.Errorf("%v", n)
	}
	if e.has("agent prompt") || e.has("agent send-keys") {
		t.Error("nudged although the screen kept changing")
	}
}

func TestInvalidResultWhileIdleIsAnErrorNeed(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{"running": true, "statuses": []any{"idle"}, "screen": "ok"}, true)
	_ = os.WriteFile(e.result, []byte(`{"status": "ok", "summary": "say "hi""}`), 0o644)
	_, err := e.executor("").WaitResult(context.Background(), e.manualSession(), e.result, 3600e9)
	n := needOf(t, err)
	if n.Kind != host.KindError || containsAll(n.Detail, e.result, "invalid JSON", `say "hi"`) != nil {
		t.Errorf("%v", n)
	}
}

func TestInvalidResultWhileWorkingKeepsWaiting(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{"running": true, "statuses": []any{"working"}, "screen": "busy"}, true)
	_ = os.WriteFile(e.result, []byte(`{"status": "ok", "summary": `), 0o644)
	_, err := e.executor("").WaitResult(context.Background(), e.manualSession(), e.result, 20e9)
	if n := needOf(t, err); n.Kind != host.KindTimeout {
		t.Errorf("%v", n)
	}
}

// Without a remembered task prompt the resend is only the first sentence
// pair: there is no "task below" to carry out.
func TestIdleResendWithoutLastPromptStopsAtStop(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{"running": true, "statuses": []any{"idle"}, "screen": emptyScreen}, true)
	_, _ = idleExecutor(e).WaitResult(context.Background(), e.manualSession(), e.result, 3600e9)
	prompts := e.callsOf("agent prompt")
	if len(prompts) != 1 {
		t.Fatalf("prompts %v", prompts)
	}
	sent := prompts[0].Argv[len(prompts[0].Argv)-1]
	want := "You stopped without writing " + e.result + ". If the task below is already done, write that file now and stop."
	if sent != want {
		t.Errorf("sent %q\nwant %q", sent, want)
	}
}
