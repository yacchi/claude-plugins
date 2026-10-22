package runner

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/approvals"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/trace"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/state"
)

// --- refresh / prompt files ---------------------------------------------------------

// refresh moves an existing task tree onto the integration HEAD before a
// dispatch (step 3a). It returns false when the package became a need, and
// the note for its context file.
func (r *runner) refresh(pid, head string) (bool, string) {
	path, _, ok := isolate.ResolveWorktree(r.root, r.task(pid), "")
	if !ok {
		r.setRefresh(pid, refreshState{fresh: true})
		return true, ""
	}
	baseline, _ := isolate.ReadBaseline(path)
	if baseline == "" || baseline == head {
		r.setRefresh(pid, refreshState{})
		return true, ""
	}
	if _, err := gitRun(r.intPath, "merge-base", "--is-ancestor", head, baseline); err == nil {
		return true, "" // the tree already sits on top of the integration HEAD
	}
	res, err := isolate.Refresh(r.root, r.task(pid), head, "")
	if err != nil {
		r.need(pid, "dispatch-error", err.Error())
		return false, ""
	}
	r.emit("refresh", pid, map[string]any{"status": res.Status, "files": len(res.Files)})
	switch res.Status {
	case "conflicted":
		var files []any
		for _, c := range res.Conflicts {
			if c.File != "" {
				files = append(files, c.File)
			}
		}
		r.need(pid, "conflict", sortedJSON(map[string]any{"files": orEmpty(files), "stage": "refresh"}))
		return false, ""
	case "error":
		note := res.Note
		if note == "" {
			note = "refresh failed"
		}
		r.need(pid, "dispatch-error", note)
		return false, ""
	case "ok", "conflicted-resolved":
	default:
		return true, ""
	}
	var changed []string
	if out, err := gitRun(r.intPath, "diff", "--name-only", res.OldBaseline+".."+head); err == nil {
		changed = nonBlank(splitLines(out))
	}
	r.setRefresh(pid, refreshState{
		fresh: true, carried: res.PatchFile != "" && len(res.Files) > 0, patchFile: res.PatchFile,
	})
	list := "- (none)"
	if len(changed) > 0 {
		list = "- " + strings.Join(changed, "\n- ")
	}
	return true, fmt.Sprintf("Your worktree was moved onto the current integration HEAD (%s); your "+
		"earlier changes were re-applied on top. Files other packages changed "+
		"since your previous base:\n%s\n", head[:min(12, len(head))], list)
}

func (r *runner) setRefresh(pid string, s refreshState) {
	r.mu.Lock()
	r.refreshInfo[pid] = s
	r.mu.Unlock()
}

func (r *runner) getRefresh(pid string) (refreshState, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.refreshInfo[pid]
	return s, ok
}

func (r *runner) contextPath(pid string) string {
	return filepath.Join(r.stateDir, "context", pid+".md")
}

func (r *runner) correctionPath(pid string) string {
	return filepath.Join(r.stateDir, "corrections", pid+".md")
}

func (r *runner) writeContext(pid, refreshNote string) (string, error) {
	pkg := r.pkg(pid)
	tree := "the isolated worktree you are started in (your cwd)"
	if p := r.store.Snapshot().Packages[pid]; p != nil && p.Tree != nil && *p.Tree != "" {
		tree = *p.Tree
	}
	lines := []string{
		"# Wave package " + pid,
		"",
		"WORKING TREE: " + tree + ". Do every read, edit, test and command inside it; " +
			"never touch files outside it.",
		"",
		"You own only these paths (files_owned); change nothing else:",
	}
	owned := pkg.FilesOwned
	if len(owned) == 0 {
		owned = []string{"(none listed)"}
	}
	for _, f := range owned {
		lines = append(lines, "- "+f)
	}
	lines = append(lines,
		"",
		"Carry-over: anything you need changed outside files_owned goes, one "+
			"self-contained instruction per line with file paths, into "+
			filepath.Join(r.stateDir, "carry", pid+".carry.md")+" -- never "+
			"into the code. Finish your own scope anyway.",
		"",
		"Read the current code before trusting the spec's paths: other packages "+
			"have moved code since it was written.",
		"",
		"Do NOT commit, stash, reset or checkout. To set changes aside use "+
			"`agent-exec shelf push` / `agent-exec shelf pop`, never `git stash`.",
		"",
		"If the task cannot be done as specified, reply with a first line "+
			"`ESCALATE: <reason>` and change nothing further.",
		"",
		"Network access goes through an allowlist. A blocked request (proxy 403, "+
			"\"blocked by network allowlist\", connection refused to an external host) "+
			"must not be worked around: no other mirrors, no proxy or environment "+
			"changes, no hand-vendoring. If the task cannot be finished without it, "+
			"reply with a first line `ESCALATE: network <host>[, <host>...]`.",
		"",
		"Before you report done, run a real check that exercises your change (the spec's "+
			"verification command, else the project's tests, type-checker or build); a syntax-only "+
			"check or one that failed to start does not count. If none can run here, say in your "+
			"result which one you did not run and why.",
		"",
		"Do not commit: leave every change uncommitted in the working tree.",
	)
	if refreshNote != "" {
		lines = append(lines, "", refreshNote)
	}
	path := r.contextPath(pid)
	return path, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

// fullFiles is a fresh dispatch's prompt: preamble, context, spec, then a
// pending post-full correction and the approval decision notes.
func (r *runner) fullFiles(pid string) []string {
	files := append(append([]string{}, r.plan.Preamble...), r.contextPath(pid), r.pkg(pid).Spec)
	if r.postFullCorrection(pid) {
		files = append(files, r.correctionPath(pid))
	}
	if notes := approvals.NotePath(r.stateDir, pid); isFile(notes) {
		files = append(files, notes)
	}
	return files
}

// approvalNotesSent sets sent decision notes aside so they are never resent.
func (r *runner) approvalNotesSent(pid string, files []string) {
	notes := approvals.NotePath(r.stateDir, pid)
	if containsStr(files, notes) {
		_ = os.Rename(notes, strings.TrimSuffix(notes, ".md")+".sent.md")
	}
}

const (
	postFullHeader = "# Post-integration correction for %s"
	postFullDetail = "post-full revert"
)

// postFullCorrection: corrections/<id>.md is a not-yet-sent post-full one.
func (r *runner) postFullCorrection(pid string) bool {
	data, err := os.ReadFile(r.correctionPath(pid))
	if err != nil {
		return false
	}
	first, _, _ := strings.Cut(string(data), "\n")
	return first == fmt.Sprintf(postFullHeader, pid)
}

// postFullSent sets a sent post-full correction aside (a later self-verify
// correction can reuse the path).
func (r *runner) postFullSent(pid string) {
	if r.postFullCorrection(pid) {
		_ = os.Rename(r.correctionPath(pid), filepath.Join(r.stateDir, "corrections", pid+".post-full.sent.md"))
	}
}

// --- dispatch ---------------------------------------------------------------------

// dispatch is prepare + dispatch, bracketed by dispatch-start/-end events.
func (r *runner) dispatch(pid string, promptFiles []string, noResume bool, kind string, attempt int) (string, map[string]any) {
	r.emit("dispatch-start", pid, map[string]any{"attempt": attempt, "cls": r.pkg(pid).Cls, "kind": kind})
	started := time.Now()
	end := map[string]any{"status": "error", "executor": nil}
	defer func() {
		end["seconds"] = math.Round(time.Since(started).Seconds()*1000) / 1000
		r.emit("dispatch-end", pid, end)
	}()
	var token string
	var result map[string]any
	if s := r.hostSession(pid); s != nil {
		// A live session already holds the contract and its code.
		result = r.hostPrompt(pid, s, promptFiles, "")
	} else {
		token, result = r.dispatchOnce(pid, promptFiles, noResume)
		if result["status"] == "delegate" && r.host != nil {
			result = r.hostStart(pid, token, result, promptFiles)
		}
	}
	end["status"], end["executor"] = result["status"], result["executor"]
	return token, result
}

// dispatchOnce is prepare + dispatch with the one unavailable retry; status
// "_stop" means the executor pool is exhausted (reason in "reason").
func (r *runner) dispatchOnce(pid string, promptFiles []string, noResume bool) (string, map[string]any) {
	pkg := r.pkg(pid)
	trace.Lock(&r.intLock, "wave integration", trace.Attrs{Pkg: pid})
	r.cleanIntegration("pre-dispatch")
	token, err := r.exec.Prepare(promptFiles, pkg.Cls, r.intPath, r.task(pid), r.opts.RunID)
	r.intLock.Unlock()
	if err != nil {
		panic(runnerError{"RuntimeError", err.Error()})
	}
	var result map[string]any
	for attempt := 0; attempt < 2; attempt++ {
		result = normalizeNumbers(r.exec.Dispatch(token, pkg.Cls, r.exhaustedList(), noResume)).(map[string]any)
		status := result["status"]
		var exhausted []string
		if status == "unavailable" {
			r.mu.Lock()
			if ex := str(result["executor"]); ex != "" {
				r.exhausted[ex] = true
			}
			r.mu.Unlock()
			exhausted = r.exhaustedList()
			if attempt == 0 {
				continue
			}
		}
		if status == "unavailable" || status == "unroutable" {
			if exhausted == nil {
				exhausted = r.exhaustedList()
			}
			which := strings.Join(exhausted, ", ")
			if which == "" {
				which = "no route"
			}
			return token, map[string]any{"status": "_stop", "reason": "executor unavailable: " + which}
		}
		return token, result
	}
	return token, result
}

func (r *runner) exhaustedList() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.exhausted))
	for k := range r.exhausted {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// runnerError is a failure inside a package job, reported as a
// dispatch-error need ("runner error: <Kind>: <Msg>").
type runnerError struct{ Kind, Msg string }

// --- interactive host ---------------------------------------------------------------

func (r *runner) hostSession(pid string) *host.Session {
	if r.host == nil {
		return nil
	}
	s, ok := r.host.Session(pid, r.stateDir)
	if !ok {
		return nil
	}
	return s
}

func (r *runner) hostResultPath(pid string) string {
	return filepath.Join(r.stateDir, "orca", pid+".result.json")
}

var terminalLine = regexp.MustCompile(`\nterminal: ([^\n]+)`)

func (r *runner) hostNeed(token string, need *host.NeedError, pid string) map[string]any {
	detail := need.Detail
	if need.Kind == host.KindStalled && strings.Contains(detail, "dialog:") {
		handle := "unknown"
		if m := terminalLine.FindStringSubmatch(detail); m != nil {
			handle = m[1]
		}
		if pid == "" {
			pid = "ID"
		}
		detail += fmt.Sprintf("\napprove it in %s (terminal %s), then run: agent-exec wave mark "+
			"--state %s --await %s"+
			"\nor add the directory to %s.add_dirs / set %s.permission_mode: "+
			"auto and re-dispatch with --status pending",
			capitalize(r.hostName), handle, r.statePath, pid, r.hostName, r.hostName)
	}
	out := map[string]any{"status": "_orca_need", "kind": need.Kind, "detail": detail,
		"token": nullable(token), "executor": r.hostName}
	if d := need.NetworkDenials(); len(d) > 0 {
		out["network_denials"] = d
	}
	return out
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}

// hostStart runs a `delegate` package in a new host session instead of
// handing it to the instructor; the result is dispatch-shaped.
func (r *runner) hostStart(pid, token string, delegated map[string]any, promptFiles []string) map[string]any {
	// The CLI dispatch already made a tree for the task; drop it (it holds
	// nothing yet) so the task id resolves to the host's tree.
	isolation := asMap(delegated["isolation"])
	if stray := str(isolation["path"]); truthy(isolation["isolate"]) && stray != "" {
		if _, adopted := isolate.ReadAdopted(stray); !adopted {
			res, err := isolate.Remove(r.root, r.task(pid), false, "")
			if err != nil || (res.Status != "removed" && res.Status != "absent") {
				return r.hostNeed(token, host.Need(host.KindError, fmt.Sprintf(
					"could not drop the dispatch worktree %s (%s)", stray, orUnknown(res.Status))), "")
			}
		}
	}
	cls := r.pkg(pid).Cls
	model := str(delegated["model"])
	if model == "" {
		model = r.host.ModelFor(cls)
	}
	s, err := r.host.Start(r.ctx, pid, r.root, r.intBranch, cls, model, r.opts.RunID, r.stateDir, r.hostAddDirs())
	if err != nil {
		return r.hostNeed(token, host.AsNeed(err), pid)
	}
	tree, name := s.Worktree, r.hostName
	_ = r.store.UpdatePackage(pid, func(p *state.Package) {
		p.Tree, p.Executor, p.Session = &tree, &name, true
	})
	if containsStr(promptFiles, r.contextPath(pid)) {
		r.mu.Lock()
		note := r.notes[pid]
		r.mu.Unlock()
		_, _ = r.writeContext(pid, note) // WORKING TREE = the host's tree
	}
	return r.hostPrompt(pid, s, promptFiles, token)
}

// hostAddDirs is the directories outside the worktree a hosted session
// must reach: the state dir, the repo, every directory holding a preamble
// or spec file, and the host's add_dirs.
func (r *runner) hostAddDirs() []string {
	dirs := []string{r.stateDir, r.root}
	for _, p := range r.plan.Preamble {
		dirs = append(dirs, filepath.Dir(absPath(p)))
	}
	for _, pkg := range r.plan.Packages {
		dirs = append(dirs, filepath.Dir(absPath(pkg.Spec)))
	}
	if rt, ok := r.host.(host.Runtime); ok {
		for _, p := range rt.Settings().AddDirs {
			expanded := expandUser(expandVars(p))
			if !filepath.IsAbs(expanded) {
				expanded = filepath.Join(r.root, expanded)
			}
			dirs = append(dirs, absPath(expanded))
		}
	}
	return host.SortedDirs(dirs)
}

func (r *runner) taskTimeout() time.Duration {
	if rt, ok := r.host.(host.Runtime); ok {
		return rt.Settings().TaskTimeout
	}
	return time.Hour
}

func (r *runner) hostPrompt(pid string, s *host.Session, promptFiles []string, token string) map[string]any {
	out, err := host.PromptAndWait(r.ctx, r.host, s, promptFiles, r.hostResultPath(pid), r.taskTimeout())
	if err != nil {
		return r.hostNeed(token, host.AsNeed(err), pid)
	}
	return out
}

func (r *runner) hostAwait(pid string, s *host.Session) map[string]any {
	if s == nil {
		return r.hostNeed("", host.Need(host.KindStalled, "agent is no longer registered"), pid)
	}
	data, err := r.host.WaitResult(r.ctx, s, r.hostResultPath(pid), r.taskTimeout())
	if err != nil {
		return r.hostNeed("", host.AsNeed(err), pid)
	}
	return host.ResultOutput(r.hostName, s, s.Terminal, true, data)
}

func (r *runner) hostClose(pid string) {
	if s := r.hostSession(pid); s != nil {
		_ = r.host.Close(s, true)
	}
}

// --- results -----------------------------------------------------------------------

// networkDenials keeps the hosts a package's session was blocked on (never
// a failure by itself) so needs and status show them.
func (r *runner) networkDenials(pid string, result map[string]any) []any {
	denials := asList(result["network_denials"])
	if len(denials) == 0 {
		return nil
	}
	_ = r.store.UpdatePackage(pid, func(p *state.Package) {
		merged := append([]any{}, p.NetworkDenials...)
		for _, d := range denials {
			if !containsJSON(merged, d) {
				merged = append(merged, d)
			}
		}
		p.NetworkDenials = merged
	})
	r.emit("network-denied", pid, map[string]any{"denials": denials})
	return denials
}

// containsJSON compares decoded JSON values by their encoding.
func containsJSON(list []any, v any) bool {
	want, _ := fsutil.MarshalJSON(v)
	for _, e := range list {
		if got, _ := fsutil.MarshalJSON(e); string(got) == string(want) {
			return true
		}
	}
	return false
}

func expandUser(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + p[1:]
		}
	}
	return p
}

// expandVars is os.path.expandvars: $NAME and ${NAME} of set variables are
// replaced, anything else is left as written.
func expandVars(s string) string {
	return varRef.ReplaceAllStringFunc(s, func(m string) string {
		name := strings.Trim(m[1:], "{}")
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		return m
	})
}

var varRef = regexp.MustCompile(`\$(\w+|\{[^}]*\})`)
