package herdr

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
)

// reviveServerStarts counts the `server` spawns the executor made.
func reviveServerStarts(e *env) int {
	n := 0
	for _, argv := range e.spawns {
		if len(argv) > 0 && argv[len(argv)-1] == "server" {
			n++
		}
	}
	return n
}

// reviveShim is a herdr binary that, while a flag file exists, stops the
// fake server and fails `workspace create` the way a parallel run's refresh
// does. A "once" flag is removed by its first failure; an "always" flag
// keeps failing.
func reviveShim(t *testing.T, e *env, once, always bool) string {
	t.Helper()
	onceFlag := filepath.Join(e.tmp, "fail-once")
	alwaysFlag := filepath.Join(e.tmp, "fail-always")
	if once {
		if err := os.WriteFile(onceFlag, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if always {
		if err := os.WriteFile(alwaysFlag, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := "#!/bin/sh\n" +
		"case \" $* \" in\n" +
		"*\" workspace create \"*)\n" +
		"\tif [ -f '" + onceFlag + "' ] || [ -f '" + alwaysFlag + "' ]; then\n" +
		"\t\trm -f '" + onceFlag + "'\n" +
		"\t\t'" + fakeBin + "' server stop >/dev/null 2>&1\n" +
		"\t\techo 'herdr workspace create failed: no herdr server is running' >&2\n" +
		"\t\texit 1\n" +
		"\tfi\n" +
		"\t;;\n" +
		"esac\n" +
		"exec '" + fakeBin + "' \"$@\"\n"
	shim := filepath.Join(e.tmp, "herdr-shim")
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return shim
}

func TestAServerStoppedOutOfBandIsStartedAgainByTheNextPackage(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{}, true)
	x := e.executor("")
	e.mustStart(x, "a")
	if _, _, err := x.call(context.Background(), callTimeout, "", "server", "stop"); err != nil {
		t.Fatal(err)
	}
	e.mustStart(x, "b")
	if n := reviveServerStarts(e); n != 2 {
		t.Errorf("server starts %d, want 2", n)
	}
}

func TestARunningServerIsNotStartedAgain(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{}, true)
	x := e.executor("")
	e.mustStart(x, "a")
	e.mustStart(x, "b")
	if n := reviveServerStarts(e); n != 1 {
		t.Errorf("server starts %d, want 1", n)
	}
}

func TestAWorkspaceCreateFailingWithTheServerGoneIsRetriedOnce(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{}, true)
	x := e.executor("")
	x.Binary = reviveShim(t, e, true, false)
	e.mustStart(x, "a")
	if n := len(e.callsOf("workspace create")); n != 1 {
		t.Errorf("successful creates %d, want 1", n)
	}
	if n := reviveServerStarts(e); n != 2 {
		t.Errorf("server starts %d, want 2", n)
	}
}

func TestAWorkspaceCreateThatKeepsFailingIsRetriedOnlyOnce(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{}, true)
	x := e.executor("")
	x.Binary = reviveShim(t, e, false, true)
	_, err := e.start(x, "a")
	if n := needOf(t, err); n.Kind != host.KindError {
		t.Errorf("%v", n)
	}
	if n := reviveServerStarts(e); n != 2 {
		t.Errorf("server starts %d, want 2 (one retry)", n)
	}
}
