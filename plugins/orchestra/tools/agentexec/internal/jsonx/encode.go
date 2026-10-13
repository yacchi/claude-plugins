package jsonx

import (
	"bytes"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Marshal renders v on one line exactly as Python's
// json.dumps(v, ensure_ascii=False) does: ", " and ": " separators, no HTML
// escaping, non-ASCII left as is, floats in repr form.
func Marshal(v any) ([]byte, error) {
	b, err := compact(v)
	if err != nil {
		return nil, err
	}
	return pythonSpacing(b), nil
}

// MarshalIndent renders v as Python's json.dumps(v, indent=2,
// ensure_ascii=False) does (no trailing newline).
func MarshalIndent(v any) ([]byte, error) {
	b, err := compact(v)
	if err != nil {
		return nil, err
	}
	return pythonIndent(b), nil
}

func compact(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := encode(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func encode(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case *Object:
		if x == nil {
			buf.WriteString("null")
			return nil
		}
		buf.WriteByte('{')
		for i, k := range x.keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeString(buf, k)
			buf.WriteByte(':')
			if err := encode(buf, x.vals[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeString(buf, k)
			buf.WriteByte(':')
			if err := encode(buf, x[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case []any:
		buf.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := encode(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case []string:
		buf.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeString(buf, e)
		}
		buf.WriteByte(']')
	case string:
		writeString(buf, x)
	case bool:
		buf.WriteString(strconv.FormatBool(x))
	case int:
		buf.WriteString(strconv.FormatInt(int64(x), 10))
	case int64:
		buf.WriteString(strconv.FormatInt(x, 10))
	case int32:
		buf.WriteString(strconv.FormatInt(int64(x), 10))
	case uint64:
		buf.WriteString(strconv.FormatUint(x, 10))
	case float64:
		buf.WriteString(FormatFloat(x))
	case float32:
		buf.WriteString(FormatFloat(float64(x)))
	case json.Number:
		buf.WriteString(string(x))
	default:
		return encodeForeign(buf, v)
	}
	return nil
}

// encodeForeign falls back to encoding/json for structs and other Go types,
// without HTML escaping. Their field order is the struct's declaration order.
func encodeForeign(buf *bytes.Buffer, v any) error {
	var tmp bytes.Buffer
	enc := json.NewEncoder(&tmp)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	buf.Write(bytes.TrimRight(tmp.Bytes(), "\n"))
	return nil
}

// FormatFloat renders f as Python's float repr (what json.dumps emits):
// "1.0", "0.0001", "1e-05", "1e+16"; non-finite values as Infinity/NaN.
func FormatFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	sci := strconv.FormatFloat(f, 'e', -1, 64)
	exp, _ := strconv.Atoi(sci[strings.IndexByte(sci, 'e')+1:])
	if exp < -4 || exp >= 16 {
		return sci
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.ContainsAny(s, ".") {
		s += ".0"
	}
	return s
}

const hexDigits = "0123456789abcdef"

func writeString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch c {
			case '"':
				buf.WriteString(`\"`)
			case '\\':
				buf.WriteString(`\\`)
			case '\n':
				buf.WriteString(`\n`)
			case '\r':
				buf.WriteString(`\r`)
			case '\t':
				buf.WriteString(`\t`)
			case '\b':
				buf.WriteString(`\b`)
			case '\f':
				buf.WriteString(`\f`)
			default:
				if c < 0x20 {
					buf.WriteString(`\u00`)
					buf.WriteByte(hexDigits[c>>4])
					buf.WriteByte(hexDigits[c&0xf])
				} else {
					buf.WriteByte(c)
				}
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			buf.WriteString(`�`)
		} else {
			buf.WriteString(s[i : i+size])
		}
		i += size
	}
	buf.WriteByte('"')
}

// pythonIndent lays compact JSON out like json.dumps(indent=2): one member
// per line, ": " after keys, and empty containers kept as {} / [].
func pythonIndent(b []byte) []byte {
	out := make([]byte, 0, len(b)*2)
	depth := 0
	newline := func() {
		out = append(out, '\n')
		for i := 0; i < depth; i++ {
			out = append(out, ' ', ' ')
		}
	}
	inStr, esc := false, false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if inStr {
			out = append(out, c)
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
			out = append(out, c)
		case '{', '[':
			out = append(out, c)
			if i+1 < len(b) && (b[i+1] == '}' || b[i+1] == ']') {
				out = append(out, b[i+1])
				i++
				continue
			}
			depth++
			newline()
		case '}', ']':
			depth--
			newline()
			out = append(out, c)
		case ',':
			out = append(out, c)
			newline()
		case ':':
			out = append(out, ':', ' ')
		default:
			out = append(out, c)
		}
	}
	return out
}

// pythonSpacing turns compact JSON into Python's default one-line layout by
// adding a space after every ',' and ':' that is not inside a string.
func pythonSpacing(b []byte) []byte {
	out := make([]byte, 0, len(b)+len(b)/8)
	inStr, esc := false, false
	for _, c := range b {
		out = append(out, c)
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case ',', ':':
			out = append(out, ' ')
		}
	}
	return out
}
