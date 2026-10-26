package herdr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/hostref"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/sandbox"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/srt"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/transcript"
)

// maxStartLine keeps the start command under macOS MAX_CANON (1024): a pane
// whose shell line editor is not up yet reads in canonical mode and drops
// input past it, newline included, so claude never starts. The margin is
// for herdr's own quoting.
const maxStartLine = 1000

// --- registry -------------------------------------------------------------------

func (e *Executor) packageSessions(stateDir string) map[string]any {
	data := e.reg.Load(stateDir)
	delete(data, WarmKey)
	return data
}

func (e *Executor) sessionStateDir(s *host.Session) string {
	if sd := s.Str("state_dir"); sd != "" {
		return sd
	}
	return e.StateDir()
}

func (e *Executor) save(s *host.Session) {
	if s.Truthy("ephemeral") {
		return // the warm reader lives under WarmKey, not as a package
	}
	if err := e.reg.Save(e.sessionStateDir(s), s); err != nil {
		e.warnf("registry: %s", err)
	}
}

func (e *Executor) forget(s *host.Session) {
	if err := e.reg.Forget(e.sessionStateDir(s), s.Pkg); err != nil {
		e.warnf("registry: %s", err)
	}
}

// Session is the registered session of pkg. An agent herdr no longer knows
// is dropped from the session (the next prompt opens a new one on the same
// worktree); a worktree that is gone drops the whole entry.
func (e *Executor) Session(pkg, stateDir string) (*host.Session, bool) {
	stateDir = e.adoptStateDir(stateDir)
	if pkg == WarmKey || stateDir == "" {
		return nil, false
	}
	rec, ok := e.reg.Load(stateDir)[pkg].(map[string]any)
	if !ok {
		return nil, false
	}
	s := host.SessionFromRecord(e.Name(), rec)
	if s.Worktree == "" {
		return nil, false
	}
	if st, err := os.Stat(s.Worktree); err != nil || !st.IsDir() {
		s.Pkg = pkg
		s.Set("state_dir", stateDir)
		// A tree removed behind the host's back leaves its workspace and the
		// Claude in it running, and the agent name taken for the next start.
		if ws := s.Str("workspace"); ws != "" {
			_, _, _ = e.call(context.Background(), callTimeout, s.Str("herdr_session"), "workspace", "close", ws)
		}
		e.forget(s)
		return nil, false
	}
	if s.Terminal != "" {
		if _, err := e.status(context.Background(), s.Terminal, s.Str("herdr_session")); err != nil {
			s.Terminal = ""
			s.Set("workspace", nil)
			e.save(s)
		}
	}
	return s, true
}

// --- agent ------------------------------------------------------------------------

func (e *Executor) status(ctx context.Context, name, sname string) (string, error) {
	payload, _, err := e.call(ctx, callTimeout, sname, "agent", "get", name)
	if err != nil {
		return "", err
	}
	if s, ok := Find(payload, "agent_status").(string); ok {
		return s, nil
	}
	return "unknown", nil
}

// screen is the visible screen of an agent as text.
func (e *Executor) screen(ctx context.Context, name string) (string, error) {
	payload, out, err := e.call(ctx, callTimeout, "", "agent", "read", name, "--source", "visible")
	if err != nil {
		return "", err
	}
	if len(payload) > 0 {
		return strings.Join(Strings(payload), "\n"), nil
	}
	return out, nil
}

// bypassMissing is why a permission bypass is refused now, "" when the
// server runs under srt with the network allowlist enforced and the host
// filesystem sandbox applies.
func (e *Executor) bypassMissing() string {
	if !e.serverReady {
		return "the herdr server is not running yet"
	}
	if !e.sandboxed {
		return "the host filesystem sandbox does not apply (" + e.unsandboxed + ")"
	}
	if !e.netRestricted {
		return "the herdr server is not running under srt with the network allowlist enforced " +
			"(network not restricted: " + e.netReason + ")"
	}
	return ""
}

// nativeArgs are the claude arguments after `agent start ... --`.
func (e *Executor) nativeArgs(s *host.Session, fork bool) ([]string, error) {
	mode := e.cfg.Str("permission_mode")
	if e.cfg.Get("permission_mode") == nil {
		mode = "acceptEdits"
	}
	template := e.cfg.Str("args")
	if !permissionModes[mode] && mode != bypassMode {
		return nil, host.Need(host.KindError, "invalid herdr.permission_mode: "+mode)
	}
	if mode == bypassMode || strings.Contains(template, bypassMode) ||
		strings.Contains(template, "--dangerously-skip-permissions") {
		if missing := e.bypassMissing(); missing != "" {
			return nil, host.Need(host.KindError, "herdr permission bypass is not allowed: "+missing)
		}
	}
	model := s.Str("model")
	if model == "" {
		model = "opus"
	}
	text := strings.ReplaceAll(template, "{model}", model)
	text = strings.ReplaceAll(text, "{permission_mode}", mode)
	args, err := host.ShellSplit(text)
	if err != nil {
		return nil, host.Need(host.KindError, "invalid herdr.args: "+err.Error())
	}
	// The prompt files and the result file live outside the worktree;
	// without --add-dir the session stops on a permission dialog.
	for _, d := range s.Strings("add_dirs") {
		args = append(args, "--add-dir", d)
	}
	if sid := s.Str("claude_session_id"); sid != "" {
		args = append(args, "--session-id", sid)
	}
	if ws := s.Str("warm_session"); fork && ws != "" {
		args = append(args, "--resume", ws, "--fork-session")
	}
	// Every start (fresh, warm reader, fork, reopen) goes through here, so
	// warm and fork share one flag set: the cache prefix includes the tools.
	flags, err := host.IsolationFlags(e.cfg, args)
	if err != nil {
		return nil, err
	}
	return append(args, flags...), nil
}

// startLine is the command line the pane receives: claude with its
// arguments shell-quoted.
func startLine(native []string) string {
	quoted := make([]string, 0, len(native))
	for _, a := range native {
		quoted = append(quoted, sandbox.ShellQuote(a))
	}
	return "claude " + strings.Join(quoted, " ")
}

// paneScreen is the visible screen of a pane as text. The pane is read, not
// the agent: a failed start may leave the agent unknown to herdr.
func (e *Executor) paneScreen(ctx context.Context, pane string) (string, error) {
	payload, out, err := e.call(ctx, callTimeout, "", "pane", "read", pane, "--source", "visible")
	if err != nil {
		return "", err
	}
	if len(payload) > 0 {
		return strings.Join(Strings(payload), "\n"), nil
	}
	return out, nil
}

// startFailure is the detail of a failed start: err, then the tail of the
// pane's screen when there is one.
func (e *Executor) startFailure(ctx context.Context, pane string, err error) string {
	screen, serr := e.paneScreen(ctx, pane)
	if serr != nil || strings.TrimSpace(screen) == "" {
		return err.Error()
	}
	return err.Error() + "\n" + host.TailText(screen)
}

func callErr(err error) *CallError {
	var ce *CallError
	if errors.As(err, &ce) {
		return ce
	}
	return nil
}

// workspaceEnv is the env of the processes of a pane on tree: the worker's
// isolation switches (host.IsolationEnv) and the config.TreeEntries of tree
// (pnpm's global virtual store, mise's trust). Each workspace gets its own, so
// a setting change applies from the next package start.
func (e *Executor) workspaceEnv(tree string) ([]string, error) {
	env, err := host.IsolationEnv(e.cfg)
	if err != nil {
		return nil, err
	}
	return append(env, config.TreeEntries(config.PnpmGVS(e.tools), tree, os.Environ())...), nil
}

// openAgent opens a workspace and an agent on the session's worktree, up
// to a usable prompt. The trust dialog is a need unless auto_trust applies
// (a worktree this executor created, under a sandboxed server).
func (e *Executor) openAgent(ctx context.Context, s *host.Session, fork bool) (string, error) {
	e.mu.Lock()
	if e.repo == "" {
		e.repo = s.Str("repo")
	}
	e.mu.Unlock()
	if err := e.ensureServer(ctx); err != nil {
		return "", err
	}
	// Every claude process gets its own id (fresh, fork, reopen), so each
	// transcript is found by id; the warm reader keeps the id warm.go preset.
	if !s.Truthy("ephemeral") {
		s.Set("claude_session_id", transcript.NewSessionID())
	}
	native, err := e.nativeArgs(s, fork)
	if err != nil {
		return "", err
	}
	if line := startLine(native); len(line) > maxStartLine {
		return "", host.Need(host.KindError, fmt.Sprintf(
			"the claude start line is %d bytes, over the %d limit: a pane that is not ready reads its "+
				"input in canonical mode, and macOS MAX_CANON (1024) drops input past that, so claude never "+
				"starts; shorten it with fewer or shorter --add-dir paths (keep specs in one directory, "+
				"put the state dir on a shorter path)", len(line), maxStartLine))
	}
	name := AgentName(s.Pkg)
	e.dropStaleAgent(ctx, name)
	env, err := e.workspaceEnv(s.Worktree)
	if err != nil {
		return "", err
	}
	args := []string{"workspace", "create", "--cwd", s.Worktree, "--label", "wave " + s.Pkg, "--no-focus"}
	for _, kv := range env {
		args = append(args, "--env", kv)
	}
	payload, _, err := e.call(ctx, callTimeout, "", args...)
	if err != nil && !e.statusRunning(ctx) {
		// A parallel run's refresh may have stopped the server after
		// ensureServer: start it again and retry the create once.
		if err := e.restartServer(ctx); err != nil {
			return "", err
		}
		payload, _, err = e.call(ctx, callTimeout, "", args...)
	}
	if err != nil {
		return "", host.Need(host.KindError, err.Error())
	}
	workspace, _ := Find(payload, "workspace_id").(string)
	pane, _ := Find(payload, "pane_id").(string)
	if workspace == "" || pane == "" {
		return "", host.Need(host.KindError, "herdr workspace create returned no workspace/pane")
	}
	s.Set("workspace", workspace)
	s.Set("pane", pane)
	s.Terminal = name
	s.Set("herdr_session", e.sname)
	e.save(s)
	e.record(s, workspace, name)

	startup := e.cfg.Seconds("startup_timeout", 90)
	ms := strconv.FormatInt(startup.Milliseconds(), 10)
	timeout := startup + 30*time.Second
	ready := false
	for attempt := 0; attempt <= shellRetries; attempt++ {
		args := append([]string{"agent", "start", name, "--kind", "claude", "--pane", pane,
			"--timeout", ms, "--"}, native...)
		_, _, err := e.call(ctx, timeout, "", args...)
		if err == nil {
			ready = true
			break
		}
		// A pane created a moment ago may not have its shell up yet.
		if strings.Contains(err.Error(), "not an available shell") && attempt < shellRetries {
			if err := e.sleep(ctx, time.Second); err != nil {
				return "", host.AsNeed(err)
			}
			continue
		}
		if ce := callErr(err); ce == nil || ce.Code != "agent_not_ready" {
			return "", host.Need(host.KindError, e.startFailure(ctx, pane, err))
		}
		break
	}
	if ready {
		e.recordHosted(s)
		return name, nil
	}
	screen, err := e.screen(ctx, name)
	if err != nil {
		return "", host.Need(host.KindError, err.Error())
	}
	if !host.IsTrustText(screen) {
		return "", host.Need(host.KindError, "agent not ready:\n"+host.TailText(screen))
	}
	autoTrust := e.cfg.Truthy("auto_trust")
	if !(autoTrust && s.Truthy("created") && e.sandboxed) {
		why := ""
		if autoTrust && s.Truthy("created") {
			why = " (auto_trust not applied: herdr not sandboxed: " + e.unsandboxed + ")"
		}
		home, _ := e.homeFor(e.sname)
		return "", host.Need(host.KindTrust, s.Worktree+" needs the folder-trust dialog accepted once; "+
			"open the herdr session "+e.sname+" (herdr home "+home+") and accept it"+why)
	}
	// The default option is "No, exit": move down first, then Enter.
	_, _, err = e.call(ctx, callTimeout, "", "agent", "send-keys", name, "down", "enter")
	if err == nil {
		// A bare `wait` returns at once while the dialog is still blocked
		// (launch_pending); only idle means it took the keys.
		_, _, err = e.call(ctx, timeout, "", "agent", "wait", name, "--until", "idle", "--timeout", ms)
	}
	if err != nil {
		tail := ""
		if scr, serr := e.screen(ctx, name); serr == nil {
			tail = host.TailText(scr)
		}
		return "", host.Need(host.KindStalled, "folder-trust dialog did not accept: "+err.Error()+"\n"+tail)
	}
	if err := e.awaitInput(ctx, name); err != nil {
		return "", err
	}
	e.emit(s.Pkg, "trust", name, nil)
	e.recordHosted(s)
	return name, nil
}

// recordHosted indexes the started claude session under its run, so the
// run's usage and resume lookups can find its transcript, and saves the
// session with the id.
func (e *Executor) recordHosted(s *host.Session) {
	runID := s.Str("run_id")
	if runID == "" {
		runID = e.runID
	}
	transcript.RecordHosted(runID, s.Str("claude_session_id"))
	e.save(s)
}

// awaitInput waits until the trust dialog is gone and Claude's input box
// is drawn: `agent wait --until idle` can return earlier, and a prompt sent
// then is swallowed while herdr still reports it ok.
func (e *Executor) awaitInput(ctx context.Context, name string) error {
	deadline := e.now().Add(e.cfg.Seconds("startup_timeout", 90))
	for {
		screen, err := e.screen(ctx, name)
		if err != nil {
			screen = ""
		}
		if !host.IsTrustText(screen) && host.HasPromptBox(host.SplitLines(screen)) {
			return host.AsNeedOrNil(e.sleep(ctx, inputSettle))
		}
		if !e.now().Before(deadline) {
			return host.Need(host.KindStalled, "input box never appeared after the folder-trust dialog:\n"+
				host.TailText(screen))
		}
		if err := e.sleep(ctx, e.pollShort()); err != nil {
			return host.AsNeed(err)
		}
	}
}

// dropStaleAgent closes the workspace of an agent already named name in this
// herdr session. openAgent runs only when the package has no live agent of
// its own, so one found here is a leftover (its tree removed while it ran, a
// crashed run) and would make `agent start` fail on the taken name.
func (e *Executor) dropStaleAgent(ctx context.Context, name string) {
	payload, _, err := e.call(ctx, callTimeout, "", "agent", "get", name)
	if err != nil {
		return
	}
	if ws, _ := Find(payload, "workspace_id").(string); ws != "" {
		if _, _, err := e.call(ctx, callTimeout, "", "workspace", "close", ws); err != nil {
			e.warnf("could not close the leftover agent %s: %s", name, err)
		}
	}
}

// record writes the workspace opened on the session's tree into the tree,
// where every removal of it finds and closes it.
func (e *Executor) record(s *host.Session, workspace, agent string) {
	home, err := e.homeFor(e.sname)
	if err == nil {
		err = isolate.WriteHostSession(s.Worktree, hostref.Ref{Host: hostref.Herdr, Binary: e.binary(),
			Session: e.sname, Home: home, Workspace: workspace, Agent: agent})
	}
	if err != nil {
		e.warnf("could not record the workspace in %s: %s", s.Worktree, err)
	}
}

func (e *Executor) closeAgent(s *host.Session) {
	workspace := s.Str("workspace")
	if workspace == "" {
		s.Terminal = ""
		return
	}
	handle := s.Terminal
	if _, _, err := e.call(context.Background(), callTimeout, "", "workspace", "close", workspace); err != nil {
		e.warnf("%s", err)
	} else if err := isolate.ClearHostSession(s.Worktree); err != nil && isDir(s.Worktree) {
		e.warnf("could not clear the hosted session record of %s: %s", s.Worktree, err)
	}
	s.Terminal = ""
	s.Set("workspace", nil)
	s.Set("pane", nil)
	e.save(s)
	e.emit(s.Pkg, "close", handle, nil)
}

// --- the contract ---------------------------------------------------------------------

func revParseCommit(repo, ref string) string {
	out, err := gitx.Run(repo, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// Start creates the orchestra worktree for `pkg-<id>` on baseRef, opens a
// workspace and a Claude agent there and waits for it to be usable. With a
// usable warm reader of the model the agent forks from it.
func (e *Executor) Start(ctx context.Context, pkgID, repo, baseRef, cls, model, runID, stateDir string, addDirs []string) (*host.Session, error) {
	stateDir = e.adoptStateDir(stateDir)
	e.mu.Lock()
	e.runID = runID
	if e.sname == "" {
		e.sname = SessionName(runID, e.Env)
	}
	if e.sname == "" {
		e.sname = "orchestra-" + sha8(absPath(stateDir))
	}
	e.mu.Unlock()
	baseSHA := revParseCommit(repo, baseRef)
	if baseSHA == "" {
		return nil, host.Need(host.KindError, "could not resolve "+baseRef+" to a commit")
	}
	dirs := host.SortedDirs(addDirs)
	warm := e.warmFor(ctx, pkgID, repo, baseRef, model, runID, stateDir, dirs)
	created, err := isolate.Create(repo, "pkg-"+pkgID, isolate.CreateOptions{
		Backend: isolate.BackendGit, Onto: baseSHA, BaseBranch: baseRef, Carry: true,
		CarryExtra: e.isolation("carry_extra"), CarryFiles: e.carryFiles(),
	})
	if err != nil || (created.Status != "created" && created.Status != "exists") || !isDir(created.Path) {
		reason := created.Reason
		if err != nil {
			reason = err.Error()
		}
		return nil, host.Need(host.KindError, "could not create the worktree for "+pkgID+": "+orUnknown(reason))
	}
	s := &host.Session{Pkg: pkgID, Name: e.Name(), Worktree: created.Path, Extra: map[string]any{
		"branch": created.Branch, "workspace": nil, "pane": nil, "repo": repo,
		"model": model, "cls": cls, "state_dir": stateDir, "created": created.Status == "created",
		"herdr_session": e.sname, "add_dirs": dirs, "net_mark": e.netMark(), "run_id": runID,
	}}
	if warm != nil {
		s.Set("warm_session", warm.str("session_id"))
		s.Set("warm_worktree", warm.str("worktree"))
	}
	e.save(s)
	if len(created.Carried) > 0 || len(created.CarriedFiles) > 0 {
		method := created.CarryMethod
		if method == "" {
			method = "none"
		}
		e.emit(pkgID, "carry", "", map[string]any{"method": method, "files": len(created.CarriedFiles)})
	}
	name, err := e.openAgent(ctx, s, warm != nil)
	if err != nil && warm != nil {
		need := host.AsNeed(err)
		if need.Kind == host.KindError {
			// The fork could not start (e.g. the warm transcript is gone):
			// warm never fails a package, so start it fresh instead.
			e.warmMu.Lock()
			e.warmFailed[model] = true
			e.warmMu.Unlock()
			e.emit(pkgID, "warm-failed", "", map[string]any{"model": model, "reason": need.Detail})
			e.closeAgent(s)
			s.Delete("warm_session")
			s.Delete("warm_worktree")
			warm = nil
			name, err = e.openAgent(ctx, s, false)
		}
	}
	if err != nil {
		_ = e.Close(s, true)
		return nil, host.AsNeed(err)
	}
	var extra map[string]any
	if warm != nil {
		extra = map[string]any{"warm": warm.str("session_id")}
	}
	e.emit(pkgID, "start", name, extra)
	s.Set("fresh", true) // the first prompt is not a resume
	if warm != nil {
		s.Set("fork_first", true)
	}
	return s, nil
}

func (e *Executor) netMark() int {
	mark := 0
	e.withRun(func(r *srt.Run) { mark = r.Mark() })
	return mark
}

func (e *Executor) isolation(key string) []string {
	v, ok := e.tools.Lookup("isolation", key)
	if !ok {
		return nil
	}
	return toStrings(v)
}

func (e *Executor) carryFiles() []string {
	v, ok := e.tools.Lookup("isolation", "carry_files")
	if !ok || v == nil {
		return nil
	}
	out := toStrings(v)
	if out == nil {
		out = []string{}
	}
	return out
}

func toStrings(v any) []string {
	list, _ := v.([]any)
	var out []string
	for _, e := range list {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return p != "" && err == nil && st.IsDir()
}
