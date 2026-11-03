package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/planning"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/statusfile"
)

const planTestIface = `{"id":"i","path":"i.txt"}`

// planTestDir is a directory holding interface i.txt and an empty specs folder.
func planTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "specs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "i.txt"), []byte("func I() int"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// planTestPkg is a skeleton entry for package id that uses interface i.
func planTestPkg(id, cls string) string {
	return fmt.Sprintf(`{"id":%q,"spec":"specs/%s.md","cls":%q,"brief":"b","risks":[],`+
		`"est_seconds":60,"files_owned":["%s.go"],"verification":"go test ./%s/...","interfaces":["i"]}`, id, id, cls, id, id)
}

// planTestWrite writes plan.json into dir and returns its path.
func planTestWrite(t *testing.T, dir, interfaces string, packages ...string) string {
	t.Helper()
	path := filepath.Join(dir, "plan.json")
	text := `{"commit_message":"feat: x","interfaces":[` + interfaces + `],"packages":[` + strings.Join(packages, ",") + `]}`
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPlanNextKeepsEveryReturnedID(t *testing.T) {
	hookCmdIsolate(t)
	dir := planTestDir(t)
	plan := planTestWrite(t, dir, planTestIface, planTestPkg("a", "light"), planTestPkg("b", "light"))
	if err := planning.Save(plan, planning.State{Round: 1, Specs: map[string]planning.SpecRecord{
		"a": {Hash: "h", Attempts: 1, InFlight: true},
		"b": {Hash: "h", Attempts: 1, InFlight: true},
	}}); err != nil {
		t.Fatal(err)
	}

	if _, code := hookCmdRun(t, "", "plan", "next", "--plan", plan, "--returned", "a", "--returned", "b"); code != 0 {
		t.Fatalf("plan next exit = %d, want 0", code)
	}
	st, err := planning.Load(plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if st.Specs[id].InFlight {
			t.Errorf("spec %s still in flight; --returned must clear every id given", id)
		}
	}
}

func TestPlanNextStatusWriteFailureKeepsExit(t *testing.T) {
	hookCmdIsolate(t)
	// A file where the status directory belongs makes the status write fail.
	state := paths.StateDir()
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "status"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := planTestDir(t)
	plan := planTestWrite(t, dir, planTestIface, planTestPkg("a", "light"))

	out, code := hookCmdRun(t, "", "plan", "next", "--plan", plan, "--id", "p1", "--repo", dir)
	if code != 0 {
		t.Fatalf("plan next exit = %d, want 0 when only the status write fails", code)
	}
	if !strings.Contains(out, `"step":"skeleton"`) {
		t.Errorf("stdout %q does not print the skeleton step", out)
	}
}

func TestPlanNextWritesStatusOnFirstStep(t *testing.T) {
	hookCmdIsolate(t)
	dir := planTestDir(t)
	plan := planTestWrite(t, dir, planTestIface, planTestPkg("a", "light"))

	if _, code := hookCmdRun(t, "", "plan", "next", "--plan", plan, "--id", "p1", "--title", "T", "--repo", dir); code != 0 {
		t.Fatalf("plan next exit = %d, want 0", code)
	}
	p, ok := statusfile.Default().ReadPlan("p1")
	if !ok {
		t.Fatal("the first plan next with --id did not write plan p1's status entry")
	}
	if p.Phase != "explore" || p.Title != "T" {
		t.Errorf("plan p1 = phase %q title %q, want explore and T", p.Phase, p.Title)
	}
}

func TestPlanLintExitCodes(t *testing.T) {
	hookCmdIsolate(t)
	cases := []struct {
		name string
		plan func(t *testing.T) string
		args []string
		want int
	}{
		{"clean skeleton", func(t *testing.T) string {
			return planTestWrite(t, planTestDir(t), planTestIface, planTestPkg("a", "light"))
		}, nil, 0},
		{"skeleton warnings", func(t *testing.T) string {
			unused := `{"id":"j","path":"i.txt"}`
			return planTestWrite(t, planTestDir(t), planTestIface+","+unused, planTestPkg("a", "light"))
		}, nil, 1},
		{"invalid skeleton", func(t *testing.T) string {
			return planTestWrite(t, planTestDir(t), planTestIface, planTestPkg("a", "huge"))
		}, nil, 3},
		{"missing spec", func(t *testing.T) string {
			return planTestWrite(t, planTestDir(t), planTestIface, planTestPkg("a", "light"))
		}, []string{"--package", "a"}, 3},
		{"unknown package", func(t *testing.T) string {
			return planTestWrite(t, planTestDir(t), planTestIface, planTestPkg("a", "light"))
		}, []string{"--package", "zz"}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan := c.plan(t)
			args := append([]string{"plan", "lint", "--plan", plan}, c.args...)
			if _, code := hookCmdRun(t, "", args...); code != c.want {
				t.Errorf("plan lint exit = %d, want %d", code, c.want)
			}
		})
	}
}

// planDirRun runs the whole command tree, so the refusal text on stderr is
// visible (hookCmdRun discards it).
func planDirRun(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return out.String(), errb.String(), code
}

// planDirTempRoot makes a fresh directory the one temporary root the tests
// refuse under. The real roots are never seamed: the test directories live
// under $TMPDIR.
func planDirTempRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	saved := paths.TempRootsSeam
	paths.TempRootsSeam = func() []string { return []string{root} }
	t.Cleanup(func() { paths.TempRootsSeam = saved })
	return root
}

// planDirRefusal is the exact text paths.RefuseTemp returns for p under root.
func planDirRefusal(flag, p, root string) string {
	return fmt.Sprintf("%s %s is under %s, which a reboot wipes; keep plans, specs, answers and wave state in the plan directory %s/<plan id>/ (agent-exec plan dir --id <plan id> prints it)", flag, p, root, paths.PlansDir())
}

func TestPlanNextDefaultsPlanFromID(t *testing.T) {
	hookCmdIsolate(t)
	repo := t.TempDir()
	out, _, code := planDirRun(t, "plan", "next", "--id", "p1", "--repo", repo)
	if code != 0 {
		t.Fatalf("plan next exit = %d, want 0", code)
	}
	var step struct {
		Step string `json:"step"`
		Plan string `json:"plan"`
	}
	if err := json.Unmarshal([]byte(out), &step); err != nil {
		t.Fatalf("stdout %q is not the step JSON: %v", out, err)
	}
	if want := filepath.Join(paths.PlanDir("p1"), "plan.json"); step.Plan != want {
		t.Errorf("plan = %q, want %q", step.Plan, want)
	}
	if step.Step != "skeleton" {
		t.Errorf("step = %q, want skeleton", step.Step)
	}
	if fi, err := os.Stat(paths.PlanDir("p1")); err != nil || !fi.IsDir() {
		t.Fatalf("plan directory not created: %v", err)
	}
	if _, _, code := planDirRun(t, "plan", "next", "--id", "p1", "--repo", repo); code != 0 {
		t.Errorf("second plan next --id exit = %d, want 0", code)
	}
}

func TestPlanNextNeedsIDOrPlan(t *testing.T) {
	hookCmdIsolate(t)
	_, stderr, code := planDirRun(t, "plan", "next")
	if code != 2 {
		t.Fatalf("plan next exit = %d, want 2", code)
	}
	want := "agent-exec: plan next: pass --id <plan id> (the plan then lives in " + paths.PlansDir() + "/<plan id>/) or --plan <path>\n"
	if stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
	if _, err := os.Stat(paths.PlansDir()); !os.IsNotExist(err) {
		t.Errorf("plans directory created for a refused call: %v", err)
	}
}

func TestPlanNextRefusesTemp(t *testing.T) {
	root := planDirTempRoot(t)
	hookCmdIsolate(t)
	planUnderRoot := filepath.Join(root, "plan.json")
	answers := filepath.Join(root, "answers")
	if err := os.WriteFile(answers, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		args    []string
		want    string
		created string
	}{
		{"plan under temp root", []string{"plan", "next", "--plan", planUnderRoot},
			planDirRefusal("--plan", planUnderRoot, root), ""},
		{"answers under temp root", []string{"plan", "next", "--id", "p1", "--answers", answers},
			planDirRefusal("--answers", answers, root), paths.PlanDir("p1")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, stderr, code := planDirRun(t, c.args...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2", code)
			}
			if want := "agent-exec: plan next: " + c.want + "\n"; stderr != want {
				t.Errorf("stderr = %q, want %q", stderr, want)
			}
			if c.created != "" {
				if _, err := os.Stat(c.created); !os.IsNotExist(err) {
					t.Errorf("%s created for a refused call: %v", c.created, err)
				}
			}
		})
	}
	if _, _, code := planDirRun(t, "plan", "next", "--id", "a b"); code != 2 {
		t.Errorf("plan next --id 'a b' exit = %d, want 2", code)
	}
	if _, err := os.Stat(paths.PlansDir()); !os.IsNotExist(err) {
		t.Errorf("plans directory created for an invalid id: %v", err)
	}
}

func TestPlanLintRefusesTemp(t *testing.T) {
	root := planDirTempRoot(t)
	hookCmdIsolate(t)
	ifaceUnderRoot := fmt.Sprintf(`{"id":"i","path":%q}`, filepath.Join(root, "i.txt"))
	specUnderRoot := fmt.Sprintf(`{"id":"a","spec":%q,"cls":"light","brief":"b","risks":[],`+
		`"est_seconds":60,"files_owned":["a.go"],"verification":"go test ./a/...","interfaces":["i"]}`, filepath.Join(root, "a.md"))
	write := func(t *testing.T, name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	specPath := filepath.Join(root, "a.md")
	ifacePath := filepath.Join(root, "i.txt")
	preamblePath := filepath.Join(root, "pre.md")
	cases := []struct {
		name string
		plan func(t *testing.T) string
		args []string
		flag string
		path string
	}{
		{"plan under temp root", func(t *testing.T) string {
			return planTestWrite(t, root, planTestIface, planTestPkg("a", "light"))
		}, nil, "--plan", filepath.Join(root, "plan.json")},
		{"spec under temp root", func(t *testing.T) string {
			return planTestWrite(t, planTestDir(t), planTestIface, specUnderRoot)
		}, nil, "spec a", specPath},
		{"spec under temp root with package", func(t *testing.T) string {
			return planTestWrite(t, planTestDir(t), planTestIface, specUnderRoot)
		}, []string{"--package", "a"}, "spec a", specPath},
		{"interface under temp root", func(t *testing.T) string {
			write(t, "i.txt")
			return planTestWrite(t, planTestDir(t), ifaceUnderRoot, planTestPkg("a", "light"))
		}, nil, "interface i", ifacePath},
		{"preamble under temp root", func(t *testing.T) string {
			write(t, "pre.md")
			path := filepath.Join(planTestDir(t), "plan.json")
			text := fmt.Sprintf(`{"commit_message":"feat: x","preamble":[%q],"interfaces":[%s],"packages":[%s]}`,
				preamblePath, planTestIface, planTestPkg("a", "light"))
			if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
			return path
		}, nil, "preamble", preamblePath},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan := c.plan(t)
			args := append([]string{"plan", "lint", "--plan", plan}, c.args...)
			_, stderr, code := planDirRun(t, args...)
			if code != 2 {
				t.Fatalf("plan lint exit = %d, want 2; stderr %q", code, stderr)
			}
			want := "agent-exec: plan lint: " + planDirRefusal(c.flag, c.path, root) + "\n"
			if stderr != want {
				t.Errorf("stderr = %q, want %q", stderr, want)
			}
		})
	}
}

func TestPlanDirPrintsPaths(t *testing.T) {
	hookCmdIsolate(t)
	out, _, code := planDirRun(t, "plan", "dir", "--id", "p1")
	if code != 0 {
		t.Fatalf("plan dir exit = %d, want 0", code)
	}
	if n := strings.Count(out, "\n"); n != 1 || !strings.HasSuffix(out, "\n") {
		t.Fatalf("stdout %q is not one line", out)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout %q is not JSON: %v", out, err)
	}
	d := paths.PlanDir("p1")
	want := map[string]string{
		"answers": filepath.Join(d, "answers"),
		"dir":     d,
		"plan":    filepath.Join(d, "plan.json"),
		"probes":  filepath.Join(d, "probes"),
		"specs":   filepath.Join(d, "specs"),
		"state":   filepath.Join(d, "state.json"),
	}
	if len(got) != len(want) {
		t.Errorf("keys = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
		t.Fatalf("plan directory not created: %v", err)
	}
	for _, name := range []string{"specs", "probes", "answers", "plan.json"} {
		if _, err := os.Stat(filepath.Join(d, name)); err == nil {
			t.Errorf("%s created by plan dir; only the directory itself should be", name)
		}
	}
}

func TestPlanDirRefusesBadID(t *testing.T) {
	hookCmdIsolate(t)
	for _, args := range [][]string{{"plan", "dir"}, {"plan", "dir", "--id", "../x"}} {
		if _, _, code := planDirRun(t, args...); code != 2 {
			t.Errorf("%v exit = %d, want 2", args, code)
		}
	}
	if _, err := os.Stat(paths.PlansDir()); !os.IsNotExist(err) {
		t.Errorf("plans directory created for a bad id: %v", err)
	}
}
