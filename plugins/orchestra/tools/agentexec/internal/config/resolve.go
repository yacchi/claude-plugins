// Package config resolves orchestra's layered configuration: built-in
// defaults, then ~/.claude/orchestra.yaml, the project's
// .claude/orchestra.yaml and .claude/orchestra.local.yaml (each also as
// .yml), deep-merged in that order and normalized.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// Layer is one config file that took part in a resolution.
type Layer struct {
	Scope string // user | project | local
	Path  string
}

// Resolved is the merged, normalized configuration. Root is the project base
// directory whose .claude/ held the project layers.
type Resolved struct {
	Values map[string]any
	Layers []Layer
	Root   string
}

var projectFiles = []string{
	"orchestra.yaml", "orchestra.yml", "orchestra.local.yaml", "orchestra.local.yml",
}

// ProjectBase is the directory whose .claude/ holds the project layers. An
// existing $ORCHESTRA_CONFIG_ROOT wins; otherwise the nearest directory from
// the cwd up to the git toplevel (inclusive) that holds a project layer file;
// otherwise the cwd.
func ProjectBase() string {
	if forced := os.Getenv("ORCHESTRA_CONFIG_ROOT"); forced != "" {
		if st, err := os.Stat(forced); err == nil && st.IsDir() {
			if abs, err := filepath.Abs(forced); err == nil {
				return abs
			}
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	top := gitToplevel(cwd)
	if top == "" {
		return cwd
	}
	top = paths.RealPath(top)
	current := paths.RealPath(cwd)
	for {
		for _, name := range projectFiles {
			if isFile(filepath.Join(current, ".claude", name)) {
				return current
			}
		}
		if current == top {
			break
		}
		parent := filepath.Dir(current)
		if parent == current || !strings.HasPrefix(current+"/", top+"/") {
			break
		}
		current = parent
	}
	return cwd
}

func gitToplevel(cwd string) string {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = cwd
	cmd.WaitDelay = 10 * time.Second
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

func firstFile(dir string, names ...string) string {
	for _, n := range names {
		if p := filepath.Join(dir, n); isFile(p) {
			return p
		}
	}
	return ""
}

// layerPaths lists the layer files lowest precedence first. A file reached
// through two logical layers (cwd is $HOME, so ./.claude is ~/.claude) is
// kept once, at its first position.
func layerPaths(base string) []Layer {
	var raw []Layer
	if p := firstFile(filepath.Join(paths.Home(), ".claude"), "orchestra.yaml", "orchestra.yml"); p != "" {
		raw = append(raw, Layer{Scope: "user", Path: p})
	}
	projectDir := filepath.Join(base, ".claude")
	if p := firstFile(projectDir, "orchestra.yaml", "orchestra.yml"); p != "" {
		raw = append(raw, Layer{Scope: "project", Path: p})
	}
	if p := firstFile(projectDir, "orchestra.local.yaml", "orchestra.local.yml"); p != "" {
		raw = append(raw, Layer{Scope: "local", Path: p})
	}
	seen := map[string]bool{}
	var out []Layer
	for _, l := range raw {
		key := paths.RealPath(l.Path)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, l)
	}
	return out
}

// Resolve reads and merges every layer. An unreadable or invalid layer is an
// error naming the file. The `available` flag of each CLI executor is not set
// here (it needs tool resolution, which sits above config); see
// tools.MarkAvailability.
func Resolve() (*Resolved, error) {
	base := ProjectBase()
	layers := layerPaths(base)
	values := Defaults()
	u := newUnion()
	for _, l := range layers {
		data, err := loadLayer(l.Path)
		if err != nil {
			return nil, err
		}
		u.add(data)
		values = DeepMerge(values, data)
	}
	normalize(values, u)
	if err := validateTiers(values); err != nil {
		return nil, err
	}
	if err := validatePnpmGVS(values); err != nil {
		return nil, err
	}
	if err := validateDashboard(values); err != nil {
		return nil, err
	}
	return &Resolved{Values: values, Layers: layers, Root: base}, nil
}

func loadLayer(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("invalid YAML in %s: %v", path, err)
	}
	v, err := loadYAML(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid YAML in %s: %v", path, err)
	}
	switch x := v.(type) {
	case nil:
		return map[string]any{}, nil
	case map[string]any:
		return x, nil
	}
	return nil, fmt.Errorf("invalid YAML in %s: the top level must be a mapping", path)
}

// loadYAML parses a single YAML document (yaml.v3 semantics) into the JSON
// value model the rest of config works on: objects map[string]any, arrays
// []any, integers int64. An empty document is nil.
func loadYAML(data []byte) (any, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, err
	}
	keepTimestampsAsText(&doc)
	var v any
	if err := doc.Decode(&v); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return nil, errors.New("expected a single document in the stream")
	} else if !errors.Is(err, io.EOF) {
		return nil, err
	}
	return jsonValue(v), nil
}

// keepTimestampsAsText retags unquoted timestamp scalars (2024-01-01) as
// strings, so they load as the text the author wrote instead of a time.Time
// that would be re-rendered as RFC 3339.
func keepTimestampsAsText(n *yaml.Node) {
	if n.Kind == yaml.ScalarNode && n.ShortTag() == "!!timestamp" {
		n.Tag = "!!str"
	}
	for _, c := range n.Content {
		keepTimestampsAsText(c)
	}
}

func jsonValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = jsonValue(e)
		}
		return x
	case map[any]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[yamlKey(k)] = jsonValue(e)
		}
		return out
	case []any:
		for i, e := range x {
			x[i] = jsonValue(e)
		}
		return x
	case int:
		return int64(x)
	case uint64:
		return float64(x)
	}
	return v
}

// yamlKey renders a non-string mapping key the way JSON object keys spell
// the same scalar.
func yamlKey(k any) string {
	switch x := k.(type) {
	case string:
		return x
	case nil:
		return "null"
	}
	return fmt.Sprint(k)
}
