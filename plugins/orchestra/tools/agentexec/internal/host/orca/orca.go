// Package orca is the Orca host of `wave run`: Claude Code sessions that
// stay alive, hosted by Orca (a desktop app with an `orca` CLI).
//
// Each package gets an Orca-created worktree (`orca worktree create`; Orca
// cannot open a terminal in a tree it did not create), adopted as task
// `pkg-<id>` (isolate.Adopt) so diff/check/refresh/integrate reach it by
// task id. Completion is signalled only by the result file: `terminal wait
// --for tui-idle` can report idle before the TUI started or while a dialog
// is up. Sessions persist in `<state dir>/orca-sessions.json`.
package orca

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/sandbox"
)

// RegistryName is the session registry file in the wave state dir.
const RegistryName = "orca-sessions.json"

const (
	down            = "\x1b[B"
	trustKeyLimit   = 6
	waitChunk       = 60 * time.Second // one `terminal wait` call
	sendWaitSubmit  = 30
	statusTimeout   = 15 * time.Second
	callTimeout     = 60 * time.Second
	worktreeTimeout = 300 * time.Second
)

var permissionModes = map[string]bool{"acceptEdits": true, "auto": true, "default": true, "plan": true}

// CallError is one failed orca call; Payload is its parsed JSON, if any.
type CallError struct {
	Msg     string
	Payload map[string]any
}

func (e *CallError) Error() string { return e.Msg }

// Executor runs packages in Orca-hosted Claude Code sessions. The exported
// fields are test seams; New fills the defaults.
type Executor struct {
	Binary string // the orca binary (default "orca", looked up on PATH)
	Clock  host.Clock
	Poll   time.Duration
	Stderr io.Writer

	cfg      host.Config
	tools    *config.Resolved
	reg      host.Registry
	onEvent  host.EventFunc
	stateMu  sync.Mutex
	stateDir string
}

var (
	_ host.Host    = (*Executor)(nil)
	_ host.Runtime = (*Executor)(nil)
)

// New is the Orca host under cfg (nil: the defaults).
func New(cfg *config.Resolved) *Executor {
	return &Executor{Binary: "orca", Clock: host.RealClock(), Poll: time.Second, Stderr: os.Stderr,
		cfg: host.MergeConfig(cfg, "orca"), tools: cfg, reg: host.Registry{FileName: RegistryName}}
}

// Name is "orca".
func (e *Executor) Name() string { return "orca" }

// Enabled is the normalized `orca.enabled`.
func (e *Executor) Enabled() host.Tri { return e.cfg.Enabled }

// Config is the merged `orca` section.
func (e *Executor) Config() host.Config { return e.cfg }

// Settings is the runner's view of the config.
func (e *Executor) Settings() host.Settings { return e.cfg.Settings() }

// SetEventFunc installs the runner's event hook.
func (e *Executor) SetEventFunc(fn host.EventFunc) { e.onEvent = fn }

// SetStateDir names the wave's state dir.
func (e *Executor) SetStateDir(dir string) {
	e.stateMu.Lock()
	e.stateDir = dir
	e.stateMu.Unlock()
}

func (e *Executor) adoptStateDir(dir string) string {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	if e.stateDir == "" {
		e.stateDir = dir
	}
	if dir == "" {
		return e.stateDir
	}
	return dir
}

// ModelFor is `orca.models[cls]`, else opus.
func (e *Executor) ModelFor(cls string) string {
	if m := e.cfg.Model(cls); m != "" {
		return m
	}
	return "opus"
}

func (e *Executor) emit(pkg, action, terminal string, extra map[string]any) {
	host.Emit(e.onEvent, pkg, action, terminal, extra)
}

func (e *Executor) warnf(format string, args ...any) {
	fmt.Fprintf(e.Stderr, "agent-exec: orca: "+format+"\n", args...)
}

// --- calls ------------------------------------------------------------------------

// call runs one `orca ... --json` and returns its `result` object.
func (e *Executor) call(ctx context.Context, timeout time.Duration, args ...string) (map[string]any, error) {
	label := strings.Join(args[:min(2, len(args))], " ")
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, e.Binary, append(append([]string{}, args...), "--json")...)
	cmd.WaitDelay = time.Second
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if cctx.Err() == context.DeadlineExceeded {
		return nil, &CallError{Msg: fmt.Sprintf("orca %s timed out after %ss", label, host.Seconds(timeout))}
	}
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		return nil, &CallError{Msg: "could not run orca: " + err.Error()}
	}
	var payload map[string]any
	_ = json.Unmarshal(out.Bytes(), &payload)
	if payload == nil || payload["ok"] != true || code != 0 {
		message := ""
		if payload != nil {
			switch x := payload["error"].(type) {
			case map[string]any:
				message, _ = x["message"].(string)
				if message == "" {
					b, _ := json.Marshal(x)
					message = string(b)
				}
			case nil:
			default:
				if host.Truthy(x) {
					message = fmt.Sprint(x)
				}
			}
		}
		if message == "" {
			message = strings.TrimSpace(errb.String())
			if message == "" {
				message = strings.TrimSpace(out.String())
			}
		}
		if message == "" {
			message = fmt.Sprintf("exit %d", code)
		}
		return nil, &CallError{Msg: "orca " + label + " failed: " + host.Excerpt(message), Payload: payload}
	}
	result, _ := payload["result"].(map[string]any)
	if result == nil {
		result = map[string]any{}
	}
	return result, nil
}

// Available is (true, "") when Orca is enabled, on PATH and its runtime up.
func (e *Executor) Available() (bool, string) {
	if e.cfg.Enabled == host.TriFalse {
		return false, "disabled by config (orca.enabled: false)"
	}
	if _, err := exec.LookPath(e.Binary); err != nil {
		return false, e.Binary + " is not on PATH"
	}
	if _, err := e.call(context.Background(), statusTimeout, "status"); err != nil {
		return false, "orca status not ok: " + err.Error()
	}
	return true, ""
}

func requestID(result any) string {
	r, _ := result.(map[string]any)
	send, _ := r["send"].(map[string]any)
	prompt, _ := send["prompt"].(map[string]any)
	id, _ := prompt["requestId"].(string)
	return id
}

var nameUnsafe = regexp.MustCompile(`[^a-z0-9._-]+`)

func sanitizeName(text string) string {
	s := strings.Trim(nameUnsafe.ReplaceAllString(strings.ToLower(text), "-"), "-.")
	if s == "" {
		return "wave"
	}
	return s
}

// --- registry -----------------------------------------------------------------------

func (e *Executor) sessionStateDir(s *host.Session) string {
	if sd := s.Str("state_dir"); sd != "" {
		return sd
	}
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	return e.stateDir
}

func (e *Executor) save(s *host.Session) {
	if err := e.reg.Save(e.sessionStateDir(s), s); err != nil {
		e.warnf("registry: %s", err)
	}
}

func (e *Executor) forget(s *host.Session) {
	if err := e.reg.Forget(e.sessionStateDir(s), s.Pkg); err != nil {
		e.warnf("registry: %s", err)
	}
}

// Session is the registered session of pkg. A terminal that no longer
// answers `terminal read` is dropped from the session (the next prompt
// opens a new one on the same worktree); a worktree that is gone drops the
// whole entry.
func (e *Executor) Session(pkg, stateDir string) (*host.Session, bool) {
	stateDir = e.adoptStateDir(stateDir)
	if stateDir == "" {
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
		e.forget(s)
		return nil, false
	}
	if s.Terminal != "" {
		if _, err := e.screen(context.Background(), s.Terminal); err != nil {
			s.Terminal = ""
			e.save(s)
		}
	}
	return s, true
}

// --- terminal -------------------------------------------------------------------------

func (e *Executor) screen(ctx context.Context, handle string) ([]string, error) {
	result, err := e.call(ctx, callTimeout, "terminal", "read", "--terminal", handle, "--screen")
	if err != nil {
		return nil, err
	}
	term, _ := result["terminal"].(map[string]any)
	switch tail := term["tail"].(type) {
	case []any:
		out := make([]string, 0, len(tail))
		for _, l := range tail {
			if s, ok := l.(string); ok {
				out = append(out, s)
			} else {
				b, _ := json.Marshal(l)
				out = append(out, string(b))
			}
		}
		return out, nil
	case string:
		return host.SplitLines(tail), nil
	case nil:
		return []string{}, nil
	default:
		return host.SplitLines(fmt.Sprint(tail)), nil
	}
}

// waitIdle is one bounded `terminal wait --for tui-idle`, then a poll
// sleep; a wait that did not settle is just another poll.
func (e *Executor) waitIdle(ctx context.Context, handle string, remaining time.Duration) error {
	d := max(time.Second, min(remaining, waitChunk))
	ms := d.Milliseconds()
	_, _ = e.call(ctx, d+30*time.Second, "terminal", "wait", "--terminal", handle, "--for", "tui-idle",
		"--timeout-ms", fmt.Sprint(ms))
	if e.Poll > 0 {
		return e.Clock.Sleep(ctx, e.Poll)
	}
	return ctx.Err()
}

// command is the hosted claude command line of the session.
func (e *Executor) command(s *host.Session) (string, error) {
	mode := e.cfg.Str("permission_mode")
	if e.cfg.Get("permission_mode") == nil {
		mode = "acceptEdits"
	}
	template := e.cfg.Str("command")
	if !permissionModes[mode] {
		return "", host.Need(host.KindError, "invalid orca.permission_mode: "+mode)
	}
	if strings.Contains(template, "bypassPermissions") || strings.Contains(template, "--dangerously-skip-permissions") {
		return "", host.Need(host.KindError, "orca permission bypass is not allowed")
	}
	model := s.Str("model")
	if model == "" {
		model = "opus"
	}
	command := strings.ReplaceAll(template, "{model}", model)
	command = strings.ReplaceAll(command, "{permission_mode}", mode)
	// The prompt files and the result file live outside the worktree;
	// without --add-dir the session stops on a read/write permission
	// dialog that nobody is there to answer.
	for _, d := range s.Strings("add_dirs") {
		command += " --add-dir " + sandbox.ShellQuote(d)
	}
	tokens, err := host.ShellSplit(command)
	if err != nil {
		return "", host.Need(host.KindError, "invalid orca.command: "+err.Error())
	}
	flags, err := host.IsolationFlags(e.cfg, tokens)
	if err != nil {
		return "", err
	}
	for _, f := range flags {
		command += " " + sandbox.ShellQuote(f)
	}
	return command, nil
}

// openTerminal runs `terminal create` on the session's worktree, then
// waits for the prompt box. The trust dialog is a need without auto_trust.
func (e *Executor) openTerminal(ctx context.Context, s *host.Session) (string, error) {
	command, err := e.command(s)
	if err != nil {
		return "", err
	}
	result, err := e.call(ctx, callTimeout, "terminal", "create", "--worktree", "path:"+s.Worktree,
		"--title", "wave "+s.Pkg, "--command", command)
	if err != nil {
		return "", host.Need(host.KindError, err.Error())
	}
	term, _ := result["terminal"].(map[string]any)
	handle, _ := term["handle"].(string)
	if handle == "" {
		return "", host.Need(host.KindError, "orca terminal create returned no handle")
	}
	s.Terminal = handle
	e.save(s)

	startup := e.cfg.Seconds("startup_timeout", 90)
	deadline := e.Clock.Now().Add(startup)
	trustKeys := 0
	for {
		screen, err := e.screen(ctx, handle)
		if err != nil {
			return "", host.Need(host.KindError, err.Error())
		}
		if host.IsTrust(screen) {
			if !e.cfg.Truthy("auto_trust") {
				return "", host.Need(host.KindTrust, s.Worktree+" needs the folder-trust dialog accepted once; "+
					"open it in Orca or trust ~/orca/workspaces")
			}
			// Arrow and Enter go in separate sends, Enter only once the
			// screen shows "Yes" selected: sent together, Enter lands
			// before the cursor moves and picks the preselected "No, exit".
			if trustKeys >= trustKeyLimit {
				return "", host.Need(host.KindStalled, "folder-trust dialog did not accept:\n"+host.TailLines(screen))
			}
			keys := []string{"--text", down}
			if host.TrustYesSelected(screen) {
				keys = []string{"--enter"}
			}
			if _, err := e.call(ctx, callTimeout, append([]string{"terminal", "send", "--terminal", handle}, keys...)...); err != nil {
				return "", host.Need(host.KindError, err.Error())
			}
			if trustKeys == 0 {
				e.emit(s.Pkg, "trust", handle, nil)
			}
			trustKeys++
		} else if host.HasPromptBox(screen) {
			return handle, nil
		}
		remaining := deadline.Sub(e.Clock.Now())
		if remaining <= 0 {
			return "", host.Need(host.KindStalled, "no Claude prompt after "+host.Seconds(startup)+"s:\n"+host.TailLines(screen))
		}
		if err := e.waitIdle(ctx, handle, remaining); err != nil {
			return "", host.AsNeed(err)
		}
	}
}

func (e *Executor) closeTerminal(s *host.Session) {
	handle := s.Terminal
	if handle == "" {
		return
	}
	if _, err := e.call(context.Background(), callTimeout, "terminal", "close", "--terminal", handle, "--tab"); err != nil {
		e.warnf("%s", err)
	}
	s.Terminal = ""
	e.save(s)
	e.emit(s.Pkg, "close", handle, nil)
}

// --- the contract -------------------------------------------------------------------------

func sha8(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]`)

// Start creates an Orca worktree on baseRef, adopts it as `pkg-<id>`,
// opens a Claude terminal there and waits for its prompt box.
func (e *Executor) Start(ctx context.Context, pkgID, repo, baseRef, cls, model, runID, stateDir string, addDirs []string) (*host.Session, error) {
	stateDir = e.adoptStateDir(stateDir)
	out, err := gitx.Run(repo, "rev-parse", "--verify", baseRef+"^{commit}")
	baseSHA := strings.TrimSpace(out)
	if err != nil || baseSHA == "" {
		return nil, host.Need(host.KindError, "could not resolve "+baseRef+" to a commit")
	}
	run8 := nonAlnum.ReplaceAllString(strings.ToLower(runID), "")
	if len(run8) > 8 {
		run8 = run8[:8]
	}
	if run8 == "" {
		abs, _ := filepath.Abs(stateDir)
		run8 = sha8(abs)
	}
	name := sanitizeName("wave-" + run8 + "-" + pkgID)
	setup := e.cfg.Str("setup")
	if e.cfg.Get("setup") == nil {
		setup = "skip"
	}
	if setup != "skip" && setup != "inherit" && setup != "run" {
		return nil, host.Need(host.KindError, "invalid orca.setup: "+setup)
	}
	result, err := e.call(ctx, worktreeTimeout, "worktree", "create", "--repo", "path:"+repo, "--name", name,
		"--base-branch", baseRef, "--setup", setup, "--no-parent")
	if err != nil {
		return nil, host.Need(host.KindError, err.Error())
	}
	wt, _ := result["worktree"].(map[string]any)
	path, _ := wt["path"].(string)
	if st, err := os.Stat(path); path == "" || err != nil || !st.IsDir() {
		return nil, host.Need(host.KindError, "orca worktree create returned no usable path")
	}
	s := &host.Session{Pkg: pkgID, Name: e.Name(), Worktree: path, Extra: map[string]any{
		"branch": wt["branch"], "request_ids": []any{}, "repo": repo, "model": model, "cls": cls,
		"state_dir": stateDir, "add_dirs": host.SortedDirs(addDirs),
	}}
	e.save(s)
	adopted, err := isolate.Adopt(repo, "pkg-"+pkgID, path, isolate.AdoptOptions{
		Baseline: baseSHA, Carry: true, CarryExtra: e.isolation("carry_extra"), CarryFiles: e.carryFiles(),
	})
	if err != nil || (adopted.Status != "adopted" && adopted.Status != "exists") {
		note := adopted.Note
		if err != nil {
			note = err.Error()
		}
		_ = e.Close(s, true)
		return nil, host.Need(host.KindError, "could not adopt "+path+": "+note)
	}
	if len(adopted.Carried) > 0 || len(adopted.CarriedFiles) > 0 {
		method := adopted.CarryMethod
		if method == "" {
			method = "none"
		}
		e.emit(pkgID, "carry", "", map[string]any{"method": method, "files": len(adopted.CarriedFiles)})
	}
	handle, err := e.openTerminal(ctx, s)
	if err != nil {
		_ = e.Close(s, true)
		return nil, host.AsNeed(err)
	}
	e.emit(pkgID, "start", handle, nil)
	s.Set("fresh", true) // the first prompt is not a resume
	return s, nil
}

func (e *Executor) isolation(key string) []string {
	v, _ := e.tools.Lookup("isolation", key)
	list, _ := v.([]any)
	var out []string
	for _, x := range list {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func (e *Executor) carryFiles() []string {
	v, ok := e.tools.Lookup("isolation", "carry_files")
	if !ok || v == nil {
		return nil
	}
	out := e.isolation("carry_files")
	if out == nil {
		out = []string{}
	}
	return out
}
