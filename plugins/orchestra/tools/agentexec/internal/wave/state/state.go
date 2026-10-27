// Package state owns a wave's state.json / events.jsonl pair, the STOP flag
// and the status rendering. The on-disk format is shared with the runner,
// and files written by earlier versions stay loadable: state.json is
// indent=2 with sorted keys.
package state

import ()

// Package statuses in display order.
var Statuses = []string{
	"pending", "implementing", "verifying", "fixing", "ready",
	"integrated", "needs", "failed", "blocked",
}

// NeedKinds are the accepted `needs` kinds.
var NeedKinds = []string{
	"escalate", "conflict", "post-integration", "self-verify", "delegate", "dispatch-error",
	"empty-after-refresh", "scope", "environment", "permission", "runaway", "network", "oversized",
	"integrate", "login", "merge",
}

var inFlight = map[string]bool{"implementing": true, "verifying": true, "fixing": true, "ready": true}

// InFlight reports whether a status counts as in flight.
func InFlight(status string) bool { return inFlight[status] }

// Package is one package's entry in state.json.
type Package struct {
	Status         string
	Since          float64
	Attempts       int
	Tree           *string
	Executor       *string
	Session        any // false, or a session id
	FilesChanged   *int
	Commit         *string
	Detail         string
	NetworkDenials []any // [{host, port}, ...]; absent when nil
	Extra          map[string]any
}

// Integration is the integration worktree record.
type Integration struct {
	Task, Path, Base *string
}

// Need is a `needs` entry; ID is nil for a wave-level need.
type Need struct {
	ID     *string
	Kind   string
	Detail string
	At     float64
}

// Stopped records why and when the runner stopped.
type Stopped struct {
	Reason string
	At     float64
}

// Held records why starts are held (Kind is login, memory or usage) and since
// when; a nil Held means starts are not held.
type Held struct {
	Kind   string
	Reason string
	Since  float64
}

// State is state.json.
type State struct {
	Version     int
	Plan        string
	Integration Integration
	Wave        int
	Packages    map[string]*Package
	Needs       []Need
	Stopped     *Stopped
	Held        *Held
	Updated     float64
	Extra       map[string]any // top-level keys this package does not know

	now    float64
	events []map[string]any
}

// Now is the clock reading of the Update cycle in progress.
func (s *State) Now() float64 { return s.now }

// Emit queues an event record, appended to events.jsonl after the state is
// written (in order, under the same lock). fields may set pkg, from, to and
// detail; anything else is appended after them.
func (s *State) Emit(kind string, fields map[string]any) {
	rec := map[string]any{"pkg": nil, "from": nil, "to": nil, "detail": ""}
	for k, v := range fields {
		rec[k] = v
	}
	rec["event"] = kind
	rec["at"] = s.now
	s.events = append(s.events, rec)
}

// NewPackage is a fresh pending package stamped with now.
func NewPackage(now float64) *Package {
	return &Package{Status: "pending", Since: now, Session: false}
}

// Pkg returns the package, creating a pending one.
func (s *State) Pkg(id string) *Package {
	if s.Packages == nil {
		s.Packages = map[string]*Package{}
	}
	p, ok := s.Packages[id]
	if !ok {
		p = NewPackage(s.now)
		s.Packages[id] = p
	}
	return p
}

func (s *State) clone() State {
	out := *s
	out.events = nil
	out.Packages = make(map[string]*Package, len(s.Packages))
	for id, p := range s.Packages {
		c := *p
		if p.NetworkDenials != nil {
			c.NetworkDenials = cloneValue(p.NetworkDenials).([]any)
		}
		c.Extra = cloneMap(p.Extra)
		out.Packages[id] = &c
	}
	out.Needs = append([]Need(nil), s.Needs...)
	if s.Stopped != nil {
		st := *s.Stopped
		out.Stopped = &st
	}
	if s.Held != nil {
		h := *s.Held
		out.Held = &h
	}
	out.Extra = cloneMap(s.Extra)
	return out
}

func cloneMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = cloneValue(v)
	}
	return out
}
