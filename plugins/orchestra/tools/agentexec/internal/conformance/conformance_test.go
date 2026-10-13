// Package conformance replays the CLI contract scenarios under
// testdata/conformance against an agent-exec implementation and compares
// the normalized result with the recorded golden.
//
//	go test ./internal/conformance                       # Go vs goldens
//	go test ./internal/conformance -impl=python          # Python vs goldens (make verify-goldens)
//	go test ./internal/conformance -impl=python -update  # (re)capture goldens from Python
//	go test ./internal/conformance -run 'TestConformance/config'
package conformance

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

var (
	implFlag   = flag.String("impl", "go", "implementation under test: go | python")
	updateFlag = flag.Bool("update", false, "rewrite the goldens from this run")
)

// goldensFromPython: until the cutover the goldens are the Python
// implementation's behavior, so only -impl=python may rewrite them.
const goldensFromPython = true

var env struct {
	git   string
	fakes map[string]string
	impl  []string
}

const (
	testdataDir = "../../testdata/conformance"
	toolsDir    = "../../.." // plugins/orchestra/tools
)

func TestMain(m *testing.M) {
	flag.Parse()
	code, err := setupEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "conformance:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(code)
	os.Exit(m.Run())
}

// recordSources opens every file the implementations under test are built
// from. The binaries are built in a subprocess, which go test's result cache
// cannot see; opening the sources makes them part of the cache key, so an
// edit to either implementation re-runs the scenarios.
func recordSources() error {
	for _, dir := range []string{"../..", toolsDir} {
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if path != dir && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".") ||
					(dir == toolsDir && path != dir)) {
					return filepath.SkipDir
				}
				return nil
			}
			switch filepath.Ext(path) {
			case ".go", ".py", ".mod", ".sum":
				f, err := os.Open(path)
				if err != nil {
					return err
				}
				return f.Close()
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// setupEnv builds the binaries the scenarios need and returns the build dir.
func setupEnv() (string, error) {
	git, err := exec.LookPath("git")
	if err != nil {
		return "", fmt.Errorf("git not found: %w", err)
	}
	env.git = git
	dir, err := os.MkdirTemp("", "agentexec-conformance-")
	if err != nil {
		return "", err
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		goBin = filepath.Join(runtime.GOROOT(), "bin", "go")
	}
	build := func(pkg, out string) error {
		cmd := exec.Command(goBin, "build", "-o", out, pkg)
		cmd.Dir = "../.."
		if b, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("go build %s: %v\n%s", pkg, err, b)
		}
		return nil
	}
	fakepi := filepath.Join(dir, "fakepi")
	if err := build("./internal/testutil/fakepi", fakepi); err != nil {
		return dir, err
	}
	env.fakes = map[string]string{"pi": fakepi}
	switch *implFlag {
	case "go":
		bin := filepath.Join(dir, "agent-exec")
		if err := build("./cmd/agent-exec", bin); err != nil {
			return dir, err
		}
		env.impl = []string{bin}
	case "python":
		py, err := pythonWithYAML()
		if err != nil {
			return dir, err
		}
		script, err := filepath.Abs(filepath.Join(toolsDir, "agent_exec.py"))
		if err != nil {
			return dir, err
		}
		env.impl = []string{py, script}
	default:
		return dir, fmt.Errorf("-impl must be go or python, not %q", *implFlag)
	}
	return dir, nil
}

// pythonWithYAML asks uv (with the caller's real environment, so its cache
// is reused) for an interpreter that has PyYAML, the Python implementation's
// only dependency. Steps then run that interpreter directly, hermetically.
func pythonWithYAML() (string, error) {
	cmd := exec.Command("uv", "run", "--quiet", "--no-project", "--with", "pyyaml",
		"python", "-c", "import sys, yaml; print(sys.executable)")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("uv run python: %v\n%s", err, stderr.String())
	}
	return strings.TrimSpace(string(out)), nil
}

func TestConformance(t *testing.T) {
	if *updateFlag && *implFlag != "python" && goldensFromPython {
		t.Fatal("-update is only allowed with -impl=python until the cutover")
	}
	// Inside the test (not TestMain) so go test records the opens.
	must(t, recordSources())
	scenarios, err := loadScenarios(testdataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(scenarios) == 0 {
		t.Fatal("no scenarios found")
	}
	for _, sc := range scenarios {
		t.Run(sc.ID(), func(t *testing.T) {
			if !sc.runsHere() {
				t.Skipf("only on %v", sc.Platforms)
			}
			got := runScenario(t, sc)
			if *updateFlag {
				writeGolden(t, sc.GoldenPath(), got)
				return
			}
			want := readGolden(t, sc.GoldenPath())
			compare(t, sc, want, got)
		})
	}
}

func runScenario(t *testing.T, sc *Scenario) Golden {
	s := newSandbox(t, sc)
	n := newNormalizer(s, sc.NormalizeKeys)
	g := Golden{Scenario: sc.ID()}
	for _, st := range sc.Steps {
		g.Steps = append(g.Steps, n.step(st, s.run(t, env.impl, st)))
	}
	return g
}

func writeGolden(t *testing.T, path string, g Golden) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	must(t, enc.Encode(g))
	must(t, os.WriteFile(path, buf.Bytes(), 0o644))
}

func readGolden(t *testing.T, path string) Golden {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden (capture with -impl=python -update): %v", err)
	}
	var g Golden
	must(t, json.Unmarshal(b, &g))
	return g
}

// compare checks got against want through a JSON round trip, so both sides
// hold the same Go types (float64 numbers: 1.0 and 1 are equal).
func compare(t *testing.T, sc *Scenario, want, got Golden) {
	var gotRT Golden
	b, err := json.Marshal(got)
	must(t, err)
	must(t, json.Unmarshal(b, &gotRT))
	if len(want.Steps) != len(gotRT.Steps) {
		t.Fatalf("golden has %d steps, scenario ran %d (re-capture?)", len(want.Steps), len(gotRT.Steps))
	}
	for i, w := range want.Steps {
		g := gotRT.Steps[i]
		st := sc.Steps[i]
		where := fmt.Sprintf("step %d (%s)", i, describe(st.Argv))
		if !reflect.DeepEqual(w.Argv, g.Argv) {
			t.Errorf("%s: golden argv %v differs from the scenario (re-capture?)", where, w.Argv)
			continue
		}
		if *implFlag == "go" {
			for _, msg := range applyGoDeviations(st, &w, &g) {
				t.Errorf("%s: %s", where, msg)
			}
		}
		if w.Exit != g.Exit {
			t.Errorf("%s: exit %d, want %d\nstderr: %s", where, g.Exit, w.Exit, g.Stderr)
		}
		if w.StdoutShape != g.StdoutShape {
			t.Errorf("%s: stdout shape %s, want %s", where, g.StdoutShape, w.StdoutShape)
		}
		if !reflect.DeepEqual(w.Stdout, g.Stdout) {
			t.Errorf("%s: stdout differs\n%s", where, diffJSON(w.Stdout, g.Stdout))
		}
		switch st.Stderr {
		case "ignore":
		case "prefix":
			for _, side := range []struct{ name, text string }{{"golden", w.Stderr}, {"got", g.Stderr}} {
				if !strings.HasPrefix(side.text, st.StderrPrefix) {
					t.Errorf("%s: %s stderr %q lacks prefix %q", where, side.name, side.text, st.StderrPrefix)
				}
			}
		default:
			if w.Stderr != g.Stderr {
				t.Errorf("%s: stderr\n got %q\nwant %q", where, g.Stderr, w.Stderr)
			}
		}
		if !reflect.DeepEqual(w.Files, g.Files) {
			t.Errorf("%s: files differ\n%s", where, diffJSON(w.Files, g.Files))
		}
	}
}

// diffJSON lists the paths where want and got differ.
func diffJSON(want, got any) string {
	var lines []string
	var walk func(path string, w, g any)
	walk = func(path string, w, g any) {
		wm, wok := w.(map[string]any)
		gm, gok := g.(map[string]any)
		if wok && gok {
			keys := map[string]bool{}
			for k := range wm {
				keys[k] = true
			}
			for k := range gm {
				keys[k] = true
			}
			for _, k := range sortedKeys(keys) {
				wv, win := wm[k]
				gv, gin := gm[k]
				switch {
				case !win:
					lines = append(lines, fmt.Sprintf("  %s.%s: unexpected %s", path, k, short(gv)))
				case !gin:
					lines = append(lines, fmt.Sprintf("  %s.%s: missing (want %s)", path, k, short(wv)))
				default:
					walk(path+"."+k, wv, gv)
				}
			}
			return
		}
		wl, wok := w.([]any)
		gl, gok := g.([]any)
		if wok && gok && len(wl) == len(gl) {
			for i := range wl {
				walk(fmt.Sprintf("%s[%d]", path, i), wl[i], gl[i])
			}
			return
		}
		if !reflect.DeepEqual(w, g) {
			lines = append(lines, fmt.Sprintf("  %s: got %s, want %s", path, short(g), short(w)))
		}
	}
	walk("$", want, got)
	return strings.Join(lines, "\n")
}

func short(v any) string {
	b, _ := json.Marshal(v)
	if len(b) > 300 {
		return string(b[:300]) + "..."
	}
	return string(b)
}
