package state

import (
	"fmt"
	"maps"
	"slices"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
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

// cloneValue deep-copies the containers of a decoded JSON value, so a
// rendered map never aliases the state it came from.
func cloneValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = cloneValue(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = cloneValue(e)
		}
		return out
	}
	return v
}

func extraOf(o map[string]any, known ...string) map[string]any {
	skip := map[string]bool{}
	for _, k := range known {
		skip[k] = true
	}
	var extra map[string]any
	for _, k := range slices.Sorted(maps.Keys(o)) {
		if skip[k] {
			continue
		}
		if extra == nil {
			extra = map[string]any{}
		}
		extra[k] = o[k]
	}
	return extra
}

func parsePackage(v any) *Package {
	o, _ := v.(map[string]any)
	if o == nil {
		o = map[string]any{}
	}
	get := func(k string) any { return o[k] }
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
	if _, ok := o["session"]; !ok {
		p.Session = false
	}
	if d, ok := get("network_denials").([]any); ok {
		p.NetworkDenials = d
	}
	return p
}

// Parse reads state.json content.
func Parse(data []byte) (State, error) {
	doc, err := fsutil.DecodeJSON(data)
	if err != nil {
		return State{}, err
	}
	o, ok := doc.(map[string]any)
	if !ok {
		return State{}, fmt.Errorf("state is not a JSON object")
	}
	get := func(k string) any { return o[k] }
	s := State{
		Version:  int(num(get("version"))),
		Plan:     str(get("plan")),
		Wave:     int(num(get("wave"))),
		Updated:  num(get("updated")),
		Packages: map[string]*Package{},
		Extra: extraOf(o, "version", "plan", "integration", "wave", "packages", "needs",
			"stopped", "updated"),
	}
	if in, ok := get("integration").(map[string]any); ok {
		iv := func(k string) *string { v, _ := in[k]; return strPtr(v) }
		s.Integration = Integration{Task: iv("task"), Path: iv("path"), Base: iv("base")}
	}
	if pk, ok := get("packages").(map[string]any); ok {
		for _, id := range slices.Sorted(maps.Keys(pk)) {
			v := pk[id]
			s.Packages[id] = parsePackage(v)
		}
	}
	if needs, ok := get("needs").([]any); ok {
		for _, n := range needs {
			no, ok := n.(map[string]any)
			if !ok {
				continue
			}
			g := func(k string) any { return no[k] }
			s.Needs = append(s.Needs, Need{ID: strPtr(g("id")), Kind: str(g("kind")), Detail: str(g("detail")), At: num(g("at"))})
		}
	}
	if st, ok := get("stopped").(map[string]any); ok && len(st) > 0 {
		g := func(k string) any { return st[k] }
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
			m["network_denials"] = cloneValue(p.NetworkDenials)
		}
		for k, v := range p.Extra {
			m[k] = cloneValue(v)
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
		m[k] = cloneValue(v)
	}
	return m
}

// Marshal is the state.json encoding: indent=2, sorted keys, no trailing
// newline.
func (s *State) Marshal() ([]byte, error) { return fsutil.MarshalJSONIndent(s.ToMap()) }
