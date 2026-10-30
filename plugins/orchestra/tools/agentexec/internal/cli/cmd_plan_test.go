package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	home := os.Getenv("HOME")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "orchestra"), []byte("x"), 0o644); err != nil {
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
