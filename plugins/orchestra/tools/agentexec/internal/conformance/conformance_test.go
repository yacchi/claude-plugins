// Package conformance replays the CLI contract scenarios under
// testdata/conformance against an agent-exec implementation and compares
// the normalized result with the recorded golden.
//
//	go test ./internal/conformance                      # Go vs goldens
//	go test ./internal/conformance -impl=go -update     # rewrite goldens (add a CHANGES.md line)
//	go test ./internal/conformance -run 'TestConformance/config'
//
// The goldens were first recorded from the retired Python implementation;
// since the cutover Go is the only implementation and the goldens are Go's.
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
	implFlag   = flag.String("impl", "go", "implementation under test (go is the only one)")
	updateFlag = flag.Bool("update", false, "rewrite the goldens from this run")
)

var env struct {
	git   string
	fakes map[string]string
	impl  []string
}

const (
	testdataDir = "../../testdata/conformance"
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

// recordSources opens every file the binaries under test are built from.
// They are built in a subprocess, which go test's result cache cannot see;
// opening the sources makes them part of the cache key, so an edit re-runs
// the scenarios.
func recordSources() error {
	const dir = "../.."
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != dir && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".go", ".mod", ".sum", ".html":
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			return f.Close()
		}
		return nil
	})
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
	if *implFlag != "go" {
		return dir, fmt.Errorf("-impl must be go (the only implementation), not %q", *implFlag)
	}
	bin := filepath.Join(dir, "agent-exec")
	if err := build("./cmd/agent-exec", bin); err != nil {
		return dir, err
	}
	env.impl = []string{bin}
	return dir, nil
}

func TestConformance(t *testing.T) {
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
				writeGolden(t, sc.GoldenPath(), bakeGoDeviations(t, sc, got))
				return
			}
			want := readGolden(t, sc.GoldenPath())
			compare(t, sc, want, got)
		})
	}
}

func runScenario(t *testing.T, sc *Scenario) Golden {
	s := newSandbox(t, sc)
	n := newNormalizer(s, sc.NormalizeKeys, sc.NormalizeText)
	g := Golden{Scenario: sc.ID()}
	vars := map[string]string{}
	for _, st := range sc.Steps {
		argv := st.Argv
		if len(st.ArgvGo) > 0 {
			argv = st.ArgvGo
		}
		raw := s.run(t, env.impl, argv, st, vars)
		for name, key := range st.Save {
			vars[name] = savedValue(t, raw.stdout, key)
		}
		s.snapshot(st, vars, &raw)
		g.Steps = append(g.Steps, n.step(st, raw))
	}
	return g
}

// savedValue is the string at key in the step's JSON stdout (its last line,
// the line a caller parses).
func savedValue(t *testing.T, stdout, key string) string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	var doc map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &doc); err != nil {
		t.Fatalf("save %s: stdout is not a JSON object: %q", key, stdout)
	}
	v, ok := doc[key].(string)
	if !ok {
		t.Fatalf("save %s: no string %q in %q", key, key, stdout)
	}
	return v
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
		t.Fatalf("missing golden (capture with -impl=go -update): %v", err)
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
		for _, msg := range applyGoDeviations(st, &w, &g) {
			t.Errorf("%s: %s", where, msg)
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
