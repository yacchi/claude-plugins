package watchdog

import (
	"bytes"
	"encoding/json"
	"io"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
)

// previewRunes caps args_preview (code points).
const previewRunes = 200

// decodeObject parses one JSONL record that is an object. Numbers stay
// json.Number so an integer of any size keeps its digits.
func decodeObject(line string) (map[string]any, bool) {
	dec := json.NewDecoder(bytes.NewReader([]byte(line)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	m, ok := v.(map[string]any)
	return m, ok
}

// Canonical is the compact JSON of a decoded value with object keys sorted
// and non-ASCII text left as is: the identity of a tool call's args. Numbers
// are compared by value, so 1 and 1.0 are the same argument.
func Canonical(v any) string {
	var b strings.Builder
	canonical(&b, v)
	return b.String()
}

func canonical(b *strings.Builder, v any) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case int:
		b.WriteString(strconv.Itoa(x))
	case float64:
		b.WriteString(strconv.FormatFloat(x, 'g', -1, 64))
	case json.Number:
		b.WriteString(number(x))
	case string:
		writeString(b, x)
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			canonical(b, e)
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys) // byte order == code point order for UTF-8
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeString(b, k)
			b.WriteByte(':')
			canonical(b, x[k])
		}
		b.WriteByte('}')
	default:
		raw, err := fsutil.MarshalJSON(x)
		if err != nil {
			b.WriteString("null")
			return
		}
		b.Write(raw)
	}
}

func writeString(b *strings.Builder, s string) {
	const hex = "0123456789abcdef"
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				b.WriteString(`\u00`)
				b.WriteByte(hex[r>>4])
				b.WriteByte(hex[r&0xf])
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// number renders a JSON number literal: integers keep every digit at any
// size, anything with a fraction or exponent is formatted by value.
func number(n json.Number) string {
	s := string(n)
	if !strings.ContainsAny(s, ".eE") {
		if s == "-0" {
			return "0"
		}
		return s
	}
	f, _ := strconv.ParseFloat(s, 64)
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func preview(v any) string {
	text := Canonical(v)
	if utf8.RuneCountInString(text) <= previewRunes {
		return text
	}
	return string([]rune(text)[:previewRunes])
}

// toolName is a tool's name for reports: the string itself, else the
// canonical form of whatever the event carried.
func toolName(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return Canonical(v)
}

func itoa(n int) string { return strconv.Itoa(n) }

func killGroup(pgid int, hard bool) error {
	sig := syscall.SIGTERM
	if hard {
		sig = syscall.SIGKILL
	}
	return syscall.Kill(-pgid, sig)
}
