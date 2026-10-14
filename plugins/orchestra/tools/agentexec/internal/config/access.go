package config

import (
	"encoding/json"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
)

// Lookup walks path through nested objects. ok is false when any step is
// missing or not an object.
func (r *Resolved) Lookup(path ...string) (any, bool) {
	if r == nil || r.Values == nil {
		return nil, false
	}
	var cur any = r.Values
	for _, k := range path {
		obj, isObj := cur.(map[string]any)
		if !isObj || obj == nil {
			return nil, false
		}
		v, ok := obj[k]
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
// not integers).
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
func (r *Resolved) Section(name string) map[string]any {
	v, _ := r.Lookup(name)
	obj, _ := v.(map[string]any)
	return obj
}

// Decode unmarshals the top-level section into out through JSON, so out's
// json tags and types apply. A missing section leaves out untouched.
func Decode(r *Resolved, section string, out any) error {
	v, ok := r.Lookup(section)
	if !ok {
		return nil
	}
	data, err := fsutil.MarshalJSON(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

// ObjectAt is the object stored under key in m, or nil when it is absent or
// not an object.
func ObjectAt(m map[string]any, key string) map[string]any {
	obj, _ := m[key].(map[string]any)
	return obj
}

// CloneValue deep-copies the containers of a JSON value (objects and
// arrays); every other value is returned as is.
func CloneValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		if x == nil {
			return x
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = CloneValue(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = CloneValue(e)
		}
		return out
	}
	return v
}

// DeepMerge merges over onto base with the config layering rules: only
// object+object recurses; every other value (lists included, and an explicit
// null) replaces the base value wholesale. Neither input is modified; values
// taken from over are shared, not copied.
func DeepMerge(base, over map[string]any) map[string]any {
	out, _ := CloneValue(base).(map[string]any)
	if out == nil {
		out = map[string]any{}
	}
	for k, v := range over {
		curObj, curIsObj := out[k].(map[string]any)
		vObj, vIsObj := v.(map[string]any)
		if curIsObj && vIsObj && curObj != nil && vObj != nil {
			out[k] = DeepMerge(curObj, vObj)
			continue
		}
		out[k] = v
	}
	return out
}
