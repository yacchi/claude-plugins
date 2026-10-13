// Package yaml11 loads YAML with the implicit typing of PyYAML's
// yaml.safe_load (YAML 1.1): `yes`/`off` are booleans, `0755` is octal,
// `1e3` is a string, `1:30` is sexagesimal, and so on. Parsing is done by
// go.yaml.in/yaml/v3; only the scalar resolution is replaced, and only for
// plain (unquoted, untagged) scalars, which is where the two specs differ.
package yaml11

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
)

// Timestamp is a YAML 1.1 timestamp. DateOnly marks `2001-12-14` (a Python
// date); HasZone is false for a naive datetime.
type Timestamp struct {
	Time     time.Time
	DateOnly bool
	HasZone  bool
}

// ISO renders the value as Python's date/datetime isoformat() does.
func (t Timestamp) ISO() string {
	if t.DateOnly {
		return t.Time.Format("2006-01-02")
	}
	s := t.Time.Format("2006-01-02T15:04:05")
	if us := t.Time.Nanosecond() / 1000; us != 0 {
		s += fmt.Sprintf(".%06d", us)
	}
	if t.HasZone {
		_, off := t.Time.Zone()
		sign := '+'
		if off < 0 {
			sign, off = '-', -off
		}
		s += fmt.Sprintf("%c%02d:%02d", sign, off/3600, off%3600/60)
	}
	return s
}

func (t Timestamp) MarshalJSON() ([]byte, error) {
	return jsonx.Marshal(t.ISO())
}

// Load parses a single YAML document. Mappings become *jsonx.Object (keys
// rendered as Python's json.dumps renders non-string keys), sequences []any,
// integers int64, floats float64, timestamps Timestamp. An empty document is
// nil.
func Load(data []byte) (any, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, errors.New("expected a single document in the stream")
	} else if !errors.Is(err, io.EOF) {
		return nil, err
	}
	return convert(&doc)
}

func convert(n *yaml.Node) (any, error) {
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil, nil
		}
		return convert(n.Content[0])
	case yaml.AliasNode:
		return convert(n.Alias)
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := convert(c)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case yaml.MappingNode:
		return convertMapping(n)
	case yaml.ScalarNode:
		return scalar(n)
	}
	return nil, fmt.Errorf("line %d: unsupported YAML node", n.Line)
}

// convertMapping follows PyYAML's flatten_mapping + construct_mapping: the
// pairs of every `<<` source are moved in front of the mapping's own pairs
// (a sequence of sources in reverse order, so the first source wins), then
// all pairs are inserted in order, a repeated key keeping its first position
// and taking the last value.
func convertMapping(n *yaml.Node) (*jsonx.Object, error) {
	var merged, own []*jsonx.Object
	pairs := jsonx.NewObject()
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if isMergeKey(k) {
			srcs, err := mergeSources(v)
			if err != nil {
				return nil, err
			}
			merged = append(merged, srcs...)
			continue
		}
		var key any = "="
		if !(k.Kind == yaml.ScalarNode && k.Value == "=" && isPlain(k)) {
			var err error
			if key, err = convert(k); err != nil {
				return nil, err
			}
		}
		val, err := convert(v)
		if err != nil {
			return nil, err
		}
		ks, err := keyString(key)
		if err != nil {
			return nil, fmt.Errorf("line %d: %v", k.Line, err)
		}
		pairs.Set(ks, val)
	}
	own = append(own, pairs)
	out := jsonx.NewObject()
	for _, src := range append(merged, own...) {
		for _, k := range src.Keys() {
			v, _ := src.Get(k)
			out.Set(k, v)
		}
	}
	return out, nil
}

func isMergeKey(k *yaml.Node) bool {
	return k.Kind == yaml.ScalarNode && k.Value == "<<" && isPlain(k)
}

func mergeSources(v *yaml.Node) ([]*jsonx.Object, error) {
	if v.Kind == yaml.AliasNode {
		v = v.Alias
	}
	switch v.Kind {
	case yaml.MappingNode:
		obj, err := convertMapping(v)
		if err != nil {
			return nil, err
		}
		return []*jsonx.Object{obj}, nil
	case yaml.SequenceNode:
		var out []*jsonx.Object
		for _, c := range v.Content {
			if c.Kind == yaml.AliasNode {
				c = c.Alias
			}
			if c.Kind != yaml.MappingNode {
				return nil, fmt.Errorf("line %d: expected a mapping for merging", c.Line)
			}
			obj, err := convertMapping(c)
			if err != nil {
				return nil, err
			}
			out = append([]*jsonx.Object{obj}, out...)
		}
		return out, nil
	}
	return nil, fmt.Errorf("line %d: expected a mapping or list of mappings for merging", v.Line)
}

// keyString renders a mapping key the way json.dumps renders a dict key.
func keyString(k any) (string, error) {
	switch x := k.(type) {
	case string:
		return x, nil
	case nil:
		return "null", nil
	case bool:
		return strconv.FormatBool(x), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case float64:
		return jsonx.FormatFloat(x), nil
	case Timestamp:
		return x.ISO(), nil
	}
	return "", errors.New("found unhashable key")
}

func isPlain(n *yaml.Node) bool {
	const notPlain = yaml.TaggedStyle | yaml.SingleQuotedStyle | yaml.DoubleQuotedStyle |
		yaml.LiteralStyle | yaml.FoldedStyle
	return n.Style&notPlain == 0
}

func scalar(n *yaml.Node) (any, error) {
	if isPlain(n) {
		return resolvePlain(n.Value)
	}
	if n.Style&yaml.TaggedStyle == 0 {
		return n.Value, nil
	}
	switch n.Tag {
	case "!!str", "!":
		return n.Value, nil
	case "!!null":
		return nil, nil
	case "!!bool":
		b, ok := boolValue(n.Value)
		if !ok {
			return nil, fmt.Errorf("line %d: invalid boolean %q", n.Line, n.Value)
		}
		return b, nil
	case "!!int":
		return intValue(n.Value)
	case "!!float":
		return floatValue(n.Value)
	case "!!timestamp":
		return timestampValue(n.Value)
	}
	return nil, fmt.Errorf("line %d: could not determine a constructor for the tag %s", n.Line, n.Tag)
}

var (
	reBool    = regexp.MustCompile(`^(?:yes|Yes|YES|no|No|NO|true|True|TRUE|false|False|FALSE|on|On|ON|off|Off|OFF)$`)
	reNull    = regexp.MustCompile(`^(?:~|null|Null|NULL|)$`)
	reInt     = regexp.MustCompile(`^(?:[-+]?0b[0-1_]+|[-+]?0[0-7_]+|[-+]?(?:0|[1-9][0-9_]*)|[-+]?0x[0-9a-fA-F_]+|[-+]?[1-9][0-9_]*(?::[0-5]?[0-9])+)$`)
	reFloat   = regexp.MustCompile(`^(?:[-+]?(?:[0-9][0-9_]*)\.[0-9_]*(?:[eE][-+][0-9]+)?|\.[0-9][0-9_]*(?:[eE][-+][0-9]+)?|[-+]?[0-9][0-9_]*(?::[0-5]?[0-9])+\.[0-9_]*|[-+]?\.(?:inf|Inf|INF)|\.(?:nan|NaN|NAN))$`)
	reTS      = regexp.MustCompile(`^(?:[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]|[0-9][0-9][0-9][0-9]-[0-9][0-9]?-[0-9][0-9]?(?:[Tt]|[ \t]+)[0-9][0-9]?:[0-9][0-9]:[0-9][0-9](?:\.[0-9]*)?(?:[ \t]*(?:Z|[-+][0-9][0-9]?(?::[0-9][0-9])?))?)$`)
	reTSParts = regexp.MustCompile(`^([0-9][0-9][0-9][0-9])-([0-9][0-9]?)-([0-9][0-9]?)(?:(?:[Tt]|[ \t]+)([0-9][0-9]?):([0-9][0-9]):([0-9][0-9])(?:\.([0-9]*))?(?:[ \t]*(Z|([-+])([0-9][0-9]?)(?::([0-9][0-9]))?))?)?$`)
)

// resolvePlain applies PyYAML's implicit resolvers in their registration
// order (bool, float, int, merge, null, timestamp, value).
func resolvePlain(s string) (any, error) {
	switch {
	case reBool.MatchString(s):
		b, _ := boolValue(s)
		return b, nil
	case reFloat.MatchString(s):
		return floatValue(s)
	case reInt.MatchString(s):
		return intValue(s)
	case s == "<<":
		return nil, errors.New("could not determine a constructor for the tag tag:yaml.org,2002:merge")
	case reNull.MatchString(s):
		return nil, nil
	case reTS.MatchString(s):
		return timestampValue(s)
	case s == "=":
		return nil, errors.New("could not determine a constructor for the tag tag:yaml.org,2002:value")
	}
	return s, nil
}

func boolValue(s string) (bool, bool) {
	switch strings.ToLower(s) {
	case "yes", "true", "on":
		return true, true
	case "no", "false", "off":
		return false, true
	}
	return false, false
}

func splitSign(s string) (string, int) {
	if s == "" {
		return s, 1
	}
	switch s[0] {
	case '-':
		return s[1:], -1
	case '+':
		return s[1:], 1
	}
	return s, 1
}

func intValue(raw string) (any, error) {
	s, sign := splitSign(strings.ReplaceAll(raw, "_", ""))
	var (
		v   int64
		err error
	)
	switch {
	case s == "0":
		return int64(0), nil
	case strings.HasPrefix(s, "0b"):
		v, err = strconv.ParseInt(s[2:], 2, 64)
	case strings.HasPrefix(s, "0x"):
		v, err = strconv.ParseInt(s[2:], 16, 64)
	case strings.HasPrefix(s, "0"):
		v, err = strconv.ParseInt(s[1:], 8, 64)
	case strings.Contains(s, ":"):
		v, err = sexagesimalInt(s)
	default:
		v, err = strconv.ParseInt(s, 10, 64)
	}
	if err != nil {
		return nil, fmt.Errorf("invalid int %q", raw)
	}
	return int64(sign) * v, nil
}

func sexagesimalInt(s string) (int64, error) {
	var total int64
	for _, part := range strings.Split(s, ":") {
		d, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return 0, err
		}
		total = total*60 + d
	}
	return total, nil
}

func floatValue(raw string) (any, error) {
	s, sign := splitSign(strings.ToLower(strings.ReplaceAll(raw, "_", "")))
	switch {
	case s == ".inf":
		return math.Inf(sign), nil
	case s == ".nan":
		return math.NaN(), nil
	case strings.Contains(s, ":"):
		var total float64
		for _, part := range strings.Split(s, ":") {
			d, err := strconv.ParseFloat(part, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid float %q", raw)
			}
			total = total*60 + d
		}
		return float64(sign) * total, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid float %q", raw)
	}
	return float64(sign) * f, nil
}

func timestampValue(s string) (any, error) {
	m := reTSParts.FindStringSubmatch(s)
	if m == nil {
		return nil, fmt.Errorf("invalid timestamp %q", s)
	}
	atoi := func(x string) int { n, _ := strconv.Atoi(x); return n }
	year, month, day := atoi(m[1]), atoi(m[2]), atoi(m[3])
	if month < 1 || month > 12 || day < 1 || day > daysIn(year, month) {
		return nil, fmt.Errorf("invalid timestamp %q: day is out of range for month", s)
	}
	if m[4] == "" {
		return Timestamp{Time: time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC), DateOnly: true}, nil
	}
	hour, minute, second := atoi(m[4]), atoi(m[5]), atoi(m[6])
	if hour > 23 || minute > 59 || second > 59 {
		return nil, fmt.Errorf("invalid timestamp %q", s)
	}
	frac := m[7]
	if len(frac) > 6 {
		frac = frac[:6]
	}
	for len(frac) < 6 {
		frac += "0"
	}
	us := atoi(frac)
	loc, hasZone := time.UTC, false
	switch {
	case m[9] != "":
		off := atoi(m[10])*3600 + atoi(m[11])*60
		if m[9] == "-" {
			off = -off
		}
		loc, hasZone = time.FixedZone("", off), true
	case m[8] == "Z":
		hasZone = true
	}
	t := time.Date(year, time.Month(month), day, hour, minute, second, us*1000, loc)
	return Timestamp{Time: t, HasZone: hasZone}, nil
}

func daysIn(year, month int) int {
	return time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day()
}
