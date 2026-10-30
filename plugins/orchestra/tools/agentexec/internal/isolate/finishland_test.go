package isolate

import (
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
)

func TestLandMode(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.Resolved
		want string
	}{
		{"nil config", nil, LandModeAuto},
		{"missing key", &config.Resolved{Values: map[string]any{}}, LandModeAuto},
		{"auto", &config.Resolved{Values: map[string]any{"isolation": map[string]any{"land": "auto"}}}, LandModeAuto},
		{"ask", &config.Resolved{Values: map[string]any{"isolation": map[string]any{"land": "ask"}}}, LandModeAsk},
	}
	for _, c := range cases {
		if got := LandMode(c.cfg); got != c.want {
			t.Errorf("%s: LandMode = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestFinishLandAskTouchesNothing(t *testing.T) {
	repo := landRepo(t)
	path := landAssembly(t, repo)
	seed := strings.TrimSpace(git(t, repo, "rev-parse", "main"))
	res := FinishLand(LandModeAsk, repo, "prog", "feat: x", "auto-land")
	want := "isolation.land is ask: when the user asks, land it from their checkout with 'agent-exec isolate land --request prog -m <message>'"
	if res.Status != "ask" || res.Task != "prog" || res.Reason != want {
		t.Fatalf("got %+v", res)
	}
	if b := headBranch(t, repo); b != "main" {
		t.Fatalf("HEAD is %s", b)
	}
	if fb := featureBranches(t, repo); fb != "" {
		t.Fatalf("feature branches created: %s", fb)
	}
	if m := strings.TrimSpace(git(t, repo, "rev-parse", "main")); m != seed {
		t.Fatalf("main moved: %s -> %s", seed, m)
	}
	if !exists(path) {
		t.Fatal("assembly removed")
	}
}

func TestFinishLandRefusesBlankMessage(t *testing.T) {
	repo := landRepo(t)
	landAssembly(t, repo)
	res := FinishLand(LandModeAuto, repo, "prog", "  ", "auto-land")
	want := "no commit message to land with: land it from the checkout with 'agent-exec isolate land --request prog -m <message in the repository's commit convention>'"
	if res.Status != "refused" || res.Task != "prog" || res.Reason != want {
		t.Fatalf("got %+v", res)
	}
	if b := headBranch(t, repo); b != "main" {
		t.Fatalf("HEAD is %s", b)
	}
	if fb := featureBranches(t, repo); fb != "" {
		t.Fatalf("feature branches created: %s", fb)
	}
}

func TestFinishLandAutoOnDefaultBranchLandsOnFeature(t *testing.T) {
	repo := landRepo(t)
	landAssembly(t, repo)
	res := FinishLand(LandModeAuto, repo, "prog", "feat: x", "auto-land")
	if res.Status != "landed" || res.Branch != "feature/auto-land" || !res.Created {
		t.Fatalf("got %+v", res)
	}
	if b := headBranch(t, repo); b != "feature/auto-land" {
		t.Fatalf("HEAD is %s", b)
	}
}
