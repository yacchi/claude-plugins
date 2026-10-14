package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/checks"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/cooldown"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/integrate"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/sandbox"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/state"
)

const (
	// integrateVerifyTimeout bounds the integration gate.
	integrateVerifyTimeout = time.Hour
	// fullBisectMax is the bisect-and-revert rounds one red --full may spend.
	fullBisectMax = 3
)

// progressWriter turns integrate's JSON progress lines into wave events,
// mapping task ids back to package ids.
type progressWriter struct {
	r     *runner
	byTsk map[string]string
	tasks bool // map the "tasks" list too (integrate-start)
}

func (w progressWriter) Write(p []byte) (int, error) {
	for _, line := range splitLines(string(p)) {
		v, err := fsutil.DecodeJSON([]byte(line))
		if err != nil {
			continue
		}
		rec := asMap(v)
		detail := map[string]any{}
		if d := asMap(rec["detail"]); d != nil {
			for k, val := range d {
				detail[k] = val
			}
		}
		if w.tasks {
			if list, ok := detail["tasks"].([]any); ok {
				mapped := make([]any, len(list))
				for i, t := range list {
					mapped[i] = t
					if pid, ok := w.byTsk[str(t)]; ok {
						mapped[i] = pid
					}
				}
				detail["tasks"] = mapped
			}
		}
		pkg := str(rec["pkg"])
		if pid, ok := w.byTsk[pkg]; ok {
			pkg = pid
		}
		w.r.emit(str(rec["event"]), pkg, detail)
	}
	return len(p), nil
}

func (r *runner) sanitized(pid string) string {
	t, err := isolate.SanitizeTaskID(r.task(pid))
	if err != nil {
		return r.task(pid)
	}
	return t
}

func (r *runner) taskMap() map[string]string {
	m := map[string]string{}
	for _, pkg := range r.plan.Packages {
		m[r.sanitized(pkg.ID)] = pkg.ID
	}
	return m
}

func (r *runner) defaultGate(since string) string {
	quoted := make([]string, len(r.deps.AgentExec))
	for i, a := range r.deps.AgentExec {
		quoted[i] = sandbox.ShellQuote(a)
	}
	return fmt.Sprintf("%s check --path . --since %s --baseline --text; rc=$?; "+
		"[ $rc -eq 0 ] || [ $rc -eq 4 ]", strings.Join(quoted, " "), sandbox.ShellQuote(since))
}

// commitFor is the integrate commit of task since since.
func (r *runner) commitFor(task, since string) *string {
	out, err := gitRun(r.intPath, "log", "--format=%H %s", since+"..HEAD")
	if err != nil {
		return nil
	}
	for _, line := range splitLines(out) {
		sha, rest, _ := strings.Cut(line, " ")
		if rest == "orchestra integrate "+task {
			return &sha
		}
	}
	return nil
}

// integrateReady integrates every ready package (plan order, skip on
// conflict, gate + bisect) and returns how many applied.
func (r *runner) integrateReady() int {
	st := r.store.Snapshot()
	var ready []string
	for _, pkg := range r.plan.Packages {
		if p := st.Packages[pkg.ID]; p != nil && p.Status == "ready" {
			ready = append(ready, pkg.ID)
		}
	}
	if len(ready) == 0 {
		return 0
	}
	since := r.intHead()
	gate := r.opts.Gate
	if gate == "" && r.verifyMode == "gated" {
		gate = r.defaultGate(since)
	}
	tasks := make([]string, len(ready))
	bySanitized := map[string]string{}
	for i, pid := range ready {
		tasks[i] = r.task(pid)
		bySanitized[r.sanitized(pid)] = pid
	}
	res, err := integrate.Integrate(r.root, tasks, integrate.Options{
		Onto: r.base, Into: r.into, OnConflict: integrate.OnConflictSkip, Verify: gate,
		VerifyTimeout: integrateVerifyTimeout, Bisect: true,
		Progress: progressWriter{r: r, byTsk: bySanitized, tasks: true},
	})
	if err != nil {
		res = integrate.Result{Status: "error", Note: err.Error()}
	}
	r.event("integrate", "", fmt.Sprintf("%s: %s", res.Status, res.Note))
	if res.Status == "error" {
		note := res.Note
		if note == "" {
			note = "integrate failed"
		}
		for _, pid := range ready {
			r.need(pid, "dispatch-error", note)
		}
		return 0
	}
	byTask := map[string]integrate.TaskResult{}
	for _, t := range res.Tasks {
		byTask[t.Task] = t
	}
	integrated := 0
	for _, pid := range ready {
		entry, ok := byTask[r.sanitized(pid)]
		status := "None"
		if ok {
			status = entry.Status
		}
		note := ""
		if p := st.Packages[pid]; p != nil && strings.HasPrefix(p.Detail, fastNote) {
			note = p.Detail
		}
		switch status {
		case "applied":
			commit := r.commitFor(r.task(pid), since)
			_ = r.store.SetStatus(pid, "integrated", note, func(p *state.Package) { p.Commit = commit })
			integrated++
			r.hostClose(pid)
		case "empty":
			_ = r.store.SetStatus(pid, "integrated", "changed nothing", nil)
			r.hostClose(pid)
		case "conflicted":
			files := []any{}
			for _, c := range entry.Conflicts {
				if c.File != "" {
					files = append(files, c.File)
				}
			}
			r.need(pid, "conflict", sortedJSON(map[string]any{"files": files, "stage": "integrate"}))
		case "reverted":
			excerpt := ""
			if entry.VerifyExcerpt != nil {
				excerpt = *entry.VerifyExcerpt
			}
			r.need(pid, "post-integration", excerpt)
		default:
			r.need(pid, "dispatch-error", "integrate reported "+status)
		}
	}
	if v := res.Verify; v != nil {
		if v.Dirtied > 0 {
			r.emit("integration-dirty", "", map[string]any{"files": v.Dirtied, "after": "gate"})
		}
		if v.Status == "fail" {
			if v.Baseline == "fail" {
				r.stop("integration tree red before this wave")
			} else {
				r.stop("integration tree red after bisect")
			}
		}
	}
	r.mu.Lock()
	r.integratedSinceFull += integrated
	r.mu.Unlock()
	return integrated
}

// --- green ---------------------------------------------------------------------------

func (r *runner) greenFile() string { return filepath.Join(r.stateDir, "green.json") }

func (r *runner) lastGreen() string {
	raw, err := os.ReadFile(r.greenFile())
	if err != nil {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	return str(m["sha"])
}

func (r *runner) recordGreen(sha string) {
	if sha == "" {
		return
	}
	b, err := fsutil.MarshalJSON(map[string]any{"sha": sha, "at": r.clock()})
	if err == nil {
		_ = writeFileAtomic(r.greenFile(), append(b, '\n'))
	}
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// greenTree checks the green SHA out in a separate worktree (created once,
// reused after).
func (r *runner) greenTree(sha string) bool {
	if sha == "" {
		return false
	}
	if !isDir(r.greenPath) {
		_ = os.MkdirAll(filepath.Dir(r.greenPath), 0o755)
		if _, err := gitRun(r.root, "worktree", "add", "--detach", r.greenPath, sha); err != nil {
			return false
		}
		for _, rel := range isolate.DetectCarryDirs(r.root, nil) {
			_ = isolate.CopyTreeFast(filepath.Join(r.root, rel), filepath.Join(r.greenPath, rel))
		}
		return true
	}
	if _, err := gitRun(r.greenPath, "checkout", "-q", "--detach", sha); err != nil {
		return false
	}
	_, _ = gitRun(r.greenPath, "clean", "-fdq")
	return true
}

func (r *runner) onGreen(sha string) {
	command := r.opts.OnGreen
	if command == "" {
		return
	}
	if !r.greenTree(sha) {
		r.emit("on-green", "", map[string]any{"sha": sha, "exit": nil, "seconds": 0.0})
		return
	}
	started := time.Now()
	command = strings.ReplaceAll(command, "{sha}", sha)
	exit := runPlain(r.greenPath, command, r.opts.FullTimeout, append(os.Environ(), "WAVE_GREEN_SHA="+sha))
	_, _ = gitRun(r.greenPath, "reset", "-q", "--hard", "HEAD")
	_, _ = gitRun(r.greenPath, "clean", "-fdq")
	var exitV any
	if exit != nil {
		exitV = *exit
	}
	r.emit("on-green", "", map[string]any{"sha": sha, "exit": exitV, "seconds": time.Since(started).Seconds()})
}

// runPlain runs command via /bin/sh -c (output discarded); a timeout kills
// it and reports nil.
func runPlain(dir, command string, timeout time.Duration, env []string) *int {
	c := exec.Command("/bin/sh", "-c", command)
	c.Dir, c.Env = dir, env
	if err := c.Start(); err != nil {
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	if timeout > 0 {
		select {
		case <-done:
		case <-time.After(timeout):
			_ = c.Process.Kill()
			<-done
			return nil
		}
	} else {
		<-done
	}
	code := c.ProcessState.ExitCode()
	return &code
}

func (r *runner) fullGreen() {
	sha := r.intHead()
	r.recordGreen(sha)
	r.onGreen(sha)
}

// --- usage-limit reset -----------------------------------------------------------------

// waitForReset waits for the earliest cooldown expiry of the exhausted
// executors (else 300 s); false when the run stopped instead.
func (r *runner) waitForReset() bool {
	now := r.clock()
	until := now + 300
	if cfg := r.config(); cfg != nil {
		active := cooldown.Active(cfg, time.Unix(0, int64(now*1e9)))
		first := true
		for _, name := range r.exhaustedList() {
			if e, ok := active[name]; ok && (first || e.Until < until) {
				until, first = e.Until, false
			}
		}
	}
	if r.opts.StopAt != nil && *r.opts.StopAt <= until {
		r.stop("stop-at reached")
		return false
	}
	r.emit("resume-wait", "", map[string]any{"until": until, "reason": "executor unavailable"})
	for r.clock() < until {
		if reason := r.stopCheck(); reason != "" {
			r.stop(reason)
			return false
		}
		r.sleep(min(30, max(0, until-r.clock())))
	}
	r.mu.Lock()
	r.exhausted = map[string]bool{}
	r.unavailable = false
	r.mu.Unlock()
	return true
}

// --- --full --------------------------------------------------------------------------

// fullFlaky re-runs a red full verification's failing files alone with
// --full-retry; true when that passed (the red was flaky).
func (r *runner) fullFlaky(res integrate.VerifyResult) bool {
	if r.opts.FullRetry == "" {
		return false
	}
	c := r.checksConfig()
	status, failed := checks.RerunFailedAlone(res.Excerpt, r.opts.FullRetry, r.intPath, c.slotDir, c.maxParallel, r.opts.FullTimeout)
	r.cleanIntegration("full-retry")
	if status != "pass" {
		return false
	}
	files := make([]any, len(failed))
	for i, f := range failed {
		files[i] = f
	}
	r.emit("flaky", "", map[string]any{"check": "full", "files": files})
	return true
}

func ensureVerify(f *ensureFail) integrate.VerifyResult {
	return integrate.VerifyResult{Status: "fail", Exit: f.result.Exit, Seconds: f.result.Seconds, Excerpt: f.excerpt}
}

func exitValue(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// full runs --full at the integration tip. Red goes through the
// --full-retry flaky filter, then bisect-and-revert; it returns the
// packages a revert sent back to pending.
func (r *runner) full() []string {
	r.mu.Lock()
	r.integratedSinceFull = 0
	r.mu.Unlock()
	r.emit("full-start", "", map[string]any{})
	var res integrate.VerifyResult
	ensureFailed := false
	if f := r.ensureFailure(r.intPath); f != nil {
		res, ensureFailed = ensureVerify(f), true
	} else {
		res = integrate.RunVerify(r.intPath, r.opts.Full, r.opts.FullTimeout)
	}
	if res.Dirtied > 0 {
		r.emit("integration-dirty", "", map[string]any{"files": res.Dirtied, "after": "full"})
	}
	r.emit("full-end", "", map[string]any{"status": res.Status, "seconds": res.Seconds})
	r.event("full", "", fmt.Sprintf("%s exit=%s\n%s", res.Status, pyStr(exitValue(res.Exit)), res.Excerpt))
	if res.Status == "pass" || (!ensureFailed && r.fullFlaky(res)) {
		r.fullGreen()
		return nil
	}
	return r.fullBisect()
}

// fullCandidates is the `orchestra integrate <task>` commits in
// green..HEAD, oldest first; commits a later `orchestra revert <task>`
// undid (the gate's own bisect) are not candidates.
func (r *runner) fullCandidates(green string) []integrate.AppliedCommit {
	out, err := gitRun(r.intPath, "log", "--reverse", "--format=%H %s", green+"..HEAD")
	if err != nil {
		return nil
	}
	known := r.taskMap()
	var cands []integrate.AppliedCommit
	for _, line := range splitLines(out) {
		sha, subject, _ := strings.Cut(line, " ")
		if task, ok := strings.CutPrefix(subject, "orchestra integrate "); ok {
			if _, isPkg := known[task]; isPkg {
				cands = append(cands, integrate.AppliedCommit{Task: task, SHA: sha})
			}
			continue
		}
		if task, ok := strings.CutPrefix(subject, "orchestra revert "); ok {
			if _, isPkg := known[task]; !isPkg {
				continue
			}
			for i := len(cands) - 1; i >= 0; i-- {
				if cands[i].Task == task {
					cands = append(cands[:i], cands[i+1:]...)
					break
				}
			}
		}
	}
	return cands
}

// restoreIntegration never leaves the integration tree detached,
// mid-revert or dirty.
func (r *runner) restoreIntegration(branch string) {
	_, _ = gitRun(r.intPath, "revert", "--quit")
	if branch != "" {
		if out, err := gitRun(r.intPath, "rev-parse", "--abbrev-ref", "HEAD"); err != nil || strings.TrimSpace(out) != branch {
			_, _ = gitRun(r.intPath, "checkout", "-q", "-f", branch)
		}
	}
	r.cleanIntegration("full-bisect")
}

// postFullRevert: a red --full was bisected to pid and its commit reverted;
// re-dispatch it with a correction, or hand it over the second time.
func (r *runner) postFullRevert(pid, excerpt string) bool {
	failed := checks.ParseFailedFiles(excerpt)
	excerpt = checks.FailureExcerpt(excerpt)
	names := strings.Join(failed, ", ")
	if names == "" {
		names = "(none parsed)"
	}
	for _, ev := range state.ReadEvents(r.statePath, -1) {
		get := func(k string) any { return ev[k] }
		if get("event") == "status" && get("pkg") == pid && get("to") == "pending" && get("detail") == postFullDetail {
			r.need(pid, "post-integration", fmt.Sprintf("full verification red after "+
				"integration (reverted twice); failing tests: %s\n%s", names, excerpt))
			return false
		}
	}
	text := fmt.Sprintf(postFullHeader, pid) + "\n\nYour integrated change broke the full " +
		"verification after integration, so it was reverted from the " +
		"integration tree. Failing tests: " + names + "\n\nExcerpt:\n\n" + excerpt +
		"\n\nFix it inside files_owned; do not commit.\n"
	if err := os.WriteFile(r.correctionPath(pid), []byte(text), 0o644); err != nil {
		r.need(pid, "post-integration", excerpt)
		return false
	}
	_ = r.store.SetStatus(pid, "pending", postFullDetail, func(p *state.Package) { p.Commit = nil })
	return true
}

// fullBisect bisects a red --full over the integrate commits since the
// last green SHA, reverting culprits; it returns the ids sent back to
// pending.
func (r *runner) fullBisect() []string {
	green := r.lastGreen()
	if green == "" {
		green = r.base
	}
	branch := ""
	if out, err := gitRun(r.intPath, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		if b := strings.TrimSpace(out); b != "HEAD" {
			branch = b
		}
	}
	if branch == "" {
		r.stop("full verification red")
		return nil
	}
	candidates := r.fullCandidates(green)
	byTask := r.taskMap()
	lastEnsure := false
	res, err := integrate.Bisect(integrate.BisectInput{
		Root: r.root, Worktree: r.intPath, Branch: branch, BaseSHA: green, Applied: candidates,
		Verify: r.opts.Full, Timeout: r.opts.FullTimeout, Max: fullBisectMax,
		Progress: progressWriter{r: r, byTsk: byTask},
		Run: func(dir, cmd string, timeout time.Duration) integrate.VerifyResult {
			if f := r.ensureFailure(dir); f != nil {
				lastEnsure = true
				return ensureVerify(f)
			}
			lastEnsure = false
			return integrate.RunVerify(dir, cmd, timeout)
		},
	})
	r.restoreIntegration(branch)
	if err != nil {
		r.stop("full verification red")
		return nil
	}
	if res.Verify.Baseline == "fail" {
		if lastEnsure {
			r.environmentFailure(green, res.Verify.Excerpt)
		} else {
			r.stop("full verification red at the last green SHA")
		}
		return nil
	}
	var reset []string
	for _, rv := range res.Reverted {
		if pid, ok := byTask[rv.Task]; ok && r.postFullRevert(pid, rv.Excerpt) {
			reset = append(reset, pid)
		}
	}
	if res.Verify.Status == "pass" {
		r.fullGreen()
	} else {
		r.stop("full verification red")
	}
	return reset
}

func (r *runner) environmentFailure(sha, excerpt string) {
	detail := fmt.Sprintf("full verification red at the last green SHA %s — environment, "+
		"not code\nensure results: ensure\n%s", sha, excerpt)
	r.need("", "environment", detail)
	r.emit("environment", "", map[string]any{"sha": sha, "excerpt": excerpt})
	r.stop("environment: full verification red at the last green SHA")
}
