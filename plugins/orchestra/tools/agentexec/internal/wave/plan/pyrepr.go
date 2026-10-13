package plan

import (
	"strconv"
	"strings"
	"unicode"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
)

// pyRepr renders a decoded JSON value the way Python's repr() does, for the
// error messages that quote plan content.
func pyRepr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		return pyStrRepr(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return jsonx.FormatFloat(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = pyRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *jsonx.Object:
		keys := x.Keys()
		parts := make([]string, len(keys))
		for i, k := range keys {
			val, _ := x.Get(k)
			parts[i] = pyStrRepr(k) + ": " + pyRepr(val)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return "?"
}

func pyStrRepr(s string) string {
	quote := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == rune(quote) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			b.WriteString(`\x` + hexN(int(r), 2))
		case r < 0x7f || unicode.IsPrint(r):
			b.WriteRune(r)
		case r <= 0xff:
			b.WriteString(`\x` + hexN(int(r), 2))
		case r <= 0xffff:
			b.WriteString(`\u` + hexN(int(r), 4))
		default:
			b.WriteString(`\U` + hexN(int(r), 8))
		}
	}
	b.WriteByte(quote)
	return b.String()
}

func hexN(n, width int) string {
	s := strconv.FormatInt(int64(n), 16)
	return strings.Repeat("0", max(width-len(s), 0)) + s
}
