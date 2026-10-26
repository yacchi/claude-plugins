// Package hookrun is what Claude Code's command hooks and the orchestra
// hooks module call through agent-exec: the SessionStart injection and the
// per-session stamps doctor reads.
package hookrun

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

const (
	// EventSessionStart: the SessionStart command hook ran for the session.
	EventSessionStart = "session-start"
	// EventModule: the hooks module saw the session's first main-loop turn.
	EventModule = "module"
)

// stampRetention bounds how long a session's stamp file is kept after its
// last write; pruning runs only when a new session first records, so the
// directory stays small without a separate sweeper.
const stampRetention = 14 * 24 * time.Hour

var (
	stampEvent   = regexp.MustCompile(`^[a-z][a-z0-9.-]*$`)
	stampSession = regexp.MustCompile(`[^A-Za-z0-9_-]`)
)

// stampFile is the on-disk shape of one session's stamps.
type stampFile struct {
	Session string           `json:"session"`
	Seen    map[string]int64 `json:"seen"`
}

// Dir is <paths.StateDir()>/hooks.
func Dir() string {
	return filepath.Join(paths.StateDir(), "hooks")
}

// Record notes that event ran in session at `at`. The session id is reduced
// to [A-Za-z0-9_-] so it cannot escape Dir; a later record of the same event
// overwrites the earlier time, and other events of the session are kept.
func Record(session, event string, at time.Time) error {
	id := stampSession.ReplaceAllString(session, "")
	if id == "" {
		return errors.New("hookrun: session id is empty after reduction")
	}
	if !stampEvent.MatchString(event) {
		return fmt.Errorf("hookrun: invalid event %q", event)
	}
	dir := Dir()
	if err := fsutil.EnsureDir(dir, 0o755); err != nil {
		return fmt.Errorf("hookrun: create %s: %w", dir, err)
	}
	file := filepath.Join(dir, id+".json")
	unlock, err := fsutil.Lock(file+".lock", fsutil.Exclusive, 2*time.Second)
	if err != nil {
		return fmt.Errorf("hookrun: lock %s: %w", file, err)
	}
	defer func() { _ = unlock() }()

	if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
		pruneStamps(dir, at)
	}

	v := stampFile{Session: id, Seen: map[string]int64{}}
	if err := fsutil.ReadJSONFile(file, &v); err != nil || v.Seen == nil {
		v = stampFile{Session: id, Seen: map[string]int64{}}
	}
	v.Seen[event] = at.Unix()
	if err := fsutil.AtomicWriteJSON(file, v, 0o644); err != nil {
		return fmt.Errorf("hookrun: write %s: %w", file, err)
	}
	return nil
}

// Seen is session's stamps (event -> unix seconds); empty, never nil, when
// the session has none or its file is unreadable.
func Seen(session string) map[string]int64 {
	id := stampSession.ReplaceAllString(session, "")
	var v stampFile
	if id == "" || fsutil.ReadJSONFile(filepath.Join(Dir(), id+".json"), &v) != nil || v.Seen == nil {
		return map[string]int64{}
	}
	return v.Seen
}

// pruneStamps unlinks the session files (and their locks) in dir last
// modified more than stampRetention before at. Errors are ignored: a stamp
// that survives one prune is removed by the next.
func pruneStamps(dir string, at time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := at.Add(-stampRetention)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		_ = os.Remove(path)
		_ = os.Remove(path + ".lock")
	}
}
