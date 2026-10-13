// Package jsonx holds the JSON value model shared by every agent-exec
// package: an insertion-ordered Object (Python dict semantics), a deep merge
// with the config layering rules, and an encoder whose output matches
// Python's json.dumps(..., ensure_ascii=False).
package jsonx

// Object is a JSON object that keeps keys in insertion order. Setting an
// existing key replaces its value in place, as a Python dict does.
type Object struct {
	keys []string
	vals map[string]any
}

func NewObject() *Object {
	return &Object{vals: map[string]any{}}
}

// Set stores v under k and returns o for chaining.
func (o *Object) Set(k string, v any) *Object {
	if o.vals == nil {
		o.vals = map[string]any{}
	}
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
	return o
}

func (o *Object) Get(k string) (any, bool) {
	if o == nil {
		return nil, false
	}
	v, ok := o.vals[k]
	return v, ok
}

// Keys returns a copy of the keys in insertion order.
func (o *Object) Keys() []string {
	if o == nil {
		return nil
	}
	return append([]string(nil), o.keys...)
}

func (o *Object) Delete(k string) {
	if o == nil {
		return
	}
	if _, ok := o.vals[k]; !ok {
		return
	}
	delete(o.vals, k)
	for i, key := range o.keys {
		if key == k {
			o.keys = append(o.keys[:i:i], o.keys[i+1:]...)
			break
		}
	}
}

func (o *Object) Len() int {
	if o == nil {
		return 0
	}
	return len(o.keys)
}

// Object returns the value under k when it is itself an object.
func (o *Object) Object(k string) *Object {
	v, _ := o.Get(k)
	obj, _ := v.(*Object)
	return obj
}

// Clone deep-copies o, including nested objects and []any slices.
func (o *Object) Clone() *Object {
	if o == nil {
		return nil
	}
	out := &Object{keys: append([]string(nil), o.keys...), vals: make(map[string]any, len(o.vals))}
	for k, v := range o.vals {
		out.vals[k] = CloneValue(v)
	}
	return out
}

// CloneValue deep-copies the mutable containers jsonx produces (*Object and
// []any); every other value is returned as is.
func CloneValue(v any) any {
	switch x := v.(type) {
	case *Object:
		return x.Clone()
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = CloneValue(e)
		}
		return out
	default:
		return v
	}
}

// DeepMerge merges over onto base with Python's _deep_merge rules: only
// object+object recurses; every other value (lists included, and an explicit
// null) replaces the base value wholesale. Neither input is modified; values
// taken from over are shared, not copied.
func DeepMerge(base, over *Object) *Object {
	if base == nil {
		base = NewObject()
	}
	out := base.Clone()
	if over == nil {
		return out
	}
	for _, k := range over.keys {
		v := over.vals[k]
		if cur, ok := out.vals[k]; ok {
			curObj, curIsObj := cur.(*Object)
			vObj, vIsObj := v.(*Object)
			if curIsObj && vIsObj && curObj != nil && vObj != nil {
				out.Set(k, DeepMerge(curObj, vObj))
				continue
			}
		}
		out.Set(k, v)
	}
	return out
}

func (o *Object) MarshalJSON() ([]byte, error) {
	return compact(o)
}

func (o *Object) UnmarshalJSON(data []byte) error {
	v, err := Decode(data)
	if err != nil {
		return err
	}
	obj, ok := v.(*Object)
	if !ok {
		return &decodeError{msg: "jsonx: JSON value is not an object"}
	}
	*o = *obj
	return nil
}
