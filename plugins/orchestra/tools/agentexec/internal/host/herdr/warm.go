package herdr

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/trace"
)

// warmRecord is one model's warm reader record under WarmKey: session_id,
// worktree, base_ref, base_rev, files, requested, repo, task, created_at,
// last_used (and invalid once a fork dirtied its tree).
type warmRecord map[string]any

func (r warmRecord) str(k string) string {
	s, _ := r[k].(string)
	return s
}

// ConfigureWarm is the runner's hook: the repo-relative files the warm
// reader reads, and how many packages of the run start on each model.
func (e *Executor) ConfigureWarm(paths []string, modelCounts map[string]int) {
	e.warmMu.Lock()
	defer e.warmMu.Unlock()
	e.warmPaths = append([]string(nil), paths...)
	e.warmCounts = map[string]int{}
	for k, v := range modelCounts {
		e.warmCounts[k] = v
	}
}

func (e *Executor) warmRecords(stateDir string) map[string]any {
	records, _ := e.reg.Load(stateDir)[WarmKey].(map[string]any)
	if records == nil {
		return map[string]any{}
	}
	return records
}

// warmPut stores (or, with nil, drops) model's warm record.
func (e *Executor) warmPut(stateDir, model string, record warmRecord) {
	err := e.reg.Update(stateDir, func(data map[string]any) bool {
		records, _ := data[WarmKey].(map[string]any)
		if records == nil {
			records = map[string]any{}
		}
		if record == nil {
			delete(records, model)
		} else {
			records[model] = map[string]any(record)
		}
		if len(records) > 0 {
			data[WarmKey] = records
		} else {
			delete(data, WarmKey)
		}
		return true
	})
	if err != nil {
		e.warnf("registry: %s", err)
	}
}

func (e *Executor) warmDrop(repo, task string) {
	if _, err := isolate.Remove(repo, task, true, ""); err != nil {
		e.warnf("warm worktree removal failed: %s", err)
	}
}

// warmCleanup removes every warm worktree of this state dir (shutdown).
func (e *Executor) warmCleanup() {
	stateDir := e.StateDir()
	if stateDir == "" {
		return
	}
	for model, v := range e.warmRecords(stateDir) {
		if rec, ok := v.(map[string]any); ok {
			r := warmRecord(rec)
			if r.str("task") != "" && r.str("repo") != "" {
				e.warmDrop(r.str("repo"), r.str("task"))
			}
		}
		e.warmPut(stateDir, model, nil)
	}
}

// warmFor is the warm record to fork pkg's session from, or nil. Never
// fails: a failure is a `warm-failed` event and a fresh start.
func (e *Executor) warmFor(ctx context.Context, pkg, repo, baseRef, model, stateDir string, addDirs []string) warmRecord {
	warm := e.cfg.Sub("warm")
	get := func(k string) any {
		if warm == nil {
			return nil
		}
		v := warm[k]
		return v
	}
	trace.Lock(&e.warmMu, "herdr warm", trace.Attrs{Pkg: pkg})
	defer e.warmMu.Unlock()
	if !host.Truthy(get("enabled")) || len(e.warmPaths) == 0 ||
		float64(e.warmCounts[model]) < host.ConfigFloat(get("min_packages"), 2) {
		return nil
	}
	if e.warmFailed[model] {
		return nil
	}
	rec, err := e.ensureWarm(ctx, model, baseRef, e.warmPaths, repo, stateDir, addDirs)
	if err != nil {
		e.warmFailed[model] = true
		e.emit(pkg, "warm-failed", "", map[string]any{"model": model, "reason": host.AsNeed(err).Detail})
		return nil
	}
	return rec
}

func (e *Executor) warmUsable(r warmRecord, repo, baseRev string, paths []string, now float64) bool {
	if host.Truthy(r["invalid"]) || !sameList(r["requested"], paths) || !isDir(r.str("worktree")) {
		return false
	}
	ttl := e.cfg.Sub("warm")
	ttlSeconds := 3000.0
	if ttl != nil {
		v := ttl["ttl_seconds"]
		ttlSeconds = host.ConfigFloat(v, 3000)
	}
	if now-host.ConfigFloat(r["last_used"], 0) >= ttlSeconds {
		return false
	}
	if r.str("base_rev") == baseRev {
		return true
	}
	out, err := gitx.Run(repo, "diff", "--name-only", fmt.Sprint(r["base_rev"]), baseRev)
	if err != nil {
		return false
	}
	for _, c := range strings.Split(out, "\n") {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		for _, f := range paths {
			if c == f || strings.HasPrefix(c, strings.TrimRight(f, "/")+"/") {
				return false
			}
		}
	}
	return true
}

// sameList reports whether the stored "requested" list equals paths.
func sameList(v any, paths []string) bool {
	var stored []any
	switch x := v.(type) {
	case []any:
		stored = x
	case []string:
		for _, s := range x {
			stored = append(stored, s)
		}
	default:
		return false
	}
	if len(stored) != len(paths) {
		return false
	}
	for i, p := range paths {
		if s, ok := stored[i].(string); !ok || s != p {
			return false
		}
	}
	return true
}

// EnsureWarm makes sure model has a usable warm reader at baseRef.
func (e *Executor) EnsureWarm(ctx context.Context, model, baseRef string, paths []string, repo, stateDir string, addDirs []string) error {
	stateDir = e.adoptStateDir(stateDir)
	trace.Lock(&e.warmMu, "herdr warm", trace.Attrs{Pkg: "warm-" + model})
	defer e.warmMu.Unlock()
	_, err := e.ensureWarm(ctx, model, baseRef, paths, repo, stateDir, host.SortedDirs(addDirs))
	return err
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// ensureWarm is the warm reader record for model at baseRef: reused while
// it is fresh and its files are unchanged, otherwise rebuilt (a reader
// session that reads the files and writes a result). Fails on anything
// wrong; the caller falls back to a fresh start.
func (e *Executor) ensureWarm(ctx context.Context, model, baseRef string, paths []string, repo, stateDir string, addDirs []string) (rec warmRecord, err error) {
	paths = append([]string{}, paths...)
	baseRev := revParseCommit(repo, baseRef)
	if baseRev == "" {
		return nil, host.Need(host.KindError, "could not resolve "+baseRef+" to a commit")
	}
	now := float64(e.now().UnixNano()) / 1e9
	if rec, ok := e.warmRecords(stateDir)[model].(map[string]any); ok {
		r := warmRecord(rec)
		if e.warmUsable(r, repo, baseRev, paths, now) {
			r["last_used"] = now
			e.warmPut(stateDir, model, r)
			return r, nil
		}
		e.warmDrop(r.str("repo"), r.str("task"))
		e.warmPut(stateDir, model, nil)
	}
	tag := sanitize(model)
	if tag == "" {
		tag = "model"
	}
	task := "warm-" + tag
	end := trace.Begin(trace.KindExec, "herdr warm rebuild", trace.Attrs{Pkg: task})
	defer func() {
		if err != nil {
			end(1)
			return
		}
		end(0)
	}()
	created, err := isolate.Create(repo, task, isolate.CreateOptions{Backend: isolate.BackendGit, Onto: baseRev})
	if err != nil || (created.Status != "created" && created.Status != "exists") || !isDir(created.Path) {
		reason := created.Reason
		if err != nil {
			reason = err.Error()
		}
		return nil, host.Need(host.KindError, "could not create the warm worktree: "+orUnknown(reason))
	}
	built := false
	defer func() {
		if !built {
			e.warmDrop(repo, task)
		}
	}()
	var files []string
	for _, p := range paths {
		if st, err := os.Stat(filepath.Join(created.Path, p)); err == nil && st.Mode().IsRegular() {
			files = append(files, p)
		}
	}
	if len(files) == 0 {
		return nil, host.Need(host.KindError, "none of the warm files exist at "+baseRef)
	}
	sid := newUUID()
	resultPath := filepath.Join(stateDir, "orca", task+".result.json")
	_ = os.MkdirAll(filepath.Dir(resultPath), 0o755)
	_ = os.Remove(resultPath)
	s := &host.Session{Pkg: task, Name: e.Name(), Worktree: created.Path, Extra: map[string]any{
		"workspace": nil, "pane": nil, "repo": repo, "model": model, "state_dir": stateDir,
		"created": created.Status == "created", "herdr_session": e.sname,
		"add_dirs":          host.SortedDirs(append(append([]string{}, addDirs...), stateDir)),
		"claude_session_id": sid, "ephemeral": true,
	}}
	line := "Read each of these files with the Read tool, using exactly these relative paths, " +
		"and nothing else: " + strings.Join(files, ", ") + ". Do not run any other tool and do not edit " +
		`anything. Then write ` + resultPath + ` containing JSON {"status": "ok"}.`
	data, err := func() (map[string]any, error) {
		defer e.closeAgent(s)
		name, err := e.openAgent(ctx, s, false)
		if err != nil {
			return nil, err
		}
		if err := e.sendPrompt(ctx, s, name, line, resultPath); err != nil {
			return nil, err
		}
		return e.waitResult(ctx, s, resultPath,
			e.cfg.Seconds("startup_timeout", 90)+e.cfg.Seconds("task_timeout", 3600))
	}()
	if err != nil {
		return nil, err
	}
	if data["status"] != "ok" {
		return nil, host.Need(host.KindError, fmt.Sprintf("warm reader reported %v", data["status"]))
	}
	if out, err := gitx.Run(created.Path, "status", "--porcelain"); err != nil || strings.TrimSpace(out) != "" {
		return nil, host.Need(host.KindError, "warm worktree is not clean after reading")
	}
	rec = warmRecord{
		"session_id": sid, "worktree": created.Path, "base_ref": baseRef, "base_rev": baseRev,
		"files": files, "requested": paths, "repo": repo, "task": task,
		"created_at": now, "last_used": now,
	}
	e.warmPut(stateDir, model, rec)
	built = true
	return rec, nil
}

// warmLeak is the files a fork left dirty in the warm worktree (the record
// is then invalidated so the next package rebuilds it); nil when clean.
func (e *Executor) warmLeak(s *host.Session) []string {
	wt := s.Str("warm_worktree")
	if !isDir(wt) {
		return nil
	}
	out, err := gitx.Run(wt, "status", "--porcelain")
	if err != nil || strings.TrimSpace(out) == "" {
		return nil
	}
	stateDir := e.sessionStateDir(s)
	model := s.Str("model")
	if rec, ok := e.warmRecords(stateDir)[model].(map[string]any); ok && rec["session_id"] == s.Str("warm_session") {
		rec["invalid"] = true
		e.warmPut(stateDir, model, rec)
	}
	var files []string
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if len(line) > 3 {
			files = append(files, line[3:])
		} else {
			files = append(files, line)
		}
	}
	return files
}
