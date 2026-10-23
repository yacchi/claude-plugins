package herdr

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
)

func TestAnOverLongStartLineIsRefusedBeforeTheWorkspace(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{"running": true}, true)
	var dirs []string
	for i := range 6 {
		dirs = append(dirs, filepath.Join(e.tmp, fmt.Sprintf("%02d-%s", i, strings.Repeat("x", 140))))
	}
	_, err := e.executor("").Start(context.Background(), "a", e.repo, "main", "standard", "opus", "run-1", e.state, dirs)
	n := needOf(t, err)
	if n.Kind != host.KindError || !strings.Contains(n.Detail, "MAX_CANON") {
		t.Errorf("%v", n)
	}
	if e.has("workspace create") {
		t.Error("a workspace was created for an over-long start line")
	}
}

func TestAFailedStartKeepsThePaneScreen(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{"running": true, "start_timeout": true, "screen": "zsh: command not found: claude\n$ "}, true)
	_, err := e.start(e.executor(""), "a")
	n := needOf(t, err)
	if n.Kind != host.KindError {
		t.Errorf("%v", n)
	}
	if err := containsAll(n.Detail, "timed out waiting for agent startup", "command not found: claude"); err != nil {
		t.Error(err)
	}
}
