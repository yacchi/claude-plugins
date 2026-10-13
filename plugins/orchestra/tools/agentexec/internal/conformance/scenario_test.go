package conformance

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Scenario is one testdata/conformance/<area>/<name>.yaml file.
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
//	normalize_keys: [elapsed_s]  # JSON keys whose values are masked
//	steps:
//	  - argv: [config, --json]
//	    cwd: sub/dir             # relative to WORK ("~" or "~/x" for HOME)
//	    stdin: "..."
//	    env: {K: V}
//	    stderr: exact            # exact (default) | ignore | prefix
//	    stderr_prefix: "agent-exec: invalid YAML in <WORK>/"
//	    snapshot: ["~/.claude/orchestra.yaml"]  # files captured after the step
//
// <ROOT>, <HOME> and <WORK> are substituted in file contents, env values and
// cwd.
type Scenario struct {
	Description   string   `yaml:"description"`
	Platforms     []string `yaml:"platforms"`
	Setup         Setup    `yaml:"setup"`
	NormalizeKeys []string `yaml:"normalize_keys"`
	Steps         []Step   `yaml:"steps"`

	area, name, path string
}

type Setup struct {
	Git                bool              `yaml:"git"`
	Files              map[string]string `yaml:"files"`
	Dirty              map[string]string `yaml:"dirty"`
	Modes              map[string]string `yaml:"modes"`
	Env                map[string]string `yaml:"env"`
	Fakes              []string          `yaml:"fakes"`
	KeepSandboxDefault bool              `yaml:"keep_sandbox_default"`
}

type Step struct {
	Argv         []string          `yaml:"argv"`
	Cwd          string            `yaml:"cwd"`
	Stdin        string            `yaml:"stdin"`
	Env          map[string]string `yaml:"env"`
	Stderr       string            `yaml:"stderr"`
	StderrPrefix string            `yaml:"stderr_prefix"`
	Snapshot     []string          `yaml:"snapshot"`
	// GoSwap / GoReplace record intentional Go-only deviations from the
	// Python golden (each also has a line in CHANGES.md); they apply only
	// under -impl=go. See applyGoDeviations.
	GoSwap    *GoSwap     `yaml:"go_swap"`
	GoReplace []GoReplace `yaml:"go_replace"`
}

// GoSwap: the golden's top-level key (JSON stdout) or "<key>: " lines (text
// stdout) named Golden is replaced in Go by Got, whose value is volatile and
// so only checked for shape: a JSON object with exactly Keys, or, in text, one
// "<got>: " line carrying every "<key>=".
type GoSwap struct {
	Golden string   `yaml:"golden"`
	Got    string   `yaml:"got"`
	Keys   []string `yaml:"keys"`
}

// GoReplace rewrites the golden's text stdout (Old -> New) before comparing.
type GoReplace struct {
	Old string `yaml:"old"`
	New string `yaml:"new"`
}

func (s *Scenario) ID() string { return s.area + "/" + s.name }

func (s *Scenario) GoldenPath() string {
	return strings.TrimSuffix(s.path, ".yaml") + ".golden.json"
}

func (s *Scenario) runsHere() bool {
	if len(s.Platforms) == 0 {
		return true
	}
	for _, p := range s.Platforms {
		if p == runtime.GOOS {
			return true
		}
	}
	return false
}

func loadScenarios(root string) ([]*Scenario, error) {
	matches, err := filepath.Glob(filepath.Join(root, "*", "*.yaml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	var out []*Scenario
	for _, path := range matches {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		sc := &Scenario{}
		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		dec.KnownFields(true)
		if err := dec.Decode(sc); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if len(sc.Steps) == 0 {
			return nil, fmt.Errorf("%s: no steps", path)
		}
		for i, st := range sc.Steps {
			switch st.Stderr {
			case "", "exact", "ignore":
			case "prefix":
				if st.StderrPrefix == "" {
					return nil, fmt.Errorf("%s: step %d: stderr: prefix needs stderr_prefix", path, i)
				}
			default:
				return nil, fmt.Errorf("%s: step %d: unknown stderr mode %q", path, i, st.Stderr)
			}
		}
		sc.path = path
		sc.area = filepath.Base(filepath.Dir(path))
		sc.name = strings.TrimSuffix(filepath.Base(path), ".yaml")
		out = append(out, sc)
	}
	return out, nil
}
