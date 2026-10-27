package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/approvals"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/checks"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/trace"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/state"
)

const fastNote = "fast check failed: "

var passingCheck = map[string]bool{"pass": true, "no-checks": true, "preexisting": true}

// runPackage is one worker job: phase implement (dispatch, then verify),
// verify (a tree that is already there), or await (a host session's
// result). A failure inside never takes the loop down.
func (r *runner) runPackage(pid, phase, refreshNote string) {
	defer func() {
		if p := recover(); p != nil {
			kind, msg := "Error", fmt.Sprint(p)
			if re, ok := p.(runnerError); ok {
				kind, msg = re.Kind, re.Msg
			}
			r.need(pid, "dispatch-error", fmt.Sprintf("runner error: %s: %s", kind, msg))
		}
	}()
	switch phase {
	case "await":
		if !r.handle(pid, "", r.hostAwait(pid, r.hostSession(pid))) {
			return
		}
	case "implement":
		r.mu.Lock()
		r.notes[pid] = refreshNote
		r.mu.Unlock()
		r.noteWidening(pid, r.applyOverrides()[pid])
		if _, err := r.writeContext(pid, refreshNote); err != nil {
			panic(runnerError{"OSError", err.Error()})
		}
		_ = r.store.SetStatus(pid, "implementing", "", nil)
		postFull := r.postFullCorrection(pid)
		files := r.fullFiles(pid)
		info, _ := r.getRefresh(pid)
		token, result := r.dispatch(pid, files, postFull || info.fresh, "implement", 1)
		if postFull {
			r.postFullSent(pid)
		}
		r.approvalNotesSent(pid, files)
		if !r.handle(pid, token, result) {
			return
		}
	}
	r.verify(pid)
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// handle records a dispatch result; true when the package moved to
// verifying.
func (r *runner) handle(pid, token string, result map[string]any) bool {
	status := str(result["status"])
	denials := r.networkDenials(pid, result)
	switch status {
	case "_orca_need":
		if r.loginNeed(pid, result) {
			return false
		}
		need := map[string]any{"orca": result["kind"], "detail": result["detail"]}
		tok := result["token"]
		if !truthy(tok) {
			tok = nullable(token)
		}
		need["token"] = tok
		if len(denials) > 0 {
			need["network_denials"] = denials
		}
		r.need(pid, "delegate", marshal(need))
		return false
	case "_stop":
		reason := str(result["reason"])
		_ = r.store.SetStatus(pid, "pending", reason, nil)
		if r.opts.ResumeOnReset && strings.HasPrefix(reason, "executor unavailable") {
			r.mu.Lock()
			r.unavailable = true
			r.mu.Unlock()
		} else {
			r.stop(reason)
		}
		return false
	}
	isolation := asMap(result["isolation"])
	tree := str(isolation["workdir"])
	if tree == "" {
		tree = str(isolation["path"])
	}
	executor := strPtr(str(result["executor"]))
	switch status {
	case "ok":
		answer := str(result["answer"])
		r.mu.Lock()
		r.answers[pid] = answer
		r.mu.Unlock()
		if strings.HasPrefix(firstLine(answer), "ESCALATE") {
			if hosts := approvals.NetworkEscalateHosts(answer); hosts != nil {
				r.networkNeed(pid, answer, hosts, tree, executor)
			} else {
				r.escalateNeed(pid, answer)
			}
			return false
		}
		if !truthy(isolation["isolate"]) || tree == "" {
			why := str(isolation["reason"])
			if why == "" {
				why = "no isolation info"
			}
			r.need(pid, "dispatch-error", "dispatch did not run isolated: "+why)
			return false
		}
		prev := r.store.Snapshot().Packages[pid]
		// A session past resume_max_tokens is not resumed: the next
		// correction goes to a fresh one with the full prompt.
		resumable := result["resumable"] != false
		session := resumable && (truthy(result["session_id"]) || (prev != nil && truthy(prev.Session)))
		_ = r.store.SetStatus(pid, "verifying", "", func(p *state.Package) {
			p.Executor, p.Session, p.Tree = executor, session, &tree
		})
		return true
	case "delegate":
		_ = r.store.UpdatePackage(pid, func(p *state.Package) { p.Tree, p.Executor = strPtr(tree), executor })
		r.need(pid, "delegate", marshal(map[string]any{
			"token":      nullable(token),
			"agent_type": result["agent_type"],
			"model":      result["model"],
			"effort":     result["effort"],
			"tree":       nullable(tree),
		}))
		return false
	case "needs-permission", "runaway":
		// Neither is a cooldown/fallback signal: the user grants a path, or
		// the cause of the runaway is fixed, and the package is resumed.
		_ = r.store.UpdatePackage(pid, func(p *state.Package) { p.Tree, p.Executor = strPtr(tree), executor })
		if status == "needs-permission" {
			list := []any{}
			for _, d := range asList(asMap(result["sandbox"])["denials"]) {
				if m := asMap(d); m != nil {
					list = append(list, map[string]any{
						"path":            m["path"],
						"grant_candidate": m["grant_candidate"],
						"why":             m["why"],
					})
				}
			}
			r.need(pid, "permission", marshal(map[string]any{"denials": list}))
			return false
		}
		runaway := asMap(result["runaway"])
		reason := result["reason"]
		if !truthy(reason) {
			reason = runaway["reason"]
		}
		lastTool := runaway["last_tool"]
		if m := asMap(lastTool); m != nil {
			lastTool = m["name"]
		}
		r.need(pid, "runaway", marshal(map[string]any{"reason": reason, "last_tool": lastTool}))
		return false
	}
	detail := valText(result["reason"])
	if !truthy(result["reason"]) {
		detail = marshal(map[string]any{
			"status":    result["status"],
			"exit_code": result["exit_code"],
			"answer":    result["answer"],
		})
	}
	r.need(pid, "dispatch-error", checks.FailureExcerpt(detail))
	return false
}

func marshal(v any) string {
	b, _ := fsutil.MarshalJSON(v)
	return string(b)
}

func sortedJSON(v any) string { return marshal(v) }

// networkNeed: `ESCALATE: network <host>...` makes the hosts the package's
// network denials (so they are `net:` approvals) and parks it on a
// `network` need until each is decided.
func (r *runner) networkNeed(pid, answer string, hosts []map[string]any, tree string, executor *string) {
	if tree != "" {
		_ = r.store.UpdatePackage(pid, func(p *state.Package) { p.Tree, p.Executor = &tree, executor })
	}
	list := make([]any, len(hosts))
	names := make([]any, len(hosts))
	for i, h := range hosts {
		list[i] = map[string]any{"host": h["host"], "port": h["port"]}
		names[i] = h["host"]
	}
	r.networkDenials(pid, map[string]any{"network_denials": list})
	lines := splitLines(strings.TrimSpace(answer))
	if len(lines) > 10 {
		lines = lines[:10]
	}
	text := []rune(strings.Join(lines, "\n"))
	if len(text) > 2000 {
		text = text[:2000]
	}
	r.need(pid, "network", marshal(map[string]any{"hosts": names, "answer": string(text)}))
}

func (r *runner) escalateNeed(pid, answer string) {
	detail := answer
	if carry, err := os.ReadFile(filepath.Join(r.stateDir, "carry", pid+".carry.md")); err == nil && len(carry) > 0 {
		detail += "\n--- carry ---\n" + string(carry)
	}
	owned := r.pkg(pid).FilesOwned
	var found []string
	for _, line := range splitLines(detail) {
		for _, tok := range strings.Fields(strings.ReplaceAll(line, ",", " ")) {
			p := strings.TrimRight(strings.Trim(tok, "`'\"()[]{}<>:;,!?"), ".")
			if p == "" || filepath.IsAbs(p) || strings.HasPrefix(p, "-") || !pathLike(p) {
				continue
			}
			inside := false
			for _, glob := range owned {
				inside = inside || checks.GlobMatch(glob, p)
			}
			if !inside && !containsStr(found, p) {
				found = append(found, p)
			}
		}
	}
	if len(found) > 0 {
		r.need(pid, "scope", detail+"\n--- outside files_owned ---\n"+strings.Join(found, "\n"))
		return
	}
	r.need(pid, "escalate", detail)
}

func (r *runner) emptyAfterRefresh(pid, answer string) bool {
	info, _ := r.getRefresh(pid)
	if !info.fresh || !info.carried {
		return false
	}
	if files, ok := r.treeFiles(pid); !ok || len(files) != 0 {
		return false
	}
	patch := info.patchFile
	if patch == "" {
		patch = "(patch file unavailable)"
	}
	lines := splitLines(answer)
	if len(lines) > 10 {
		lines = lines[:10]
	}
	r.need(pid, "empty-after-refresh", strings.Join(lines, "\n")+"\n"+patch)
	return true
}

func (r *runner) answer(pid string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.answers[pid]
}

// --- self-verify ---------------------------------------------------------------------

// checkOutcome is a self-verify result: the `agent-exec check --json`
// object (status, files, checks[], flaky?).
type checkOutcome map[string]any

func (c checkOutcome) status() string { return str(c["status"]) }

func (c checkOutcome) flaky() []any { return asList(c["flaky"]) }

func (c checkOutcome) items() []map[string]any {
	var out []map[string]any
	for _, it := range asList(c["checks"]) {
		if m := asMap(it); m != nil {
			out = append(out, m)
		}
	}
	return out
}

func (c checkOutcome) files() *int {
	switch n := c["files"].(type) {
	case int:
		return &n
	case int64:
		v := int(n)
		return &v
	}
	return nil
}

func roundSeconds(d time.Duration) float64 { return math.Round(d.Seconds()*1000) / 1000 }

// check is gated self-verify: `agent-exec check --task pkg-<id>
// --baseline` over the package tree.
func (r *runner) check(pid string) checkOutcome {
	args := []string{"--task", r.task(pid), "--baseline", "--json", "--repo", r.root}
	if session, ok := paths.SessionID(); ok && session != "" {
		args = append(args, "--session", session)
	}
	tree := ""
	if p := r.store.Snapshot().Packages[pid]; p != nil && p.Tree != nil {
		tree = *p.Tree
	}
	r.emit("check-start", pid, map[string]any{"name": "self-verify", "tree": tree})
	started := time.Now()
	stdout, _ := r.runCheck(args)
	parsed := checkOutcome{"status": "error", "checks": []any{}}
	if lines := splitLines(strings.TrimSpace(stdout)); len(lines) > 0 {
		var m map[string]any
		if json.Unmarshal([]byte(lines[len(lines)-1]), &m) == nil && m != nil {
			parsed = checkOutcome(normalizeNumbers(m).(map[string]any))
		}
	}
	r.emit("check-end", pid, map[string]any{"name": "self-verify", "status": parsed["status"],
		"seconds": roundSeconds(time.Since(started))})
	return parsed
}

func (r *runner) runCheck(args []string) (string, int) {
	if r.deps.Check != nil {
		return r.deps.Check(args)
	}
	argv := append(append(append([]string{}, r.deps.AgentExec...), "check"), args...)
	cmd := exec.Command(argv[0], argv[1:]...)
	var out bytes.Buffer
	cmd.Stdout = &out
	end := trace.Begin(trace.KindExec, "wave verify", trace.Attrs{})
	err := cmd.Run()
	end(exitCode(err))
	var ee *exec.ExitError
	switch {
	case err == nil:
		return out.String(), 0
	case errors.As(err, &ee):
		return out.String(), ee.ExitCode()
	}
	return "", -1
}

func failureText(c checkOutcome) string {
	var parts []string
	for _, it := range c.items() {
		if it["status"] == "fail" {
			parts = append(parts, fmt.Sprintf("## %s\n%s", valText(it["name"]), str(it["excerpt"])))
		}
	}
	if len(parts) == 0 {
		return "check status " + valText(c["status"])
	}
	return strings.Join(parts, "\n\n")
}

// fastCheck is verify_mode final: only the `fast: true` check items, over
// the package tree's changed files.
func (r *runner) fastCheck(pid string) checkOutcome {
	cc := r.checksConfig()
	items := cc.itemsWhere(true)
	tree, _, _ := isolate.ResolveWorktree(r.root, r.task(pid), "")
	files, ok := r.treeFiles(pid)
	if tree == "" || !ok {
		return checkOutcome{"status": "error", "checks": []any{}}
	}
	if len(items) == 0 {
		return checkOutcome{"status": "no-checks", "files": len(files), "checks": []any{}}
	}
	r.emit("check-start", pid, map[string]any{"name": "fast", "tree": tree})
	started := time.Now()
	results := checks.Run(tree, items, checks.Options{
		Files: files, SlotDir: cc.slotDir, MaxParallel: cc.maxParallel, RunAll: true,
		Ensure: cc.ensure, RetryFailedAlone: cc.retryFailedAlone,
	}).Checks
	status := foldStatus(results)
	var flaky []any
	list := make([]any, len(results))
	for i, res := range results {
		for _, f := range res.Flaky {
			flaky = append(flaky, f)
		}
		list[i] = map[string]any{"name": res.Name, "status": res.Status, "excerpt": res.Excerpt}
	}
	r.emit("check-end", pid, map[string]any{"name": "fast", "status": status,
		"seconds": roundSeconds(time.Since(started))})
	out := checkOutcome{"status": status, "files": len(files), "checks": list}
	if len(flaky) > 0 {
		out["flaky"] = flaky
	}
	return out
}

// foldStatus: fail if any failed, pass if any passed, else no-checks.
func foldStatus(results []checks.CheckResult) string {
	seen := map[string]bool{}
	for _, res := range results {
		seen[res.Status] = true
	}
	switch {
	case seen["fail"]:
		return "fail"
	case seen["pass"]:
		return "pass"
	}
	return "no-checks"
}

// verifyFast: a red fast check is recorded on the package (its detail,
// carried into `integrated`) and never blocks it.
func (r *runner) verifyFast(pid string) {
	c := r.fastCheck(pid)
	if fl := c.flaky(); len(fl) > 0 {
		r.emit("flaky", pid, map[string]any{"check": "fast", "files": fl})
	}
	status := c.status()
	if status == "error" {
		r.need(pid, "self-verify", "check could not run (status error)")
		return
	}
	if r.emptyAfterRefresh(pid, r.answer(pid)) {
		return
	}
	detail := ""
	if status == "fail" {
		var failed []any
		var names []string
		for _, it := range c.items() {
			if it["status"] == "fail" {
				failed = append(failed, it["name"])
				names = append(names, valText(it["name"]))
			}
		}
		detail = fastNote + strings.Join(names, ", ")
		r.emit("fast-check", pid, map[string]any{"status": "fail", "checks": orEmpty(failed)})
	}
	files := c.files()
	_ = r.store.SetStatus(pid, "ready", detail, func(p *state.Package) { p.FilesChanged = files })
}

func (r *runner) verify(pid string) {
	if r.verifyMode == "final" {
		r.verifyFast(pid)
		return
	}
	for round := 0; round < 2; round++ {
		c := r.check(pid)
		if fl := c.flaky(); len(fl) > 0 {
			r.emit("flaky", pid, map[string]any{"check": "self-verify", "files": fl})
		}
		status := c.status()
		if passingCheck[status] {
			if r.emptyAfterRefresh(pid, r.answer(pid)) {
				return
			}
			files := c.files()
			_ = r.store.SetStatus(pid, "ready", "", func(p *state.Package) { p.FilesChanged = files })
			return
		}
		if status != "fail" {
			r.need(pid, "self-verify", "check could not run (status "+valText(c["status"])+")")
			return
		}
		failures := failureText(c)
		if round == 1 {
			if strings.HasPrefix(firstLine(failures), "ESCALATE") {
				r.escalateNeed(pid, failures)
				return
			}
			r.need(pid, "self-verify", failures)
			return
		}
		correction := r.correctionPath(pid)
		text := fmt.Sprintf("# Correction for %s\n\nThese checks failed on your changes. Fix "+
			"exactly this, stay inside files_owned, do not commit.\n\n%s\n", pid, failures)
		if err := os.WriteFile(correction, []byte(text), 0o644); err != nil {
			panic(runnerError{"OSError", err.Error()})
		}
		prev := r.store.Snapshot().Packages[pid]
		attempts := 0
		hasSession := false
		if prev != nil {
			attempts, hasSession = prev.Attempts, truthy(prev.Session)
		}
		_ = r.store.SetStatus(pid, "fixing", "self-verify failed", func(p *state.Package) { p.Attempts = attempts + 1 })
		var token string
		var result map[string]any
		if hasSession {
			token, result = r.dispatch(pid, []string{correction}, false, "correction", attempts+2)
		} else {
			token, result = r.dispatch(pid, append(r.fullFiles(pid), correction), true, "correction", attempts+2)
		}
		if !r.handle(pid, token, result) {
			return
		}
	}
}
