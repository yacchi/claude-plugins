package dispatch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// The per-task executor session store: a resumable executor (pi) keeps one
// session per orchestra task id, so a correction round continues the SAME
// conversation. It is advisory only: every read failure is "no stored
// session", which just means a cold start.

// SessionRecord is one (executor, task) record.
type SessionRecord struct {
	Executor, Task, SessionID, Workdir, Updated string
}

var sessionKeys = []string{"executor", "task", "session_id", "workdir", "updated"}

// SessionDir is the session store directory (a sibling of the ledger dir).
func SessionDir(cfg *config.Resolved) string { return filepath.Join(stateBase(cfg), "sessions") }

// sessionPath names the record; both components are sanitized so neither
// can escape the store.
func sessionPath(cfg *config.Resolved, executor, task string) (string, error) {
	e, err := isolate.SanitizeTaskID(executor)
	if err != nil {
		return "", err
	}
	t, err := isolate.SanitizeTaskID(task)
	if err != nil {
		return "", err
	}
	return filepath.Join(SessionDir(cfg), e+"-"+t+".json"), nil
}

// ReadSession returns the stored record, or false for every failure mode
// (missing, unreadable, malformed, a required key missing or not a
// non-empty string).
func ReadSession(cfg *config.Resolved, executor, task string) (SessionRecord, bool) {
	path, err := sessionPath(cfg, executor, task)
	if err != nil {
		return SessionRecord{}, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return SessionRecord{}, false
	}
	v, err := jsonx.Decode(raw)
	if err != nil {
		return SessionRecord{}, false
	}
	o, ok := v.(*jsonx.Object)
	if !ok || o == nil {
		return SessionRecord{}, false
	}
	vals := make([]string, len(sessionKeys))
	for i, k := range sessionKeys {
		x, _ := o.Get(k)
		s, isStr := x.(string)
		if !isStr || s == "" {
			return SessionRecord{}, false
		}
		vals[i] = s
	}
	return SessionRecord{vals[0], vals[1], vals[2], vals[3], vals[4]}, true
}

// WriteSession creates or replaces the record (dir 0700, file 0600) through
// an O_EXCL temp file renamed over the target, so a reader sees the old or
// the new record, never a torn one.
func WriteSession(cfg *config.Resolved, executor, task, sessionID, workdir string) (SessionRecord, error) {
	path, err := sessionPath(cfg, executor, task)
	if err != nil {
		return SessionRecord{}, err
	}
	if err := privateDir(SessionDir(cfg)); err != nil {
		return SessionRecord{}, err
	}
	rec := SessionRecord{
		Executor: executor, Task: task, SessionID: sessionID, Workdir: abs(workdir),
		Updated: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}
	payload, err := compactJSON(jsonx.NewObject().
		Set("executor", rec.Executor).Set("task", rec.Task).Set("session_id", rec.SessionID).
		Set("workdir", rec.Workdir).Set("updated", rec.Updated))
	if err != nil {
		return SessionRecord{}, err
	}
	for attempt := range 10 {
		tmp := fmt.Sprintf("%s.tmp-%d-%d", path, os.Getpid(), attempt)
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return SessionRecord{}, err
		}
		_, werr := f.Write(payload)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr == nil {
			werr = os.Rename(tmp, path)
		}
		if werr != nil {
			os.Remove(tmp)
			return SessionRecord{}, werr
		}
		return rec, nil
	}
	return SessionRecord{}, fmt.Errorf("could not write session record: %s", path)
}

// sameWorkdir: both paths name the same directory once symlinks are
// resolved (macOS /tmp vs /private/tmp is not a different tree).
func sameWorkdir(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return paths.RealPath(abs(a)) == paths.RealPath(abs(b))
}
