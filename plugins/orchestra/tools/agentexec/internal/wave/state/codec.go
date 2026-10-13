package state

import (
	"fmt"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
)

func num(v any) float64 {
	switch x := v.(type) {
	case int64:
		return float64(x)
	case float64:
		return x
	case bool:
		if x {
			return 1
		}
	}
	return 0
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func strPtr(v any) *string {
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}

func intPtr(v any) *int {
	switch x := v.(type) {
	case int64:
		n := int(x)
		return &n
	case float64:
		n := int(x)
		return &n
	}
	return nil
}

func ptrVal[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// sortable turns jsonx objects into plain maps (sorted on output) so nested
// values obey sort_keys like Python's json.dump.
func sortable(v any) any {
	switch x := v.(type) {
	case *jsonx.Object:
		out := make(map[string]any, x.Len())
		for _, k := range x.Keys() {
			val, _ := x.Get(k)
			out[k] = sortable(val)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = sortable(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = sortable(e)
		}
		return out
	}
	return v
}

func extraOf(o *jsonx.Object, known ...string) map[string]any {
	skip := map[string]bool{}
	for _, k := range known {
		skip[k] = true
	}
	var extra map[string]any
	for _, k := range o.Keys() {
		if skip[k] {
			continue
		}
		if extra == nil {
			extra = map[string]any{}
		}
		extra[k], _ = o.Get(k)
	}
	return extra
}

func parsePackage(v any) *Package {
	o, _ := v.(*jsonx.Object)
	if o == nil {
		o = jsonx.NewObject()
	}
	get := func(k string) any { v, _ := o.Get(k); return v }
	p := &Package{
		Status:       str(get("status")),
		Since:        num(get("since")),
		Attempts:     int(num(get("attempts"))),
		Tree:         strPtr(get("tree")),
		Executor:     strPtr(get("executor")),
		Session:      get("session"),
		FilesChanged: intPtr(get("files_changed")),
		Commit:       strPtr(get("commit")),
		Detail:       str(get("detail")),
		Extra: extraOf(o, "status", "since", "attempts", "tree", "executor", "session",
			"files_changed", "commit", "detail", "network_denials"),
	}
	if _, ok := o.Get("session"); !ok {
		p.Session = false
	}
	if d, ok := get("network_denials").([]any); ok {
		p.NetworkDenials = d
	}
	return p
}

// Parse reads state.json content.
func Parse(data []byte) (State, error) {
	doc, err := jsonx.Decode(data)
	if err != nil {
		return State{}, err
	}
	o, ok := doc.(*jsonx.Object)
	if !ok {
		return State{}, fmt.Errorf("state is not a JSON object")
	}
	get := func(k string) any { v, _ := o.Get(k); return v }
	s := State{
		Version:  int(num(get("version"))),
		Plan:     str(get("plan")),
		Wave:     int(num(get("wave"))),
		Updated:  num(get("updated")),
		Packages: map[string]*Package{},
		Extra: extraOf(o, "version", "plan", "integration", "wave", "packages", "needs",
			"stopped", "updated"),
	}
	if in, ok := get("integration").(*jsonx.Object); ok {
		iv := func(k string) *string { v, _ := in.Get(k); return strPtr(v) }
		s.Integration = Integration{Task: iv("task"), Path: iv("path"), Base: iv("base")}
	}
	if pk, ok := get("packages").(*jsonx.Object); ok {
		for _, id := range pk.Keys() {
			v, _ := pk.Get(id)
			s.Packages[id] = parsePackage(v)
		}
	}
	if needs, ok := get("needs").([]any); ok {
		for _, n := range needs {
			no, ok := n.(*jsonx.Object)
			if !ok {
				continue
			}
			g := func(k string) any { v, _ := no.Get(k); return v }
			s.Needs = append(s.Needs, Need{ID: strPtr(g("id")), Kind: str(g("kind")), Detail: str(g("detail")), At: num(g("at"))})
		}
	}
	if st, ok := get("stopped").(*jsonx.Object); ok && st.Len() > 0 {
		g := func(k string) any { v, _ := st.Get(k); return v }
		s.Stopped = &Stopped{Reason: str(g("reason")), At: num(g("at"))}
	}
	return s, nil
}

// ToMap is the state as the plain map written to state.json (and printed by
// `wave status --json`).
func (s *State) ToMap() map[string]any {
	pkgs := make(map[string]any, len(s.Packages))
	for id, p := range s.Packages {
		m := map[string]any{
			"status": p.Status, "since": p.Since, "attempts": p.Attempts,
			"tree": ptrVal(p.Tree), "executor": ptrVal(p.Executor), "session": p.Session,
			"files_changed": ptrVal(p.FilesChanged), "commit": ptrVal(p.Commit), "detail": p.Detail,
		}
		if p.NetworkDenials != nil {
			m["network_denials"] = sortable(p.NetworkDenials)
		}
		for k, v := range p.Extra {
			m[k] = sortable(v)
		}
		pkgs[id] = m
	}
	needs := make([]any, 0, len(s.Needs))
	for _, n := range s.Needs {
		needs = append(needs, map[string]any{"id": ptrVal(n.ID), "kind": n.Kind, "detail": n.Detail, "at": n.At})
	}
	var stopped any
	if s.Stopped != nil {
		stopped = map[string]any{"reason": s.Stopped.Reason, "at": s.Stopped.At}
	}
	m := map[string]any{
		"version": s.Version, "plan": s.Plan,
		"integration": map[string]any{
			"task": ptrVal(s.Integration.Task), "path": ptrVal(s.Integration.Path), "base": ptrVal(s.Integration.Base),
		},
		"wave": s.Wave, "packages": pkgs, "needs": needs, "stopped": stopped, "updated": s.Updated,
	}
	for k, v := range s.Extra {
		m[k] = sortable(v)
	}
	return m
}

// Marshal is the state.json encoding: indent=2, sorted keys, no trailing
// newline.
func (s *State) Marshal() ([]byte, error) { return jsonx.MarshalIndent(s.ToMap()) }
