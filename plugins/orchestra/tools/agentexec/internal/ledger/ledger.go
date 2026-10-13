// Package ledger is the numbers-only run ledger: one JSONL file per run under
// <ledger dir>/<session id>/, written by dispatch and read by usage. Only
// allowlisted enums and non-negative integers ever reach the disk.
package ledger

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
)

var (
	// RunIDPattern is what a --run-id (and a stored run file name) must match.
	RunIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	// SessionPattern is what a session id (a directory name) must match.
	SessionPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
	// OrdinalPattern is a per-session execution ordinal used as a run selector.
	OrdinalPattern = regexp.MustCompile(`^[0-9]{1,4}$`)

	modelPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,64}$`)
	filePattern  = regexp.MustCompile(`^([0-9]+)-([A-Za-z0-9_-]{1,64})\.jsonl$`)
)

const (
	maxLines         = 10000
	defaultRetention = 30
)

var (
	executors = []string{"claude", "pi"}
	classes   = []string{"light", "standard", "deep", "independent-review"}
	statuses  = []string{"ok", "error", "unavailable", "needs-permission", "runaway"}

	intKeys = []string{
		"input_tokens", "output_tokens", "cached_input_tokens",
		"api_duration_ms", "session_duration_ms",
		"cache_write_input_tokens", "reasoning_output_tokens",
		"cost_micro_usd", "sandbox_denials", "sandbox_granted",
	}
)

func oneOf(v any, allowed []string) (string, bool) {
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	for _, a := range allowed {
		if a == s {
			return s, true
		}
	}
	return "", false
}

// nonNegInt is Python's _is_nonneg_int: an integer (never a bool) >= 0.
func nonNegInt(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, x >= 0
	case int:
		return int64(x), x >= 0
	}
	return 0, false
}

// Sanitize allowlists and validates one record; nil means it carries nothing
// worth keeping and must be dropped.
func Sanitize(raw *jsonx.Object) *jsonx.Object {
	if raw == nil {
		return nil
	}
	out := jsonx.NewObject()
	if v, _ := raw.Get("ts"); v != nil {
		if ts, ok := v.(string); ok && ts != "" && !strings.ContainsAny(ts, "\r\n") {
			out.Set("ts", ts)
		}
	}
	for _, f := range []struct {
		key     string
		allowed []string
	}{{"executor", executors}, {"cls", classes}, {"status", statuses}} {
		v, _ := raw.Get(f.key)
		if s, ok := oneOf(v, f.allowed); ok {
			out.Set(f.key, s)
		}
	}
	if v, _ := raw.Get("model"); v != nil {
		if m, ok := v.(string); ok && modelPattern.MatchString(m) {
			out.Set("model", m)
		}
	}
	for _, key := range intKeys {
		v, _ := raw.Get(key)
		if n, ok := nonNegInt(v); ok {
			out.Set(key, n)
		}
	}
	for _, key := range []string{"executor", "cls", "status", "model"} {
		if _, ok := out.Get(key); ok {
			return out
		}
	}
	return nil
}

// sandboxCounts is the denial/grant counts (never paths) of a result's
// sandbox block; ok is false when the result has no sandbox denials field.
func sandboxCounts(result *jsonx.Object) (denials, granted int, ok bool) {
	sb := result.Object("sandbox")
	if sb == nil {
		return 0, 0, false
	}
	d, has := sb.Get("denials")
	if !has {
		return 0, 0, false
	}
	dl, _ := d.([]any)
	g, _ := sb.Get("granted")
	gl, _ := g.([]any)
	return len(dl), len(gl), true
}

// BuildRecord flattens a normalized capture result into a (pre-sanitize)
// ledger record.
func BuildRecord(executor, model, cls string, result *jsonx.Object) *jsonx.Object {
	rec := jsonx.NewObject()
	rec.Set("executor", executor).Set("model", model).Set("cls", cls)
	status, _ := result.Get("status")
	rec.Set("status", status)
	if d, g, ok := sandboxCounts(result); ok {
		rec.Set("sandbox_denials", int64(d)).Set("sandbox_granted", int64(g))
	}
	usage := result.Object("usage")
	if usage == nil {
		return rec
	}
	for _, key := range []string{"api_duration_ms", "session_duration_ms", "cost_micro_usd"} {
		if v, ok := usage.Get(key); ok {
			rec.Set(key, v)
		}
	}
	if tokens := usage.Object("tokens"); tokens != nil {
		for _, key := range []string{
			"input_tokens", "output_tokens", "cached_input_tokens",
			"cache_write_input_tokens", "reasoning_output_tokens",
		} {
			if v, ok := tokens.Get(key); ok {
				rec.Set(key, v)
			}
		}
	}
	return rec
}

// Dir is the ledger directory: ledger.dir, else "runs" beside the telemetry
// directory (the pre-ledger-config location, for partial configs).
func Dir(cfg *config.Resolved) string {
	if d, ok := cfg.String("ledger", "dir"); ok && d != "" {
		return fsutil.ExpandUser(d)
	}
	tdir, _ := cfg.String("telemetry", "dir")
	if tdir == "" {
		tdir = "~/.claude/orchestra/telemetry"
	}
	return filepath.Join(filepath.Dir(fsutil.ExpandUser(tdir)), "runs")
}

var (
	retentionMu  sync.Mutex
	retentionRan bool
)

// ResetRetention re-arms the once-per-process retention sweep (tests).
func ResetRetention() {
	retentionMu.Lock()
	retentionRan = false
	retentionMu.Unlock()
}

// sweepRetention runs the ledger part of the retention sweep at most once per
// process. The token, session-store and detach files are dispatch's.
func sweepRetention(cfg *config.Resolved) {
	retentionMu.Lock()
	if retentionRan {
		retentionMu.Unlock()
		return
	}
	retentionRan = true
	retentionMu.Unlock()
	days, ok := cfg.Int("ledger", "retention_days")
	if !ok || days <= 0 {
		days = defaultRetention
	}
	SweepOlder(Dir(cfg), time.Now().Add(-time.Duration(days)*24*time.Hour))
}

// SweepOlder removes *.jsonl and .ordinal-* files older than cutoff from dir
// and from its immediate subdirectories. Best effort.
func SweepOlder(dir string, cutoff time.Time) {
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return
	}
	dirs := []string{dir}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if fi, err := os.Stat(filepath.Join(dir, e.Name())); err == nil && fi.IsDir() {
			dirs = append(dirs, filepath.Join(dir, e.Name()))
		}
	}
	for _, d := range dirs {
		names, err := os.ReadDir(d)
		if err != nil {
			return
		}
		for _, e := range names {
			name := e.Name()
			if !strings.HasSuffix(name, ".jsonl") && !strings.HasPrefix(name, ".ordinal-") {
				continue
			}
			path := filepath.Join(d, name)
			if fi, err := os.Stat(path); err == nil && fi.Mode().IsRegular() && fi.ModTime().Before(cutoff) {
				_ = os.Remove(path)
			}
		}
	}
}

// Append writes one sanitized, compact record for the current session
// ($CLAUDE_CODE_SESSION_ID) under runID ("" = the session's no.run file).
// It never fails the caller: anything wrong means nothing is written.
func Append(rec *jsonx.Object, runID string, cfg *config.Resolved) {
	if _, set := cfg.Lookup("ledger", "enabled"); set {
		if enabled, _ := cfg.Bool("ledger", "enabled"); !enabled {
			return // anything but a real true switches the ledger off
		}
	}
	session := os.Getenv("CLAUDE_CODE_SESSION_ID")
	if strings.TrimSpace(session) == "" || session == "." || session == ".." ||
		!SessionPattern.MatchString(session) {
		return
	}
	if runID != "" && !RunIDPattern.MatchString(runID) {
		return
	}
	clean := Sanitize(rec)
	if clean == nil {
		return
	}
	if _, ok := clean.Get("ts"); !ok {
		clean.Set("ts", fsutil.NowISO())
	}
	dir := Dir(cfg)
	if err := private(dir); err != nil {
		return
	}
	sessionDir := filepath.Join(dir, session)
	if err := private(sessionDir); err != nil {
		return
	}
	name := "no.run.jsonl"
	if runID != "" {
		name = runFilename(sessionDir, runID)
	}
	path := filepath.Join(sessionDir, name)
	if err := fsutil.AppendJSONLine(path, clean, 0o666); err != nil {
		return
	}
	enforceCap(path)
	sweepRetention(cfg)
}

// private creates dir with mode 0700 (parents get the default mode, as
// os.makedirs(mode=) does) and re-applies the mode to an existing one.
func private(dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o777); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// enforceCap keeps only the newest maxLines lines of path.
func enforceCap(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.SplitAfter(string(data), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > maxLines {
		_ = os.WriteFile(path, []byte(strings.Join(lines[len(lines)-maxLines:], "")), 0o666)
	}
}

// findRunFile is the existing "NNN-<run>.jsonl" of a run in the session dir.
func findRunFile(sessionDir, runID string) (string, bool) {
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if m := filePattern.FindStringSubmatch(e.Name()); m != nil && m[2] == runID {
			return e.Name(), true
		}
	}
	return "", false
}

// runFilename finds or atomically allocates the execution ordinal of a run
// (O_EXCL sentinels make concurrent writers pick distinct ordinals).
func runFilename(sessionDir, runID string) string {
	unprefixed := runID + ".jsonl"
	if _, err := os.ReadDir(sessionDir); err != nil {
		return unprefixed
	}
	if name, ok := findRunFile(sessionDir, runID); ok {
		return name
	}
	if fi, err := os.Stat(filepath.Join(sessionDir, unprefixed)); err == nil && fi.Mode().IsRegular() {
		return unprefixed
	}
	claim := filepath.Join(sessionDir, fmt.Sprintf(".ordinal-run-%s.claim", runID))
	f, err := os.OpenFile(claim, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if !os.IsExist(err) {
			return unprefixed
		}
		for i := 0; i < 20; i++ {
			if name, ok := findRunFile(sessionDir, runID); ok {
				return name
			}
			time.Sleep(10 * time.Millisecond)
		}
		return unprefixed
	}
	f.Close()
	ordinal := 1
	for {
		sentinel := filepath.Join(sessionDir, fmt.Sprintf(".ordinal-%03d.reserve", ordinal))
		sf, err := os.OpenFile(sentinel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			sf.Close()
			break
		}
		if !os.IsExist(err) {
			return unprefixed
		}
		ordinal++
	}
	if err := os.WriteFile(claim, []byte(fmt.Sprint(ordinal)), 0o600); err != nil {
		return unprefixed
	}
	return fmt.Sprintf("%03d-%s.jsonl", ordinal, runID)
}
