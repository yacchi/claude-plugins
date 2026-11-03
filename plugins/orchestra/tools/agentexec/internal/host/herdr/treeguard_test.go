package herdr

import (
	"context"
	"errors"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/proctree"
)

// treeGuardCalls counts what the seams were asked for.
type treeGuardCalls struct {
	lookups, measures int
	kills             []int
	includeRoot       []bool
}

// treeGuardRestore puts the four seams back when the test ends.
func treeGuardRestore(t *testing.T) {
	t.Helper()
	lookup, measure, kill, limits := treeGuardLookup, treeGuardMeasure, treeGuardKill, treeGuardLimits
	t.Cleanup(func() {
		treeGuardLookup, treeGuardMeasure, treeGuardKill, treeGuardLimits = lookup, measure, kill, limits
	})
}

// treeGuardSeams replaces every seam: the pane's shell is root, measure
// returns u, and kill records its calls and returns killed and killErr.
func treeGuardSeams(t *testing.T, root int, u proctree.Usage, measureErr error, killed int, killErr error) *treeGuardCalls {
	t.Helper()
	treeGuardRestore(t)
	c := &treeGuardCalls{}
	treeGuardLookup = func(_ *Executor, _ context.Context, _ *host.Session) int {
		c.lookups++
		return root
	}
	treeGuardMeasure = func(int) (proctree.Usage, error) {
		c.measures++
		return u, measureErr
	}
	treeGuardKill = func(r int, includeRoot bool) (int, error) {
		c.kills = append(c.kills, r)
		c.includeRoot = append(c.includeRoot, includeRoot)
		return killed, killErr
	}
	treeGuardLimits = func() proctree.Limits { return proctree.Limits{MaxProcs: 200} }
	return c
}

// treeGuardBusy is a working agent that never writes its result.
func treeGuardBusy(e *env) {
	e.scenario(map[string]any{"running": true, "statuses": []any{"working"}, "screen": "busy"}, true)
}

func TestTreeGuardShellPID(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		want    int
	}{
		{"top level", map[string]any{"shell_pid": 4242.0}, 4242},
		{"nested", map[string]any{"result": map[string]any{"pane": map[string]any{"shell_pid": 4242.0}}}, 4242},
		{"missing", map[string]any{}, 0},
		{"string", map[string]any{"shell_pid": "4242"}, 0},
		{"init pid", map[string]any{"shell_pid": 1.0}, 0},
		{"zero", map[string]any{"shell_pid": 0.0}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := treeGuardShellPID(tc.payload); got != tc.want {
				t.Fatalf("treeGuardShellPID = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestTreeGuardTripKillsAndFails(t *testing.T) {
	e := newEnv(t)
	treeGuardBusy(e)
	x := e.executor("")
	s := e.manualSession()
	c := treeGuardSeams(t, 4242, proctree.Usage{Root: 4242, Procs: 312}, nil, 312, nil)

	_, err := x.waitResult(context.Background(), s, e.result, 20e9)
	n := needOf(t, err)
	if n.Kind != host.KindError {
		t.Fatalf("kind = %q, want %q", n.Kind, host.KindError)
	}
	want := "process guard: 312 processes under pid 4242 (limit 200); killed the worker's process tree (312 processes)"
	if n.Detail != want {
		t.Fatalf("detail = %q\nwant     %q", n.Detail, want)
	}
	if len(c.kills) != 1 || c.kills[0] != 4242 || !c.includeRoot[0] {
		t.Fatalf("kill calls = %v (includeRoot %v), want one call (4242, true)", c.kills, c.includeRoot)
	}
}

func TestTreeGuardKillErrorSuffix(t *testing.T) {
	e := newEnv(t)
	treeGuardBusy(e)
	x := e.executor("")
	s := e.manualSession()
	killErr := errors.New("proctree: could not kill 2 of 5 processes: operation not permitted")
	treeGuardSeams(t, 4242, proctree.Usage{Root: 4242, Procs: 312}, nil, 3, killErr)

	_, err := x.waitResult(context.Background(), s, e.result, 20e9)
	n := needOf(t, err)
	want := "process guard: 312 processes under pid 4242 (limit 200); killed the worker's process tree (3 processes); " +
		"proctree: could not kill 2 of 5 processes: operation not permitted"
	if n.Detail != want {
		t.Fatalf("detail = %q\nwant     %q", n.Detail, want)
	}
}

func TestTreeGuardKillNoRootHasNoSuffix(t *testing.T) {
	e := newEnv(t)
	treeGuardBusy(e)
	x := e.executor("")
	s := e.manualSession()
	treeGuardSeams(t, 4242, proctree.Usage{Root: 4242, Procs: 312}, nil, 0, proctree.ErrNoRoot)

	_, err := x.waitResult(context.Background(), s, e.result, 20e9)
	n := needOf(t, err)
	want := "process guard: 312 processes under pid 4242 (limit 200); killed the worker's process tree (0 processes)"
	if n.Detail != want {
		t.Fatalf("detail = %q\nwant     %q", n.Detail, want)
	}
}

func TestTreeGuardUnderLimitKeepsWaiting(t *testing.T) {
	e := newEnv(t)
	treeGuardBusy(e)
	x := e.executor("")
	s := e.manualSession()
	c := treeGuardSeams(t, 4242, proctree.Usage{Root: 4242, Procs: 10}, nil, 0, nil)

	_, err := x.waitResult(context.Background(), s, e.result, 20e9)
	if n := needOf(t, err); n.Kind != host.KindTimeout {
		t.Fatalf("kind = %q, want %q", n.Kind, host.KindTimeout)
	}
	if len(c.kills) != 0 {
		t.Fatalf("kill called %v times under the limit", len(c.kills))
	}
	if c.measures <= 1 {
		t.Fatalf("measure called %d times, want it measured on every poll", c.measures)
	}
	if c.lookups != 1 {
		t.Fatalf("lookup called %d times, want once per wait", c.lookups)
	}
}

func TestTreeGuardUnsupportedStopsGuarding(t *testing.T) {
	e := newEnv(t)
	treeGuardBusy(e)
	x := e.executor("")
	s := e.manualSession()
	c := treeGuardSeams(t, 4242, proctree.Usage{}, proctree.ErrUnsupported, 0, nil)

	_, err := x.waitResult(context.Background(), s, e.result, 20e9)
	if n := needOf(t, err); n.Kind != host.KindTimeout {
		t.Fatalf("kind = %q, want %q", n.Kind, host.KindTimeout)
	}
	if c.measures != 1 {
		t.Fatalf("measure called %d times, want exactly 1", c.measures)
	}
	if len(c.kills) != 0 {
		t.Fatalf("kill called %v times on an unsupported platform", len(c.kills))
	}
}

func TestTreeGuardResultWinsOverTrip(t *testing.T) {
	e := newEnv(t)
	treeGuardBusy(e)
	x := e.executor("")
	s := e.manualSession()
	e.writeResult("ok")
	c := treeGuardSeams(t, 4242, proctree.Usage{Root: 4242, Procs: 312}, nil, 312, nil)

	data, err := x.waitResult(context.Background(), s, e.result, 20e9)
	if err != nil {
		t.Fatalf("waitResult: %v", err)
	}
	if data["status"] != "ok" {
		t.Fatalf("data = %v, want the written result", data)
	}
	if c.measures != 0 || len(c.kills) != 0 {
		t.Fatalf("measure %d, kill %v: a written result must end the wait before the tree is checked", c.measures, c.kills)
	}
}

func TestTreeGuardLookupFailureRunsUnguarded(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{"running": true, "statuses": []any{"working"}, "screen": "busy"}, true)
	x := e.executor("")
	s := e.manualSession()
	treeGuardRestore(t)
	measured := 0
	treeGuardMeasure = func(int) (proctree.Usage, error) {
		measured++
		return proctree.Usage{Procs: 312}, nil
	}

	if got := x.treeGuardRoot(context.Background(), s); got != 0 {
		t.Fatalf("treeGuardRoot = %d, want 0 when process-info fails", got)
	}
	if n := e.count("pane process-info"); n != 1 {
		t.Fatalf("pane process-info called %d times, want 1", n)
	}

	_, err := x.waitResult(context.Background(), s, e.result, 20e9)
	if n := needOf(t, err); n.Kind != host.KindTimeout {
		t.Fatalf("kind = %q, want %q", n.Kind, host.KindTimeout)
	}
	if measured != 0 {
		t.Fatalf("measure called %d times with no root", measured)
	}
}

func TestTreeGuardNoPaneSkipsLookup(t *testing.T) {
	e := newEnv(t)
	e.scenario(map[string]any{"running": true, "statuses": []any{"working"}, "screen": "busy"}, true)
	x := e.executor("")
	s := e.manualSession()
	s.Set("pane", "")

	if got := x.treeGuardRoot(context.Background(), s); got != 0 {
		t.Fatalf("treeGuardRoot = %d, want 0 without a pane", got)
	}
	if e.has("pane process-info") {
		t.Fatal("pane process-info called for a session without a pane")
	}
}
