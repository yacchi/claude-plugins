package usage

import (
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

var (
	windowRE   = regexp.MustCompile(`^([0-9]+)([mhd])$`)
	fractionRE = regexp.MustCompile(`\.([0-9]+)`)
	// The ISO 8601 forms datetime.fromisoformat accepts (extended and basic
	// date, optional time with optional seconds and fraction, optional UTC
	// offset); the fraction is normalized to six digits beforehand.
	isoRE = regexp.MustCompile(`^([0-9]{4})-?([0-9]{2})-?([0-9]{2})` +
		`(?:[Tt ]([0-9]{2})(?::?([0-9]{2})(?::?([0-9]{2})(?:\.([0-9]{6}))?)?)?` +
		`(?:([+-])([0-9]{2})(?::?([0-9]{2})(?::?([0-9]{2}))?)?)?)?$`)
)

var windowSeconds = map[string]int64{"m": 60, "h": 3600, "d": 86400}

// ParseTimestamp reads a log timestamp as an instant in UTC. It tolerates a
// trailing "Z", odd fractional-second widths and naive stamps (taken as
// UTC); anything else is not ok and the caller skips the record rather than
// guessing.
func ParseTimestamp(v any) (time.Time, bool) {
	s, ok := v.(string)
	if !ok {
		return time.Time{}, false
	}
	return parseTimestampString(s)
}

func parseTimestampString(s string) (time.Time, bool) {
	text := strings.TrimFunc(s, unicode.IsSpace)
	if text == "" {
		return time.Time{}, false
	}
	if strings.HasSuffix(text, "Z") || strings.HasSuffix(text, "z") {
		text = text[:len(text)-1] + "+00:00"
	}
	text = fractionRE.ReplaceAllStringFunc(text, func(m string) string {
		digits := m[1:]
		if len(digits) > 6 {
			digits = digits[:6]
		}
		return "." + digits + strings.Repeat("0", 6-len(digits))
	})
	m := isoRE.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}, false
	}
	num := func(i int) int {
		n, _ := strconv.Atoi(m[i])
		return n
	}
	year, month, day := num(1), num(2), num(3)
	hour, minute, second := 0, 0, 0
	if m[4] != "" {
		hour = num(4)
	}
	if m[5] != "" {
		minute = num(5)
	}
	if m[6] != "" {
		second = num(6)
	}
	micro := 0
	if m[7] != "" {
		micro = num(7)
	}
	if month < 1 || month > 12 || day < 1 || hour > 23 || minute > 59 || second > 59 {
		return time.Time{}, false
	}
	t := time.Date(year, time.Month(month), day, hour, minute, second, micro*1000, time.UTC)
	if t.Day() != day || year < 1 {
		return time.Time{}, false // 31 February and the like
	}
	if m[8] != "" {
		oh, om, os := num(9), 0, 0
		if m[10] != "" {
			om = num(10)
		}
		if m[11] != "" {
			os = num(11)
		}
		if oh > 23 || om > 59 || os > 59 {
			return time.Time{}, false
		}
		offset := time.Duration(oh)*time.Hour + time.Duration(om)*time.Minute + time.Duration(os)*time.Second
		if m[8] == "+" {
			t = t.Add(-offset)
		} else {
			t = t.Add(offset)
		}
	}
	return t, true
}

// ParseSince turns a --since value and the current time into the window's
// cutoff: "<N>m|h|d" back from now, or an ISO 8601 timestamp.
func ParseSince(value string, now time.Time) (time.Time, bool) {
	text := strings.TrimFunc(value, unicode.IsSpace)
	if text == "" {
		return time.Time{}, false
	}
	if m := windowRE.FindStringSubmatch(text); m != nil {
		amount, err := strconv.ParseInt(m[1], 10, 64)
		// Beyond ~290 years the duration arithmetic would overflow.
		if err != nil || amount > 9_000_000_000/windowSeconds[m[2]] {
			return time.Time{}, false
		}
		return now.Add(-time.Duration(amount*windowSeconds[m[2]]) * time.Second), true
	}
	return parseTimestampString(text)
}
