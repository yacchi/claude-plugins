package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/statusfile"
)

func TestStatusPlanUpdateKeepsTitleAndStartedAt(t *testing.T) {
	hookCmdIsolate(t)
	repo := t.TempDir()

	if _, code := hookCmdRun(t, "", "status", "plan", "--id", "p1", "--status", "planning",
		"--phase", "explore", "--title", "T", "--repo", repo); code != 0 {
		t.Fatalf("first status plan exit = %d, want 0", code)
	}
	first, ok := statusfile.Default().ReadPlan("p1")
	if !ok {
		t.Fatal("plan p1 not written")
	}

	if _, code := hookCmdRun(t, "", "status", "plan", "--id", "p1", "--status", "needs-user",
		"--phase", "done", "--questions", "2", "--repo", repo); code != 0 {
		t.Fatalf("second status plan exit = %d, want 0", code)
	}
	second, _ := statusfile.Default().ReadPlan("p1")
	if second.Title != "T" {
		t.Errorf("title = %q, want %q", second.Title, "T")
	}
	if second.StartedAt != first.StartedAt {
		t.Errorf("started_at = %d, want %d (kept)", second.StartedAt, first.StartedAt)
	}

	out, code := hookCmdRun(t, "", "status", "show")
	if code != 0 {
		t.Fatalf("status show exit = %d, want 0", code)
	}
	var doc statusfile.Doc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("status show output is not status.json: %v", err)
	}
	if len(doc.Plans) != 1 {
		t.Fatalf("plans = %d, want 1", len(doc.Plans))
	}
	if p := doc.Plans[0]; p.Status != "needs-user" || p.Questions != 2 {
		t.Errorf("plan = status %q questions %d, want needs-user and 2", p.Status, p.Questions)
	}
	if doc.Summary.Needs != 1 {
		t.Errorf("summary.needs = %d, want 1", doc.Summary.Needs)
	}
}

func TestStatusWhoamiReportsAccountAndHost(t *testing.T) {
	hookCmdIsolate(t)
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"),
		[]byte(`{"oauthAccount":{"organizationName":"Acme"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	out, code := hookCmdRun(t, "", "status", "whoami")
	if code != 0 {
		t.Fatalf("whoami exit = %d, want 0", code)
	}
	var got struct {
		Account *string `json:"account"`
		Host    string  `json:"host"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("whoami output %q: %v", out, err)
	}
	if got.Account == nil || *got.Account != "Acme" {
		t.Errorf("account = %v, want Acme", got.Account)
	}
	if strings.Contains(got.Host, ".") {
		t.Errorf("host = %q, want no domain part", got.Host)
	}

	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	out, code = hookCmdRun(t, "", "status", "whoami")
	if code != 0 {
		t.Fatalf("whoami with no account exit = %d, want 0", code)
	}
	var none map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &none); err != nil {
		t.Fatalf("whoami output %q: %v", out, err)
	}
	if string(none["account"]) != "null" {
		t.Errorf("account = %s, want null", none["account"])
	}
}

func TestStatusPlanRejectsBadFlags(t *testing.T) {
	hookCmdIsolate(t)
	for _, args := range [][]string{
		{"status", "plan", "--id", "bad id", "--status", "planning"},
		{"status", "plan", "--id", "p1", "--status", "nope"},
		{"status", "plan", "--id", "p1", "--status", "planning", "--phase", "nope"},
	} {
		if _, code := hookCmdRun(t, "", args...); code != 2 {
			t.Errorf("%v exit = %d, want 2", args, code)
		}
	}
}

func TestStatusCloseRefusesLiveRunnerAndAcceptsEnded(t *testing.T) {
	hookCmdIsolate(t)
	state := filepath.Join(t.TempDir(), "state.json")

	if _, code := hookCmdRun(t, "", "status", "close", "--state", state, "--status", "done"); code != 3 {
		t.Fatalf("unknown state exit = %d, want 3", code)
	}

	now := time.Now().Unix()
	run := statusfile.Run{
		Key:       statusfile.RunKey(state),
		PID:       os.Getpid(),
		UpdatedAt: now,
		StartedAt: now,
		Status:    "needs",
		State:     state,
	}
	if err := statusfile.Default().WriteRun(run); err != nil {
		t.Fatal(err)
	}
	if _, code := hookCmdRun(t, "", "status", "close", "--state", state, "--status", "stopped"); code != 2 {
		t.Fatalf("live runner exit = %d, want 2", code)
	}

	ended := now
	run.EndedAt = &ended
	if err := statusfile.Default().WriteRun(run); err != nil {
		t.Fatal(err)
	}
	if _, code := hookCmdRun(t, "", "status", "close", "--state", state, "--status", "stopped"); code != 0 {
		t.Fatalf("ended run exit = %d, want 0", code)
	}
	got, _ := statusfile.Default().ReadRun(run.Key)
	if got.Status != "stopped" {
		t.Errorf("status = %q, want stopped", got.Status)
	}
}
