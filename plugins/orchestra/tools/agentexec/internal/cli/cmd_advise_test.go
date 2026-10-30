package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
)

func TestAdviseJSONListsRepoAdvice(t *testing.T) {
	hookCmdIsolate(t)
	repo := t.TempDir()
	if _, err := gitx.Run(repo, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "jest.config.js"), []byte("module.exports = { preset: 'ts-jest' };\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	out, code := hookCmdRun(t, "", "advise", "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var got struct {
		Advice []struct {
			ID   string `json:"id"`
			Path string `json:"path"`
		} `json:"advice"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if len(got.Advice) != 1 || got.Advice[0].ID != "ts-jest-typecheck" || got.Advice[0].Path != "jest.config.js" {
		t.Errorf("advice = %+v, want one ts-jest-typecheck for jest.config.js", got.Advice)
	}
}

func TestAdviseOutsideRepoIsEmptyList(t *testing.T) {
	hookCmdIsolate(t)
	t.Chdir(t.TempDir())
	if out, code := hookCmdRun(t, "", "advise", "--json"); code != 0 || strings.TrimSpace(out) != `{"advice":[]}` {
		t.Errorf("exit %d, output %q; want exit 0 and an empty list, not null", code, out)
	}
	if out, code := hookCmdRun(t, "", "advise"); code != 0 || out != "no advice\n" {
		t.Errorf("text: exit %d, stdout %q; want exit 0 and \"no advice\"", code, out)
	}
}
