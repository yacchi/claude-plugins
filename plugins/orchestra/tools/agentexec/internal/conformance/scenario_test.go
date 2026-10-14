package conformance

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Scenario is one testdata/conformance/<area>/<name>.yaml file; the
// fields are described in the package comment (conformance_test.go).
type Scenario struct {
	Description   string     `yaml:"description"`
	Platforms     []string   `yaml:"platforms"`
	Setup         Setup      `yaml:"setup"`
	NormalizeKeys []string   `yaml:"normalize_keys"`
	NormalizeText []TextRule `yaml:"normalize_text"`
	Steps         []Step     `yaml:"steps"`

	area, name, path string
}

// TextRule is one normalize_text rewrite (Go regexp syntax; $1 in Replace).
type TextRule struct {
	Pattern string `yaml:"pattern"`
	Replace string `yaml:"replace"`
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
	Argv           []string          `yaml:"argv"`
	Cwd            string            `yaml:"cwd"`
	Stdin          string            `yaml:"stdin"`
	Env            map[string]string `yaml:"env"`
	StderrContains []string          `yaml:"stderr_contains"`
	Snapshot       []string          `yaml:"snapshot"`
	Save           map[string]string `yaml:"save"`
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
		for _, r := range sc.NormalizeText {
			if _, err := regexp.Compile(r.Pattern); err != nil {
				return nil, fmt.Errorf("%s: normalize_text: %w", path, err)
			}
		}
		if len(sc.Steps) == 0 {
			return nil, fmt.Errorf("%s: no steps", path)
		}
		sc.path = path
		sc.area = filepath.Base(filepath.Dir(path))
		sc.name = strings.TrimSuffix(filepath.Base(path), ".yaml")
		out = append(out, sc)
	}
	return out, nil
}
