package jsonx

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"strings"
)

type decodeError struct{ msg string }

func (e *decodeError) Error() string { return e.msg }

// Decode parses one JSON document into jsonx values: objects become
// *Object (key order kept), arrays []any, integers int64 (float64 when they
// overflow), other numbers float64.
func Decode(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, &decodeError{msg: "jsonx: trailing data after JSON value"}
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := NewObject()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, _ := kt.(string)
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				obj.Set(key, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return obj, nil
		case '[':
			arr := []any{}
			for dec.More() {
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return arr, nil
		}
		return nil, &decodeError{msg: "jsonx: unexpected delimiter " + t.String()}
	case json.Number:
		return number(string(t)), nil
	default:
		return t, nil
	}
}

func number(s string) any {
	if !strings.ContainsAny(s, ".eE") {
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return i
		}
	}
	f, _ := strconv.ParseFloat(s, 64)
	return f
}
