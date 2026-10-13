package herdr

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
)

// The runner starts and prompts packages from parallel goroutines while its
// scheduler ticks the network: one server, every session registered.
func TestParallelPackagesShareOneServer(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{"prompt_result": map[string]any{"status": "ok", "summary": "done"}}, true)
	x := e.executor("")
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for _, pkg := range []string{"a", "b", "c"} {
		wg.Add(1)
		go func(pkg string) {
			defer wg.Done()
			s, err := e.start(x, pkg)
			if err == nil {
				result := filepath.Join(e.state, pkg+".result.json")
				_, err = host.PromptAndWait(context.Background(), x, s, []string{"/p/" + pkg + ".md"}, result, 60e9)
			}
			errs <- err
		}(pkg)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			x.NetworkTick()
			x.NetworkDenials(0)
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if len(e.spawns) != 1 {
		t.Errorf("servers started: %d", len(e.spawns))
	}
	if n := len(x.packageSessions(e.state)); n != 3 {
		t.Errorf("registered sessions: %d", n)
	}
}
