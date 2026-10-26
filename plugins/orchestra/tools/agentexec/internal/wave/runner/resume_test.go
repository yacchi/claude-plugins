package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
)

// resumeGateHost is a fake host whose sessions carry a Claude session id,
// recording every prompt's files and every Close (which, like the real one,
// unregisters the session).
type resumeGateHost struct {
	*fakeHost
	sid     string
	files   [][]string
	retires []bool // remove flag of each Close
}

func (h *resumeGateHost) Start(ctx context.Context, pid, repo, baseRef, cls, model, runID, stateDir string, dirs []string) (*host.Session, error) {
	s, err := h.fakeHost.Start(ctx, pid, repo, baseRef, cls, model, runID, stateDir, dirs)
	if err == nil && h.sid != "" {
		s.Extra = map[string]any{"claude_session_id": h.sid}
	}
	return s, err
}

func (h *resumeGateHost) Prompt(ctx context.Context, s *host.Session, files []string, resultPath string, timeout time.Duration) error {
	h.mu.Lock()
	h.files = append(h.files, append([]string{}, files...))
	h.mu.Unlock()
	return h.fakeHost.Prompt(ctx, s, files, resultPath, timeout)
}

func (h *resumeGateHost) Close(s *host.Session, remove bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.retires = append(h.retires, remove)
	delete(h.sessions, s.Pkg)
	return nil
}

// resumeGateKeptTree counts the closes that kept the tree (the retirement;
// the close after integration removes it).
func resumeGateKeptTree(h *resumeGateHost) int {
	n := 0
	for _, remove := range h.retires {
		if !remove {
			n++
		}
	}
	return n
}

// resumeGateTranscript writes sid's transcript into the test's Claude
// config dir (newWaveRepo's temp $CLAUDE_CONFIG_DIR); its last response
// carried input+cacheRead+cacheCreate.
func resumeGateTranscript(t *testing.T, sid string, input, cacheRead, cacheCreate int) {
	t.Helper()
	if sid == "" {
		return
	}
	writeFile(t, filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects", "p", sid+".jsonl"),
		`{"type":"user","message":{"content":"hi"}}`+"\n"+fmt.Sprintf(
			`{"type":"assistant","message":{"usage":{"input_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d}}}`+"\n",
			input, cacheRead, cacheCreate))
}

// resumeGateConfig is a config with enforcement.resume_max_tokens = limit
// (nil = key absent).
func resumeGateConfig(limit any) func() (*config.Resolved, error) {
	values := map[string]any{}
	if limit != nil {
		values["enforcement"] = map[string]any{"resume_max_tokens": limit}
	}
	return func() (*config.Resolved, error) { return &config.Resolved{Values: values}, nil }
}

// resumeGateRun runs A in the host with a failing first attempt, so the
// self-verify correction goes to A's live session (Claude session sid);
// transcript is the session whose transcript ends on usage (input,
// cache read, cache creation).
func resumeGateRun(t *testing.T, sid, transcript string, usage [3]int, cfg func() (*config.Resolved, error), delegates int) (*waveRepo, *resumeGateHost, *fakeExecutor) {
	t.Helper()
	w := newWaveRepo(t)
	resumeGateTranscript(t, transcript, usage[0], usage[1], usage[2])
	w.checks(noBadCheck)
	w.plan(pkgDef{id: "A"})
	h := &resumeGateHost{fakeHost: newFakeHost("herdr"), sid: sid}
	n := 0
	h.work = func(pid, tree string) {
		n++
		text := "good\n"
		if n == 1 {
			text = "BAD\n"
		}
		writeFile(t, filepath.Join(tree, "a.txt"), text)
	}
	var rounds []round
	for range delegates {
		rounds = append(rounds, result(delegated, nil))
	}
	ex := newFake(t, map[string][]round{"A": rounds})
	_, code := w.runWithHost(ex, Deps{Host: h, Config: cfg})
	eq(t, "exit", code, 0)
	if got, _ := w.intFile("a.txt"); got != "good\n" {
		t.Errorf("a.txt %q", got)
	}
	return w, h, ex
}

func TestResumeGateRetiresALongSessionForAFreshOne(t *testing.T) {
	w, h, ex := resumeGateRun(t, "sid-A", "sid-A", [3]int{1000, 140000, 9000}, resumeGateConfig(100000), 2)

	eq(t, "kept-tree closes", resumeGateKeptTree(h), 1)
	eqJSON(t, "resume-retired", w.details("resume-retired", "A"),
		[]map[string]any{{"host": "herdr", "context_tokens": 150000.0, "limit": 90000.0}})
	if len(ex.calls) != 2 {
		t.Fatalf("executor calls %d", len(ex.calls))
	}
	correction := filepath.Join(w.waveDir, "corrections", "A.md")
	want := append(append([]string{}, ex.calls[0].PromptFiles...), correction)
	eqJSON(t, "fresh prompt", ex.calls[1].PromptFiles, want)
	eq(t, "noResume", ex.calls[1].NoResume, true)
	// One prompt per session: the old one never got the correction.
	eqJSON(t, "host prompts", h.files, [][]string{ex.calls[0].PromptFiles, want})
	eq(t, "starts", len(h.started), 2)
}

func TestResumeGateResumesWhenNotRetired(t *testing.T) {
	cases := []struct {
		name, sid, transcript string
		tokens                int
		limit                 any
	}{
		{"below the limit", "sid-A", "sid-A", 50000, 100000},
		{"no claude session id", "", "sid-A", 150000, 100000},
		{"no transcript", "sid-A", "", 0, 100000},
		{"limit off", "sid-A", "sid-A", 150000, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, h, ex := resumeGateRun(t, c.sid, c.transcript, [3]int{c.tokens, 0, 0}, resumeGateConfig(c.limit), 1)

			eq(t, "kept-tree closes", resumeGateKeptTree(h), 0)
			eq(t, "resume-retired", len(w.events("resume-retired")), 0)
			eq(t, "executor calls", len(ex.calls), 1)
			correction := filepath.Join(w.waveDir, "corrections", "A.md")
			eqJSON(t, "host prompts", h.files, [][]string{ex.calls[0].PromptFiles, {correction}})
		})
	}
}

func TestResumeGateUnresumableResultGetsAFullCorrection(t *testing.T) {
	w := newWaveRepo(t)
	w.checks(noBadCheck)
	w.plan(pkgDef{id: "A"})
	unresumable := func(t *testing.T, s spec, ex []string) map[string]any {
		out := edit(map[string]string{"a.txt": "BAD\n"}, editOpt{session: "s1"})(t, s, ex)
		out["resumable"] = false
		return out
	}
	ex := newFake(t, map[string][]round{"A": {unresumable, edit(map[string]string{"a.txt": "good\n"})}})
	_, code := w.run(ex)
	eq(t, "exit", code, 0)
	if len(ex.calls) != 2 {
		t.Fatalf("calls %d", len(ex.calls))
	}
	correction := filepath.Join(w.waveDir, "corrections", "A.md")
	want := append(append([]string{}, ex.calls[0].PromptFiles...), correction)
	eqJSON(t, "correction prompt", ex.calls[1].PromptFiles, want)
	eq(t, "noResume", ex.calls[1].NoResume, true)
}
