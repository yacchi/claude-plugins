package host

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
)

// Session is one package's registered session. Its registry record is the
// flat JSON object: "pkg", "worktree" and
// "terminal" (null when no agent is open) are fields here, every other key
// (repo, model, cls, state_dir, created, add_dirs, workspace, pane,
// herdr_session, net_mark, warm_session, warm_worktree, request_ids, ...)
// lives in Extra with its JSON value kept as decoded (numbers as
// json.Number, so a record survives a Go round trip byte for byte).
//
// Name is the host that owns the session ("herdr" or "orca"); it is not
// stored. Resumed is set by Prompt: the prompt went to an agent that was
// already running (not stored either).
type Session struct {
	Pkg, Name, Worktree, Terminal string
	Extra                         map[string]any
	Resumed                       bool
}

// The keys a record keeps outside Extra.
const (
	keyPkg      = "pkg"
	keyWorktree = "worktree"
	keyTerminal = "terminal"
)

// SessionFromRecord is the Session of one registry record.
func SessionFromRecord(name string, rec map[string]any) *Session {
	s := &Session{Name: name, Extra: map[string]any{}}
	for k, v := range rec {
		switch k {
		case keyPkg:
			s.Pkg, _ = v.(string)
		case keyWorktree:
			s.Worktree, _ = v.(string)
		case keyTerminal:
			s.Terminal, _ = v.(string)
		default:
			s.Extra[k] = v
		}
	}
	return s
}

// Record is the session as its registry stores it.
func (s *Session) Record() map[string]any {
	rec := make(map[string]any, len(s.Extra)+3)
	for k, v := range s.Extra {
		rec[k] = v
	}
	rec[keyPkg] = s.Pkg
	rec[keyWorktree] = s.Worktree
	rec[keyTerminal] = nullable(s.Terminal)
	return rec
}

// Get is the Extra value under k.
func (s *Session) Get(k string) (any, bool) {
	if s.Extra == nil {
		return nil, false
	}
	v, ok := s.Extra[k]
	return v, ok
}

// Set stores v under k in Extra.
func (s *Session) Set(k string, v any) {
	if s.Extra == nil {
		s.Extra = map[string]any{}
	}
	s.Extra[k] = v
}

// Delete removes k from Extra.
func (s *Session) Delete(k string) { delete(s.Extra, k) }

// Str is the Extra string under k ("" when absent, null or not a string).
func (s *Session) Str(k string) string {
	v, _ := s.Get(k)
	str, _ := v.(string)
	return str
}

// Truthy reports whether the Extra value under k is set.
func (s *Session) Truthy(k string) bool {
	v, _ := s.Get(k)
	return Truthy(v)
}

// Strings is the string elements of the Extra list under k.
func (s *Session) Strings(k string) []string {
	v, _ := s.Get(k)
	return stringList(v)
}

// Pop removes k and reports its truthiness (false when absent).
func (s *Session) Pop(k string) bool {
	v, ok := s.Get(k)
	if !ok {
		return false
	}
	delete(s.Extra, k)
	return Truthy(v)
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func stringList(v any) []string {
	switch x := v.(type) {
	case []string:
		return append([]string(nil), x...)
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if str, ok := e.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

// --- registry ----------------------------------------------------------------

// Registry is a host's session registry, `<state dir>/<Name>`: package id
// -> session record, plus host-private keys (herdr's "_warm"). Writes are
// serialized per Registry; the file is JSON with indent=1 and sorted
// keys plus a trailing newline, the layout earlier versions wrote.
type Registry struct {
	FileName string
	mu       sync.Mutex
}

// Path is the registry file of stateDir.
func (r *Registry) Path(stateDir string) string { return filepath.Join(stateDir, r.FileName) }

// Load is the registry of stateDir; a missing or unreadable file is empty.
func (r *Registry) Load(stateDir string) map[string]any { return LoadRegistryFile(r.Path(stateDir)) }

// Update loads, edits and rewrites the registry under the lock. fn returns
// false to skip the write.
func (r *Registry) Update(stateDir string, fn func(data map[string]any) bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	data := r.Load(stateDir)
	if !fn(data) {
		return nil
	}
	return WriteRegistryFile(r.Path(stateDir), data)
}

// Save stores s under its package id.
func (r *Registry) Save(stateDir string, s *Session) error {
	return r.Update(stateDir, func(data map[string]any) bool {
		data[s.Pkg] = s.Record()
		return true
	})
}

// Forget drops pkg's record.
func (r *Registry) Forget(stateDir, pkg string) error {
	return r.Update(stateDir, func(data map[string]any) bool {
		if v, ok := data[pkg]; !ok || v == nil {
			return false
		}
		delete(data, pkg)
		return true
	})
}

// LoadRegistryFile reads a registry file; anything but a JSON object is an
// empty registry.
func LoadRegistryFile(path string) map[string]any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var data map[string]any
	if dec.Decode(&data) != nil || data == nil {
		return map[string]any{}
	}
	return data
}

// EncodeRegistry renders data as JSON indented by one space (keys sorted)
// plus a trailing newline.
func EncodeRegistry(data map[string]any) ([]byte, error) {
	compact, err := fsutil.MarshalJSON(data)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, compact, "", " "); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// WriteRegistryFile atomically replaces path with data.
func WriteRegistryFile(path string, data map[string]any) error {
	b, err := EncodeRegistry(data)
	if err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(path, b, 0o644)
}

// ReadSession is the record of pkg in stateDir's registry file named
// fileName, without asking the host whether it is still alive (`wave mark
// --await` looks the session up this way in each host's registry).
func ReadSession(stateDir, fileName, hostName, pkg string) (*Session, bool) {
	rec, ok := LoadRegistryFile(filepath.Join(stateDir, fileName))[pkg].(map[string]any)
	if !ok {
		return nil, false
	}
	return SessionFromRecord(hostName, rec), true
}

// SortedDirs is the sorted, deduplicated absolute dirs with any dir under
// another listed dir dropped: --add-dir grants the whole subtree.
func SortedDirs(dirs []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, d := range dirs {
		if abs, err := filepath.Abs(d); err == nil {
			d = abs
		}
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	sort.Strings(out)
	// An ancestor always sorts before its descendants, so kept dirs are enough.
	kept := []string{}
	for _, d := range out {
		nested := false
		for _, a := range kept {
			prefix := a
			if !strings.HasSuffix(prefix, string(filepath.Separator)) {
				prefix += string(filepath.Separator)
			}
			if strings.HasPrefix(d, prefix) {
				nested = true
				break
			}
		}
		if !nested {
			kept = append(kept, d)
		}
	}
	return kept
}
