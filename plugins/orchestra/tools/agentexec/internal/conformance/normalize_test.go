package conformance

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// GoldenStep is the normalized record of one step. Stdout is stored by
// shape: "empty", "json" (one document), "jsonl" (one document per line) or
// "text". JSON is stored decoded and compared as values, so key order,
// indentation and number spelling do not matter. stderr is not recorded:
// a step pins the parts of it that are a contract with stderr_contains.
type GoldenStep struct {
	Argv        []string       `json:"argv"`
	Exit        int            `json:"exit"`
	StdoutShape string         `json:"stdout_shape"`
	Stdout      any            `json:"stdout"`
	Files       map[string]any `json:"files,omitempty"`
}

type Golden struct {
	Scenario string       `json:"scenario"`
	Steps    []GoldenStep `json:"steps"`
}

var (
	reToken   = regexp.MustCompile(`dsp-[0-9a-f]{12}`)
	reRequest = regexp.MustCompile(`\b\d{8}-00000000\b`)
	reUUID    = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	reSHA     = regexp.MustCompile(`\b[0-9a-f]{40}\b`)
	reTS      = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?`)
)

// normalizer masks what legitimately differs between runs: the sandbox
// paths (realpath'd /private/var spellings included), dispatch tokens,
// request ids (the date changes with the calendar; the fixed session
// makes the suffix constant), uuids and SHAs (numbered by first
// appearance, so equal values stay equal), timestamps, and the values of
// volatile JSON keys (an object value keeps its keys and has every leaf
// masked, so its shape is still compared).
type normalizer struct {
	paths    [][2]string
	numbered map[string]map[string]string
	keys     map[string]bool
	rules    []textRule
}

type textRule struct {
	re      *regexp.Regexp
	replace string
}

func newNormalizer(s *sandbox, keys []string, rules []TextRule) *normalizer {
	n := &normalizer{numbered: map[string]map[string]string{}, keys: map[string]bool{}}
	for _, r := range rules {
		n.rules = append(n.rules, textRule{regexp.MustCompile(r.Pattern), r.Replace})
	}
	for _, k := range append([]string{"pid", "port"}, keys...) {
		n.keys[k] = true
	}
	add := func(from, to string) {
		n.paths = append(n.paths, [2]string{from, to})
		if real, err := filepath.EvalSymlinks(from); err == nil && real != from {
			n.paths = append(n.paths, [2]string{real, to})
		}
	}
	add(s.home, "<HOME>")
	add(s.work, "<WORK>")
	add(s.root, "<ROOT>")
	// The unresolved spelling is registered too, so a path that never went
	// through EvalSymlinks does not leak into a golden.
	add(s.wt, "<WT>")
	add(filepath.Join(s.home, ".local", "state", "claude-orchestra", "worktrees", filepath.Base(s.wt)), "<WT>")
	sort.SliceStable(n.paths, func(i, j int) bool { return len(n.paths[i][0]) > len(n.paths[j][0]) })
	return n
}

func (n *normalizer) number(kind, v string) string {
	m := n.numbered[kind]
	if m == nil {
		m = map[string]string{}
		n.numbered[kind] = m
	}
	if out, ok := m[v]; ok {
		return out
	}
	out := fmt.Sprintf("<%s#%d>", kind, len(m)+1)
	m[v] = out
	return out
}

func (n *normalizer) text(s string) string {
	for _, p := range n.paths {
		s = strings.ReplaceAll(s, p[0], p[1])
	}
	s = reToken.ReplaceAllStringFunc(s, func(v string) string { return n.number("TOKEN", v) })
	s = reRequest.ReplaceAllString(s, "<DATE>-00000000")
	s = reUUID.ReplaceAllStringFunc(s, func(v string) string { return n.number("UUID", v) })
	s = reSHA.ReplaceAllStringFunc(s, func(v string) string { return n.number("SHA", v) })
	s = reTS.ReplaceAllString(s, "<TS>")
	for _, r := range n.rules {
		s = r.re.ReplaceAllString(s, r.replace)
	}
	return s
}

// value normalizes a decoded JSON value, visiting object keys in sorted
// order so the numbering does not depend on the output's key order.
func (n *normalizer) value(v any) any {
	switch x := v.(type) {
	case string:
		return n.text(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = n.value(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for _, k := range sortedKeys(x) {
			if n.keys[k] && x[k] != nil {
				out[n.text(k)] = mask(x[k], "<"+strings.ToUpper(k)+">")
				continue
			}
			out[n.text(k)] = n.value(x[k])
		}
		return out
	}
	return v
}

// mask replaces every leaf of v (null included) with m, keeping the keys
// of objects.
func mask(v any, m string) any {
	obj, ok := v.(map[string]any)
	if !ok {
		return m
	}
	out := make(map[string]any, len(obj))
	for k, e := range obj {
		out[k] = mask(e, m)
	}
	return out
}

// content classifies and normalizes stdout or a snapshot file.
func (n *normalizer) content(s string) (shape string, v any) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return "empty", nil
	}
	if doc, ok := decodeJSON(trimmed); ok {
		return "json", n.value(doc)
	}
	lines := strings.Split(trimmed, "\n")
	docs := make([]any, 0, len(lines))
	for _, line := range lines {
		doc, ok := decodeJSON(line)
		if !ok {
			return "text", n.text(s)
		}
		docs = append(docs, n.value(doc))
	}
	return "jsonl", docs
}

func decodeJSON(s string) (any, bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if dec.More() {
		return nil, false
	}
	return v, true
}

func (n *normalizer) step(st Step, raw rawStep) GoldenStep {
	shape, out := n.content(raw.stdout)
	g := GoldenStep{
		Argv:        st.Argv,
		Exit:        raw.exit,
		StdoutShape: shape,
		Stdout:      out,
	}
	if len(raw.files) > 0 {
		g.Files = map[string]any{}
		for _, p := range sortedKeys(raw.files) {
			c := raw.files[p]
			if c == nil {
				g.Files[p] = nil
				continue
			}
			shape, v := n.content(*c)
			g.Files[p] = map[string]any{"shape": shape, "content": v}
		}
	}
	return g
}

func TestRequestIDsAreMasked(t *testing.T) {
	root := t.TempDir()
	s := &sandbox{
		root: root,
		home: filepath.Join(root, "home"),
		work: filepath.Join(root, "work"),
		wt:   filepath.Join(root, "home", ".local", "state", "claude-orchestra", "worktrees", "work-00000000"),
	}
	n := newNormalizer(s, nil, nil)

	cases := map[string]string{
		"/work/requests/20261028-00000000/.integration/work/feat": "/work/requests/<DATE>-00000000/.integration/work/feat",
		"20261028-00000000-2":                  "<DATE>-00000000-2",
		"00000000-0000-4000-8000-0000000c0f0e": "<UUID#1>",
		"120261028-00000000":                   "120261028-00000000",
		"20261028-000000001":                   "20261028-000000001",
	}
	for in, want := range cases {
		if got := n.text(in); got != want {
			t.Errorf("text(%q) = %q, want %q", in, got, want)
		}
	}
}
