package conformance

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// GoldenStep is the normalized, implementation-independent record of one
// step. Stdout is kept by shape: a JSON document (json-indent / json-line)
// or JSON lines (jsonl) are stored decoded, so key order and whitespace do
// not matter but multi-line vs one-line output does; anything else is text.
type GoldenStep struct {
	Argv        []string       `json:"argv"`
	Exit        int            `json:"exit"`
	StdoutShape string         `json:"stdout_shape"`
	Stdout      any            `json:"stdout"`
	Stderr      string         `json:"stderr"`
	Files       map[string]any `json:"files,omitempty"`
}

type Golden struct {
	Scenario string       `json:"scenario"`
	Steps    []GoldenStep `json:"steps"`
}

var (
	reToken = regexp.MustCompile(`dsp-[0-9a-f]{12}`)
	reUUID  = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	reSHA   = regexp.MustCompile(`\b[0-9a-f]{40}\b`)
	reTS    = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?`)
)

// normalizer masks what legitimately differs between runs: the sandbox
// paths (realpath'd /private/var spellings included), dispatch tokens,
// uuids and SHAs (numbered by first appearance, so equal values stay
// equal), timestamps, and the values of volatile JSON keys.
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
	s = reUUID.ReplaceAllStringFunc(s, func(v string) string { return n.number("UUID", v) })
	s = reSHA.ReplaceAllStringFunc(s, func(v string) string { return n.number("SHA", v) })
	s = reTS.ReplaceAllString(s, "<TS>")
	for _, r := range n.rules {
		s = r.re.ReplaceAllString(s, r.replace)
	}
	return s
}

// value normalizes a decoded JSON value, visiting object keys in sorted
// order so the numbering does not depend on either implementation's key
// order.
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
				out[n.text(k)] = "<" + strings.ToUpper(k) + ">"
				continue
			}
			out[n.text(k)] = n.value(x[k])
		}
		return out
	}
	return v
}

// content classifies and normalizes stdout or a snapshot file.
func (n *normalizer) content(s string) (shape string, v any) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return "empty", nil
	}
	if doc, ok := decodeJSON(trimmed); ok {
		if strings.Contains(trimmed, "\n") {
			return "json-indent", n.value(doc)
		}
		return "json-line", n.value(doc)
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
		Stderr:      n.text(raw.stderr),
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
