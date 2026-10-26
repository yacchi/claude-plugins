package usage

import (
	"path/filepath"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/transcript"
)

const hostedSession = "11111111-1111-4111-8111-111111111111"

// hostedFixture points HOME, the state dir and the Claude config at temp
// directories and returns report options for the claude source only.
func hostedFixture(t *testing.T) (ReportOptions, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	cfg := decodeCfg(t, `{}`)
	return ReportOptions{Since: cutoff, Now: now, Sources: []string{"claude"}, Cfg: cfg, Home: home, RunIDs: []string{"wf_hosted"}}, filepath.Join(home, ".claude", "projects", "-p")
}

func TestHostedSessionTranscriptCountsForRun(t *testing.T) {
	o, project := hostedFixture(t)
	transcript.RecordHosted("wf_hosted", hostedSession)
	write(t, filepath.Join(project, hostedSession+".jsonl"),
		line{ts: now, in: 10, cacheWrite: 5, cacheRead: 100, out: 7, id: "h1"}.String())
	c := BuildReport(o).Sources.Entries["claude"]
	if c.Status != "ok" || c.Scope != "run" || c.Tokens.InputTokens != 15 || c.Tokens.CachedInputTokens != 100 || c.Tokens.OutputTokens != 7 {
		t.Errorf("%+v", c)
	}
}

func TestHostedSessionSubagentsCountForRun(t *testing.T) {
	o, project := hostedFixture(t)
	transcript.RecordHosted("wf_hosted", hostedSession)
	write(t, filepath.Join(project, hostedSession+".jsonl"), line{ts: now, in: 10, id: "h1"}.String())
	write(t, filepath.Join(project, hostedSession, "subagents", "agent-x.jsonl"), line{ts: now, in: 3, id: "h2"}.String())
	c := BuildReport(o).Sources.Entries["claude"]
	if c.Status != "ok" || c.Tokens.InputTokens != 13 {
		t.Errorf("%+v", c)
	}
}

func TestTranscriptReachableTwiceIsCountedOnce(t *testing.T) {
	o, project := hostedFixture(t)
	transcript.RecordHosted("wf_hosted", hostedSession)
	write(t, filepath.Join(project, hostedSession, "subagents", "workflows", "wf_hosted", "a.jsonl"), line{ts: now, in: 10, id: "w1"}.String())
	c := BuildReport(o).Sources.Entries["claude"]
	if c.Status != "ok" || c.Tokens.InputTokens != 10 || c.Sessions != 1 {
		t.Errorf("%+v", c)
	}
}

func TestRunWithNoWorkflowOrHostedSessionIsNotAttributable(t *testing.T) {
	o, project := hostedFixture(t)
	write(t, filepath.Join(project, hostedSession+".jsonl"), line{ts: now, in: 10, id: "h1"}.String())
	c := BuildReport(o).Sources.Entries["claude"]
	if !c.NotAttributable || c.Reason != "no matching transcript data" {
		t.Errorf("%+v", c)
	}
}
