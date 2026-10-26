package dispatch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// The per-task executor session store: a resumable executor (pi) keeps one
// session per orchestra task id, so a correction round continues the SAME
// conversation. It is advisory only: every read failure is "no stored
// session", which just means a cold start.

// SessionRecord is one (executor, task) record. ContextTokens is the last
// run's context (the tokens its final request re-read); 0 means unknown, as
// in records written before it was stored.
type SessionRecord struct {
	Executor, Task, SessionID, Workdir, Updated string
	ContextTokens                               int64
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
	v, err := fsutil.DecodeJSON(raw)
	if err != nil {
		return SessionRecord{}, false
	}
	o, ok := v.(map[string]any)
	if !ok || o == nil {
		return SessionRecord{}, false
	}
	vals := make([]string, len(sessionKeys))
	for i, k := range sessionKeys {
		x := o[k]
		s, isStr := x.(string)
		if !isStr || s == "" {
			return SessionRecord{}, false
		}
		vals[i] = s
	}
	ctxTokens, _ := resumeGateCount(o["context_tokens"])
	return SessionRecord{
		Executor: vals[0], Task: vals[1], SessionID: vals[2], Workdir: vals[3], Updated: vals[4],
		ContextTokens: ctxTokens,
	}, true
}

// WriteSession creates or replaces the record (dir 0700, file 0600) through
// an O_EXCL temp file renamed over the target, so a reader sees the old or
// the new record, never a torn one.
func WriteSession(cfg *config.Resolved, executor, task, sessionID, workdir string, contextTokens int64) (SessionRecord, error) {
	path, err := sessionPath(cfg, executor, task)
	if err != nil {
		return SessionRecord{}, err
	}
	if err := privateDir(SessionDir(cfg)); err != nil {
		return SessionRecord{}, err
	}
	rec := SessionRecord{
		Executor: executor, Task: task, SessionID: sessionID, Workdir: abs(workdir),
		Updated: time.Now().UTC().Format("2006-01-02T15:04:05Z"), ContextTokens: contextTokens,
	}
	payload, err := compactJSON(map[string]any{
		"executor":       rec.Executor,
		"task":           rec.Task,
		"session_id":     rec.SessionID,
		"workdir":        rec.Workdir,
		"updated":        rec.Updated,
		"context_tokens": rec.ContextTokens,
	})
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

// resumeGateCount reads a non-negative token count from a decoded JSON
// value; false for anything else, so a missing or malformed count is unknown.
func resumeGateCount(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, n >= 0
	case float64:
		return int64(n), n >= 0
	}
	return 0, false
}

// resumeGateContext is the last request's context the executor reported in
// the result's usage; false when it reported none.
func resumeGateContext(result map[string]any) (int64, bool) {
	usage, ok := get(result, "usage").(map[string]any)
	if !ok {
		return 0, false
	}
	return resumeGateCount(usage["last_context_tokens"])
}
