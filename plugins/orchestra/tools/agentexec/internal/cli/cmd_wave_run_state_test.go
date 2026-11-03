package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// waveStateRun executes `wave run` with args and returns its stdout, its exit
// code and the message of its exit error ("" on success).
func waveStateRun(t *testing.T, args ...string) (string, int, string) {
	t.Helper()
	var buf bytes.Buffer
	cmd := newWaveCmd()
	cmd.SetArgs(append([]string{"run"}, args...))
	cmd.SetOut(&buf)
	cmd.SetErr(io.Discard)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	if err := cmd.Execute(); err != nil {
		ee := asExitError(cmd, err)
		return buf.String(), ee.Code, ee.Msg
	}
	return buf.String(), 0, ""
}

// waveStateTempRoot makes a directory under t.TempDir() the only temporary
// root for the test, and returns it.
func waveStateTempRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(paths.RealPath(t.TempDir()), "tmp")
	old := paths.TempRootsSeam
	paths.TempRootsSeam = func() []string { return []string{root} }
	t.Cleanup(func() { paths.TempRootsSeam = old })
	return root
}

func TestWaveStateResolve(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	if got, err := waveStateResolve("/a/plans/x/plan.json", ""); err != nil || got != "/a/plans/x/state.json" {
		t.Errorf("default beside absolute plan = %q, %v; want /a/plans/x/state.json", got, err)
	}
	if got, err := waveStateResolve("/a/p.json", "rel/s.json"); err != nil || got != "rel/s.json" {
		t.Errorf("explicit state = %q, %v; want rel/s.json kept as given", got, err)
	}
	want := filepath.Join(cwd, "sub", "state.json")
	if got, err := waveStateResolve("sub/plan.json", ""); err != nil || got != want {
		t.Errorf("default beside relative plan = %q, %v; want %q", got, err, want)
	}
}

func TestWaveStateDetachedArgs(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		state string
		want  []string
	}{
		{
			name:  "defaulted state gets appended",
			args:  []string{"wave", "run", "--plan", "/a/p.json", "--into", "x", "--detach", "--json"},
			state: "/a/state.json",
			want:  []string{"wave", "run", "--plan", "/a/p.json", "--into", "x", "--state=/a/state.json"},
		},
		{
			name:  "explicit state is rewritten (separate value)",
			args:  []string{"wave", "run", "--plan", "p", "--state", "rel/s.json", "--into", "x", "--detach"},
			state: "/abs/s.json",
			want:  []string{"wave", "run", "--plan", "p", "--state=/abs/s.json", "--into", "x"},
		},
		{
			name:  "explicit state is rewritten (equals form)",
			args:  []string{"wave", "run", "--plan", "p", "--state=rel/s.json", "--into", "x", "--detach"},
			state: "/abs/s.json",
			want:  []string{"wave", "run", "--plan", "p", "--state=/abs/s.json", "--into", "x"},
		},
	}
	for _, c := range cases {
		if got := detachedArgs(c.args, c.state); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: detachedArgs = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestWaveStateDefaultedWithoutFlag(t *testing.T) {
	repo := waveRunReqRepo(t, true)
	root := os.Getenv(isolate.RequestRootEnv)
	plan := filepath.Join(t.TempDir(), "p.json")
	out, code, _ := waveStateRun(t, "--plan", plan, "--into", "x", "--repo", repo, "--uncommitted", "ignore")
	if code != 2 {
		t.Fatalf("exit %d, want 2 (the plan error): %s", code, out)
	}
	var rep map[string]any
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("output %q is not the runner's report: %v", out, err)
	}
	if rep["kind"] != "plan" {
		t.Errorf("report: %v, want kind plan", rep)
	}
	waveRunReqAssertNoTree(t, repo, root)
}

func TestWaveStateRefusesTempPlan(t *testing.T) {
	repo := waveRunReqRepo(t, true)
	root := os.Getenv(isolate.RequestRootEnv)
	R := waveStateTempRoot(t)
	plan := filepath.Join(R, "p", "plan.json")
	out, code, msg := waveStateRun(t, "--plan", plan, "--into", "x", "--repo", repo, "--detach")
	if code != 2 {
		t.Fatalf("exit %d, want 2: %s", code, out)
	}
	want := "wave run: " + paths.RefuseTemp("--plan", plan).Error()
	if msg != want {
		t.Errorf("msg = %q, want %q", msg, want)
	}
	if out != "" {
		t.Errorf("stdout %q, want none", out)
	}
	if _, err := os.Stat(filepath.Join(R, "p")); !os.IsNotExist(err) {
		t.Errorf("plan dir under the temp root was created (stat err %v)", err)
	}
	waveRunReqAssertNoTree(t, repo, root)
}

func TestWaveStateRefusesTempState(t *testing.T) {
	repo := waveRunReqRepo(t, true)
	root := os.Getenv(isolate.RequestRootEnv)
	R := waveStateTempRoot(t)
	state := filepath.Join(R, "s", "state.json")
	plan := filepath.Join(t.TempDir(), "p.json")
	out, code, msg := waveStateRun(t, "--plan", plan, "--state", state, "--into", "x", "--repo", repo, "--detach")
	if code != 2 {
		t.Fatalf("exit %d, want 2: %s", code, out)
	}
	want := "wave run: " + paths.RefuseTemp("--state", state).Error()
	if msg != want {
		t.Errorf("msg = %q, want %q", msg, want)
	}
	if _, err := os.Stat(filepath.Join(R, "s")); !os.IsNotExist(err) {
		t.Errorf("state dir under the temp root was created (stat err %v)", err)
	}
	waveRunReqAssertNoTree(t, repo, root)
}

func TestWaveIntoResolve(t *testing.T) {
	t.Chdir(t.TempDir())
	cases := []struct {
		name, plan, runID, into, want string
	}{
		{"defaults to the plan directory", "/a/plans/20261103-abc/plan.json", "", "", "prog-20261103-abc"},
		{"run id beats the directory", "/a/plans/p1/plan.json", "r9", "", "prog-r9"},
		{"explicit into beats the run id", "/a/plans/p1/plan.json", "r9", "mine", "mine"},
		{"explicit into beats the directory", "/a/plans/p1/plan.json", "", "mine", "mine"},
		{"relative plan uses its directory", "sub/plan.json", "", "", "prog-sub"},
	}
	for _, c := range cases {
		got, err := waveIntoResolve(c.plan, c.runID, c.into)
		if err != nil || got != c.want {
			t.Errorf("%s: waveIntoResolve = %q, %v; want %q", c.name, got, err, c.want)
		}
	}
}

func TestWaveIntoNotRequired(t *testing.T) {
	repo := waveRunReqRepo(t, true)
	root := os.Getenv(isolate.RequestRootEnv)
	plan := filepath.Join(t.TempDir(), "p.json")
	out, code, msg := waveStateRun(t, "--plan", plan, "--repo", repo, "--uncommitted", "ignore")
	if code != 2 {
		t.Fatalf("exit %d, want 2 (the plan error): %s", code, out)
	}
	if strings.Contains(msg, `"into"`) {
		t.Errorf("msg %q: --into is still required", msg)
	}
	var rep map[string]any
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("output %q is not the runner's report: %v", out, err)
	}
	if rep["kind"] != "plan" {
		t.Errorf("report: %v, want kind plan", rep)
	}
	waveRunReqAssertNoTree(t, repo, root)
}

func TestWavePlanStillRequired(t *testing.T) {
	_, code, msg := waveStateRun(t, "--into", "x")
	if code == 0 {
		t.Fatal("exit 0 without --plan, want nonzero")
	}
	if !strings.Contains(msg, "plan") {
		t.Errorf("msg %q, want it to name --plan", msg)
	}
}
