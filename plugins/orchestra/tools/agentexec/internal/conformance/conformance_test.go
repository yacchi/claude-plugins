// Package conformance replays the agent-exec CLI contract scenarios under
// testdata/conformance against the built binary and compares the normalized
// result with the recorded golden (<area>/<name>.golden.json beside each
// <area>/<name>.yaml).
//
//	go test ./internal/conformance                       # check the goldens
//	go test ./internal/conformance -update               # rewrite every golden
//	go test ./internal/conformance -run 'TestConformance/config' [-update]
//
// Compared per step: the argv, the exit code (exactly), stdout and the
// snapshot files. JSON (one document, or JSON lines) is compared as values:
// key order, whitespace and number spelling (1, 1.0, 1e0) do not matter, but
// field presence and null vs absent do. Any other text is compared exactly
// after normalization. stderr is not recorded; a step pins the part of it
// that is a contract with stderr_contains. After an intended output change,
// rerun with -update and review the golden diff.
//
// A scenario file:
//
//	description: what the scenario pins
//	platforms: [darwin]          # optional; skipped elsewhere
//	setup:
//	  git: true                  # git init <WORK> and commit `files`
//	  files:                     # written before the commit; "~/x" is under
//	    .claude/orchestra.yaml: | #   HOME, anything else under WORK
//	      tiers: {light: sonnet}
//	  dirty: {src/a.txt: "x\n"}  # written after the commit
//	  modes: {bin/tool: "0755"}  # chmod after writing
//	  env: {ORCHESTRA_CONFIG_ROOT: "<WORK>/other"}
//	  fakes: [pi]                # fake executors on PATH
//	  keep_sandbox_default: true # skip the harness's `sandbox: {mode: off}` user layer
//	normalize_keys: [elapsed_s]  # JSON keys whose non-null values are masked
//	normalize_text:              # regex rewrites of every string (after the
//	  - pattern: '"seconds": [0-9.e-]+'   #   built-in masks), e.g. durations
//	    replace: '"seconds": <S>'         #   inside JSON-in-a-string details
//	steps:
//	  - argv: [config, --json]
//	    cwd: sub/dir             # relative to WORK ("~" or "~/x" for HOME)
//	    stdin: "..."
//	    env: {K: V}
//	    stderr_contains: ["agent-exec: invalid YAML in <WORK>/"]  # normalized stderr
//	    snapshot: ["~/.claude/orchestra.yaml"]  # files captured after the step
//	    save: {TOK: token}       # top-level stdout JSON key later steps use as <@TOK>
//
// <ROOT>, <HOME> and <WORK> are substituted in file contents, env values and
// cwd; <@NAME> (a saved value) in argv, stdin, env values and snapshot
// paths. Every step also sees AGENT_EXEC_UNDER_TEST, an executable running
// the binary under test, for a PATH shim that must reach it
// (`exec "$AGENT_EXEC_UNDER_TEST" "$@"`: a detached child re-runs the shim).
// Normalization (see normalizer) masks the sandbox paths, dispatch tokens,
// uuids, SHAs, timestamps and the pid/port keys.
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

var updateFlag = flag.Bool("update", false, "rewrite the goldens from this run")

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
				writeGolden(t, sc.GoldenPath(), got)
				return
			}
			want := readGolden(t, sc.GoldenPath())
			compare(t, sc, want, got)
		})
	}
}

// runScenario runs every step and returns the normalized result. Each
// step's stderr_contains is checked here, so -update cannot record a run
// that breaks it.
func runScenario(t *testing.T, sc *Scenario) Golden {
	s := newSandbox(t, sc)
	n := newNormalizer(s, sc.NormalizeKeys, sc.NormalizeText)
	g := Golden{Scenario: sc.ID()}
	vars := map[string]string{}
	for i, st := range sc.Steps {
		raw := s.run(t, env.impl, st.Argv, st, vars)
		if len(st.StderrContains) > 0 {
			stderr := n.text(raw.stderr)
			for _, want := range st.StderrContains {
				if !strings.Contains(stderr, want) {
					t.Errorf("step %d (%s): stderr lacks %q\nstderr: %s", i, describe(st.Argv), want, stderr)
				}
			}
		}
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
		t.Fatalf("missing golden (capture with -update): %v", err)
	}
	var g Golden
	must(t, json.Unmarshal(b, &g))
	return g
}

// compare checks got against want through a JSON round trip, so both sides
// hold the same Go types (float64 numbers: 1, 1.0 and 1e0 are equal).
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
		where := fmt.Sprintf("step %d (%s)", i, describe(sc.Steps[i].Argv))
		if !reflect.DeepEqual(w.Argv, g.Argv) {
			t.Errorf("%s: golden argv %v differs from the scenario (re-capture?)", where, w.Argv)
			continue
		}
		if w.Exit != g.Exit {
			t.Errorf("%s: exit %d, want %d", where, g.Exit, w.Exit)
		}
		if w.StdoutShape != g.StdoutShape {
			t.Errorf("%s: stdout shape %s, want %s", where, g.StdoutShape, w.StdoutShape)
		}
		if !reflect.DeepEqual(w.Stdout, g.Stdout) {
			t.Errorf("%s: stdout differs\n%s", where, diffJSON(w.Stdout, g.Stdout))
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
		ws, wok := w.(string)
		gs, gok := g.(string)
		if wok && gok && ws != gs && (strings.Contains(ws, "\n") || strings.Contains(gs, "\n")) {
			lines = append(lines, fmt.Sprintf("  %s: text differs\n%s", path, diffLines(ws, gs)))
			return
		}
		if !reflect.DeepEqual(w, g) {
			lines = append(lines, fmt.Sprintf("  %s: got %s, want %s", path, short(g), short(w)))
		}
	}
	walk("$", want, got)
	return strings.Join(lines, "\n")
}

// diffLines shows where two texts first differ, with a little context.
func diffLines(want, got string) string {
	wl := strings.Split(want, "\n")
	gl := strings.Split(got, "\n")
	i := 0
	for i < len(wl) && i < len(gl) && wl[i] == gl[i] {
		i++
	}
	var b strings.Builder
	for j := max(0, i-2); j < i; j++ {
		fmt.Fprintf(&b, "      %s\n", wl[j])
	}
	for j := i; j < min(len(wl), i+3); j++ {
		fmt.Fprintf(&b, "    - %s\n", wl[j])
	}
	for j := i; j < min(len(gl), i+3); j++ {
		fmt.Fprintf(&b, "    + %s\n", gl[j])
	}
	return strings.TrimRight(b.String(), "\n")
}

func short(v any) string {
	b, _ := json.Marshal(v)
	if len(b) > 300 {
		return string(b[:300]) + "..."
	}
	return string(b)
}
