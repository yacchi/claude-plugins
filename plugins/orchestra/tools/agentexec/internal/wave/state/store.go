package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
)

// Store is every read and write of one wave's state.json + events.jsonl.
// Mutations hold an exclusive flock on <state>.lock for the whole
// read-mutate-write-append cycle, so threads and separate processes (the Go
// runner, `wave mark`) can share the file.
type Store struct {
	Path string
	// Clock returns epoch seconds; injectable for tests.
	Clock func() float64
}

func wallClock() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Open returns the store for a state file. The file need not exist yet
// (Init creates it).
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("state path is empty")
	}
	return &Store{Path: path, Clock: wallClock}, nil
}

func (s *Store) dir() string {
	abs, err := filepath.Abs(s.Path)
	if err != nil {
		return filepath.Dir(s.Path)
	}
	return filepath.Dir(abs)
}

// EventsPath is the events.jsonl beside the state file.
func (s *Store) EventsPath() string { return EventsPath(s.Path) }

// EventsPath is <state minus .json>.events.jsonl.
func EventsPath(statePath string) string {
	abs, err := filepath.Abs(statePath)
	if err != nil {
		abs = statePath
	}
	base := strings.TrimSuffix(filepath.Base(abs), ".json")
	return filepath.Join(filepath.Dir(abs), base+".events.jsonl")
}

func (s *Store) read() (State, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return State{}, err
	}
	st, err := Parse(data)
	if err != nil {
		return State{}, fmt.Errorf("%s: %w", s.Path, err)
	}
	return st, nil
}

func (s *Store) write(st *State) error {
	data, err := st.Marshal()
	if err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(s.Path, data, 0o600)
}

func (s *Store) appendEvents(events []map[string]any) error {
	if len(events) == 0 {
		return nil
	}
	f, err := os.OpenFile(s.EventsPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, ev := range events {
		line, err := fsutil.MarshalJSON(eventObject(ev))
		if err != nil {
			return err
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			return err
		}
	}
	return nil
}

// eventObject orders a record: at, pkg, from, to, event,
// detail, then any extra keys sorted.
func eventObject(ev map[string]any) map[string]any {
	o := map[string]any{}
	base := []string{"at", "pkg", "from", "to", "event", "detail"}
	for _, k := range base {
		o[k] = ev[k]
	}
	var extra []string
	for k := range ev {
		if !contains(base, k) {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		o[k] = ev[k]
	}
	return o
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

func (s *Store) lock() (func() error, error) {
	if err := os.MkdirAll(s.dir(), 0o755); err != nil {
		return nil, err
	}
	return fsutil.Lock(s.Path+".lock", fsutil.Exclusive, -1)
}

// Update runs one locked read-mutate-write(-append) cycle. fn mutates the
// state in place (State.Emit queues events); `updated` is bumped to the
// cycle's clock reading. An error from fn aborts the cycle without writing.
func (s *Store) Update(fn func(*State) error) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	st, err := s.read()
	if err != nil {
		return err
	}
	st.now = s.Clock()
	if err := fn(&st); err != nil {
		return err
	}
	st.Updated = st.now
	if err := s.write(&st); err != nil {
		return err
	}
	return s.appendEvents(st.events)
}

// Snapshot is a deep copy of the current state; a missing or unreadable
// file yields the zero State.
func (s *Store) Snapshot() State {
	st, err := s.read()
	if err != nil {
		return State{Packages: map[string]*Package{}}
	}
	return st.clone()
}

// Load is Snapshot reporting the read error.
func (s *Store) Load() (State, error) {
	st, err := s.read()
	if err != nil {
		return State{}, err
	}
	return st.clone(), nil
}

// Init creates state.json per the schema, or adds the missing package ids to
// an existing one (existing packages and top-level fields are untouched).
func (s *Store) Init(planPath string, packageIDs []string, integrationTask string) (State, error) {
	unlock, err := s.lock()
	if err != nil {
		return State{}, err
	}
	defer unlock()
	now := s.Clock()
	var st State
	if _, statErr := os.Stat(s.Path); statErr == nil {
		if st, err = s.read(); err != nil {
			return State{}, err
		}
		if st.Packages == nil {
			st.Packages = map[string]*Package{}
		}
		for _, id := range packageIDs {
			if _, ok := st.Packages[id]; !ok {
				st.Packages[id] = NewPackage(now)
			}
		}
	} else {
		task := integrationTask
		st = State{
			Version: 1, Plan: planPath, Integration: Integration{Task: &task},
			Packages: map[string]*Package{}, Needs: []Need{},
		}
		for _, id := range packageIDs {
			st.Packages[id] = NewPackage(now)
		}
	}
	st.Updated = now
	if err := s.write(&st); err != nil {
		return State{}, err
	}
	return st.clone(), nil
}

// Event appends one event record (and bumps `updated`). Errors
// are swallowed: an event is diagnostics, never a reason to stop the run.
func (s *Store) Event(kind string, fields map[string]any) {
	_ = s.Update(func(st *State) error {
		st.Emit(kind, fields)
		return nil
	})
}

// SetStatus moves a package to status, stamping `since` on a change and
// logging a status event; fn (optional) sets the package's other fields.
func (s *Store) SetStatus(pkg, status, detail string, fn func(*Package)) error {
	if !contains(Statuses, status) {
		return fmt.Errorf("unknown status: %q", status)
	}
	return s.Update(func(st *State) error {
		p := st.Pkg(pkg)
		old := p.Status
		if old != status {
			p.Since = st.now
		}
		p.Status = status
		p.Detail = detail
		if fn != nil {
			fn(p)
		}
		st.Emit("status", map[string]any{"pkg": pkg, "from": old, "to": status, "detail": detail})
		return nil
	})
}

// UpdatePackage mutates a package's non-status fields (no event). Status and
// since only move through SetStatus.
func (s *Store) UpdatePackage(pkg string, fn func(*Package)) error {
	return s.Update(func(st *State) error {
		fn(st.Pkg(pkg))
		return nil
	})
}

// AddNeed records a need and, for a package (pkg != ""), moves it to
// "needs". An empty pkg is a wave-level need.
func (s *Store) AddNeed(pkg, kind, detail string) error {
	if !contains(NeedKinds, kind) {
		return fmt.Errorf("unknown need kind: %q", kind)
	}
	return s.Update(func(st *State) error {
		n := Need{Kind: kind, Detail: detail, At: st.now}
		if pkg != "" {
			n.ID = &pkg
		}
		st.Needs = append(st.Needs, n)
		if pkg == "" {
			st.Emit("need", map[string]any{"detail": detail})
			return nil
		}
		p := st.Pkg(pkg)
		old := p.Status
		if old != "needs" {
			p.Since = st.now
		}
		p.Status = "needs"
		p.Detail = detail
		st.Emit("status", map[string]any{"pkg": pkg, "from": old, "to": "needs", "detail": detail})
		return nil
	})
}

// ClearNeed drops every need recorded for pkg ("" = wave-level ones).
func (s *Store) ClearNeed(pkg string) error {
	return s.Update(func(st *State) error {
		kept := make([]Need, 0, len(st.Needs))
		for _, n := range st.Needs {
			if (n.ID == nil && pkg == "") || (n.ID != nil && *n.ID == pkg) {
				continue
			}
			kept = append(kept, n)
		}
		st.Needs = kept
		return nil
	})
}

// ClearStopped forgets a previous stop.
func (s *Store) ClearStopped() error {
	return s.Update(func(st *State) error { st.Stopped = nil; return nil })
}

// SetHeld records why starts are held; nil clears the hold.
func (s *Store) SetHeld(h *Held) error {
	return s.Update(func(st *State) error { st.Held = h; return nil })
}

// SetIntegration replaces the integration record's task/path/base.
func (s *Store) SetIntegration(in Integration) error {
	return s.Update(func(st *State) error { st.Integration = in; return nil })
}

// SetWave sets the wave counter.
func (s *Store) SetWave(n int) error {
	return s.Update(func(st *State) error { st.Wave = n; return nil })
}

// MarkStopped records that the runner stopped, with a stop event.
func (s *Store) MarkStopped(reason string) error {
	return s.Update(func(st *State) error {
		st.Stopped = &Stopped{Reason: reason, At: st.now}
		st.Emit("stop", map[string]any{"detail": reason})
		return nil
	})
}
