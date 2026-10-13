package config

import (
	"encoding/json"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
)

// Lookup walks path through nested objects. ok is false when any step is
// missing or not an object.
func (r *Resolved) Lookup(path ...string) (any, bool) {
	if r == nil || r.Values == nil {
		return nil, false
	}
	var cur any = r.Values
	for _, k := range path {
		obj, isObj := cur.(*jsonx.Object)
		if !isObj || obj == nil {
			return nil, false
		}
		v, ok := obj.Get(k)
		if !ok {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

// Bool is the value at path when it is a boolean.
func (r *Resolved) Bool(path ...string) (bool, bool) {
	v, _ := r.Lookup(path...)
	b, ok := v.(bool)
	return b, ok
}

// Int is the value at path when it is an integer (booleans and floats are
// not integers, as in Python's isinstance(v, int) minus bool).
func (r *Resolved) Int(path ...string) (int, bool) {
	v, _ := r.Lookup(path...)
	switch x := v.(type) {
	case int64:
		return int(x), true
	case int:
		return x, true
	}
	return 0, false
}

// String is the value at path when it is a string.
func (r *Resolved) String(path ...string) (string, bool) {
	v, _ := r.Lookup(path...)
	s, ok := v.(string)
	return s, ok
}

// StringList returns the string elements of the list at path (non-strings
// are skipped; a missing or non-list value is nil).
func (r *Resolved) StringList(path ...string) []string {
	v, _ := r.Lookup(path...)
	list, _ := v.([]any)
	var out []string
	for _, e := range list {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// Section is the top-level object name, or nil when absent or not an object.
func (r *Resolved) Section(name string) *jsonx.Object {
	v, _ := r.Lookup(name)
	obj, _ := v.(*jsonx.Object)
	return obj
}

// Decode unmarshals the top-level section into out through JSON, so out's
// json tags and types apply. A missing section leaves out untouched.
func Decode(r *Resolved, section string, out any) error {
	v, ok := r.Lookup(section)
	if !ok {
		return nil
	}
	data, err := jsonx.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}
