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

// ManagedWorktreeMarkers are the files orchestra keeps in the git dir of a
// worktree it created or adopted (the on-disk contract of isolate/meta.go,
// pinned by a test there). They tell an orchestra worktree from a linked
// worktree the user made.
var ManagedWorktreeMarkers = []string{"orchestra-baseline", "orchestra-adopted", "orchestra-role"}

// ProjectBase is the directory whose .claude/ holds the project layers. An
// existing $ORCHESTRA_CONFIG_ROOT wins; otherwise the nearest directory from
// the cwd up to the git toplevel (inclusive) that holds a project layer file;
// otherwise the cwd. In a worktree orchestra created or adopted (an
// integration or package tree) the walk runs in the user's own checkout the
// worktree belongs to, not in the worktree: the untracked, per-machine
// .claude/orchestra.local.yaml exists only there.
func ProjectBase() string {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	return ProjectBaseAt(cwd)
}

// ProjectBaseAt is ProjectBase for dir instead of the cwd.
func ProjectBaseAt(dir string) string {
	if forced := os.Getenv("ORCHESTRA_CONFIG_ROOT"); forced != "" {
		if st, err := os.Stat(forced); err == nil && st.IsDir() {
			if abs, err := filepath.Abs(forced); err == nil {
				return abs
			}
		}
	}
	cwd := dir
	if abs, err := filepath.Abs(dir); err == nil {
		cwd = abs
	}
	top := gitToplevel(cwd)
	if top == "" {
		return cwd
	}
	top = paths.RealPath(top)
	current := paths.RealPath(cwd)
	if origin := originCheckout(top); origin != "" {
		// the same place of the user's checkout (its root when the
		// worktree's subdirectory does not exist there)
		mapped := origin
		if rel, err := filepath.Rel(top, current); err == nil && !strings.HasPrefix(rel, "..") {
			if p := filepath.Join(origin, rel); isDir(p) {
				mapped = p
			}
		}
		top, current, cwd = origin, mapped, mapped
	}
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
		if parent == current || !withinDir(current, top) {
			break
		}
		current = parent
	}
	return cwd
}

// originCheckout is the main working tree a worktree orchestra created or
// adopted belongs to; "" for the main tree itself, for a linked worktree
// without orchestra's markers (the user's own, with its own layers) and when
// the main tree cannot be told (a bare repository).
func originCheckout(top string) string {
	gitDir := gitOut(top, "rev-parse", "--absolute-git-dir")
	common := gitOut(top, "rev-parse", "--git-common-dir")
	if gitDir == "" || common == "" {
		return ""
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(top, common)
	}
	if paths.RealPath(common) == paths.RealPath(gitDir) {
		return ""
	}
	managed := false
	for _, m := range ManagedWorktreeMarkers {
		if _, err := os.Stat(filepath.Join(gitDir, m)); err == nil {
			managed = true
			break
		}
	}
	if !managed {
		return ""
	}
	// the first entry of the list is the main working tree
	main := ""
	for _, line := range strings.Split(gitOut(top, "worktree", "list", "--porcelain"), "\n") {
		if strings.HasPrefix(line, "worktree ") {
			main = paths.RealPath(strings.TrimSpace(strings.TrimPrefix(line, "worktree ")))
			break
		}
	}
	if main == "" {
		return ""
	}
	if main == top {
		return ""
	}
	if _, err := os.Stat(filepath.Join(main, ".git")); err != nil {
		return ""
	}
	return main
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func gitToplevel(cwd string) string { return gitOut(cwd, "rev-parse", "--show-toplevel") }

// gitOut is the trimmed stdout of a git command in dir, "" on failure.
func gitOut(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
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
		if err := checkRemovedDashboard(l.Path, data); err != nil {
			return nil, err
		}
		if err := checkRemovedFanoutKeys(l.Path, data); err != nil {
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
	if err := validateLand(values); err != nil {
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

// withinDir reports whether p is dir or lies below it (case-insensitive on
// Windows, where Rel compares so).
func withinDir(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
