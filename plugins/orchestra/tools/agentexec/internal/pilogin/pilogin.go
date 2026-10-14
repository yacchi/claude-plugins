// Package pilogin is `agent-exec pi login`: Claude drives pi's interactive
// /login through orchestra's own herdr, so the user only has to open a URL
// (and type a code).
//
// pi has no non-interactive login, and the user does not run agent-exec or
// pi in a terminal. This starts the managed pi in a pane of a dedicated herdr
// session (orchestra-pilogin-<uid>, never a worker session, not wrapped in
// srt: it is user-initiated and only pi's auth UI runs), types the /login
// keys, reads the screen, and reports the URL/code. wait polls pi's
// credential check until the login lands; paste and cancel finish or abort a
// login in progress.
package pilogin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host/herdr"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/sandbox"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/tools"
)

// The modes of Options.Mode.
const (
	ModeStart  = "start"
	ModeWait   = "wait"
	ModePaste  = "paste"
	ModeCancel = "cancel"
)

// StateName is the file (in the orchestra state dir) a login in progress is
// remembered in.
const StateName = "pilogin.json"

// DefaultTimeout is the --wait / --paste timeout in seconds.
const DefaultTimeout = 600

const (
	authPoll     = 3 * time.Second
	stepTimeout  = 45 * time.Second
	startTimeout = 90 * time.Second
)

// ProviderLabels maps a provider id to the label pi shows in "Select
// provider to configure".
var ProviderLabels = map[string]string{
	"openai-codex":   "OpenAI Codex",
	"openai":         "OpenAI",
	"anthropic":      "Anthropic",
	"github-copilot": "GitHub Copilot",
}

const (
	authMethodLabel = "Sign in with an account"
	deviceLabel     = "Device code login"
	piReadyMarker   = "/ commands"
)

// Options select what Run does. The zero values of the seams are the real
// thing.
type Options struct {
	Mode     string  // start (default), wait, paste, cancel
	Provider string  // start: the provider to log in (default: the first not ready)
	Paste    string  // paste: the text to type
	Timeout  float64 // wait, paste: seconds (callers pass DefaultTimeout unless --timeout)
	Cfg      *config.Resolved

	Herdr    *herdr.Executor // the herdr to drive (default: herdr.New(Cfg))
	PiPath   string          // the pi binary (default: resolved through tools)
	StateDir string          // default: the orchestra state dir
	Cwd      string          // the pane's working directory (default: cwd)
	UID      int             // names the herdr session; 0: the current user
	// Spawn starts the herdr server detached (default: exec with Setsid).
	Spawn func(argv, env []string, log *os.File) error
}

// loginError is an unexpected screen: it carries the step and the screen.
type loginError struct {
	step, screen, reason string
}

func (e *loginError) Error() string { return e.step }

// SessionName is the herdr session the login runs in.
func SessionName(uid int) string {
	return fmt.Sprintf("orchestra-pilogin-%d", uid)
}

type login struct {
	cfg      *config.Resolved
	hx       *herdr.Executor
	piPath   string
	stateDir string
	cwd      string
	sname    string
	spawn    func(argv, env []string, log *os.File) error
}

func newLogin(o Options) *login {
	cfg := o.Cfg
	if cfg == nil {
		cfg = &config.Resolved{Values: config.Defaults(), Root: config.ProjectBase()}
	}
	uid := o.UID
	if uid == 0 {
		uid = os.Getuid()
	}
	l := &login{cfg: cfg, hx: o.Herdr, piPath: o.PiPath, stateDir: o.StateDir, cwd: o.Cwd,
		sname: SessionName(uid), spawn: o.Spawn}
	if l.hx == nil {
		l.hx = herdr.New(cfg)
	}
	if l.stateDir == "" {
		l.stateDir = sandbox.StateDir()
	}
	if l.cwd == "" {
		l.cwd, _ = os.Getwd()
	}
	if l.spawn == nil {
		l.spawn = spawnDetached
	}
	// The herdr plumbing (binary, scrubbed env, own XDG home, call parsing)
	// is the executor's; only the server start differs (no srt).
	l.hx.SetSessionName(l.sname)
	l.hx.SetStateDir(l.stateDir)
	return l
}

func spawnDetached(argv, env []string, log *os.File) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// --- state ----------------------------------------------------------------------

func (l *login) statePath() string { return filepath.Join(l.stateDir, StateName) }

// loadState is the login in progress, nil when there is none.
func (l *login) loadState() map[string]any {
	raw, err := os.ReadFile(l.statePath())
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	if pane, _ := m["pane"].(string); pane == "" {
		return nil
	}
	return m
}

func (l *login) saveState(state map[string]any) error {
	if err := os.MkdirAll(l.stateDir, 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	if err := enc.Encode(state); err != nil { // sorted keys, indent 1
		return err
	}
	tmp := l.statePath() + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, l.statePath())
}

func (l *login) clearState() { _ = os.Remove(l.statePath()) }

// --- pi -------------------------------------------------------------------------

func (l *login) pi() (string, error) {
	if l.piPath != "" {
		return l.piPath, nil
	}
	if path := tools.Resolve("pi", l.cfg).Path; path != "" {
		return path, nil
	}
	return "", &loginError{step: "pi-not-found", reason: "pi is not installed: run `agent-exec tools install pi`"}
}

// authReady is the credential probe doctor shares; a missing pi is a
// loginError, not "not ready".
func (l *login) authReady(provider string) (bool, error) {
	pi, err := l.pi()
	if err != nil {
		return false, err
	}
	return tools.PiAuthReady(l.cfg, pi, provider, l.cwd), nil
}

// --- herdr ----------------------------------------------------------------------

func asCallError(err error) (*herdr.CallError, bool) {
	var ce *herdr.CallError
	return ce, errors.As(err, &ce)
}

func (l *login) serverUp() bool { return l.hx.StatusRunning() }

func (l *login) herdrBinary() string {
	if l.hx.Binary != "" {
		return l.hx.Binary
	}
	return tools.Resolve("herdr", l.cfg).Path
}

func (l *login) startServer() error {
	if l.serverUp() {
		return nil
	}
	home, err := l.hx.HomeDir()
	if err != nil {
		return &herdr.CallError{Msg: err.Error()}
	}
	if err := os.MkdirAll(filepath.Join(home, "herdr"), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(home, "herdr", "config.toml"), []byte(herdr.ConfigTOML), 0o644); err != nil {
		return err
	}
	logDir := filepath.Join(l.stateDir, "herdr")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(logDir, l.sname+".server.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()
	bin := l.herdrBinary()
	if bin == "" {
		return &herdr.CallError{Msg: "herdr is not configured (binary or session name missing)"}
	}
	env, err := l.hx.ProcessEnv("")
	if err != nil {
		return &herdr.CallError{Msg: err.Error()}
	}
	if err := l.spawn([]string{bin, "--session", l.sname, "server"}, env, logFile); err != nil {
		return &loginError{step: "server", reason: "could not start herdr: " + err.Error()}
	}
	deadline := l.now().Add(30 * time.Second)
	for !l.serverUp() {
		if !l.now().Before(deadline) {
			return &loginError{step: "server", reason: "herdr server did not start"}
		}
		l.sleep(time.Second)
	}
	return nil
}

func (l *login) stopServer() {
	if l.serverUp() {
		_, _ = l.hx.Call("server", "stop")
	}
}

func (l *login) now() time.Time { return l.hx.Clock.Now() }

func (l *login) sleep(d time.Duration) { _ = l.hx.Clock.Sleep(context.Background(), d) }

func (l *login) read(pane string) (string, error) {
	payload, out, err := l.hx.CallPayload("pane", "read", pane, "--source", "visible")
	if err != nil {
		return "", err
	}
	return ScreenText(out, payload), nil
}

func (l *login) keys(pane string, keys ...string) error {
	_, err := l.hx.Call(append([]string{"pane", "send-keys", pane}, keys...)...)
	return err
}

func (l *login) text(pane, text string) error {
	_, err := l.hx.Call("pane", "send-text", pane, text)
	return err
}

// waitScreen polls the pane until pred(screen); else a loginError.
func (l *login) waitScreen(pane, step string, pred func(string) bool, timeout time.Duration) (string, error) {
	deadline := l.now().Add(timeout)
	for {
		screen, err := l.read(pane)
		if err != nil {
			return "", err
		}
		if pred(screen) {
			return screen, nil
		}
		if !l.now().Before(deadline) {
			return "", &loginError{step: step, screen: screen}
		}
		l.sleep(time.Second)
	}
}

// cleanup closes the login pane and stops the login server. Best effort; it
// only ever touches this package's own session.
func (l *login) cleanup(state map[string]any, escape bool) {
	if state == nil {
		state = l.loadState()
	}
	if state != nil {
		pane, _ := state["pane"].(string)
		if escape {
			_ = l.keys(pane, "esc")
		}
		if ws, _ := state["workspace"].(string); ws != "" {
			_, _ = l.hx.Call("workspace", "close", ws)
		} else {
			_, _ = l.hx.Call("pane", "close", pane)
		}
	}
	l.stopServer()
	l.clearState()
}

// --- the flow -------------------------------------------------------------------

func manual(step, reason, screen string) map[string]any {
	out := map[string]any{"status": "manual", "step": step, "screen_tail": Tail(screen, 25)}
	if reason != "" {
		out["reason"] = reason
	}
	return out
}

// start drives /login up to the URL screen.
func (l *login) start(provider string) (int, map[string]any, error) {
	providers := []string{provider}
	if provider == "" {
		providers = tools.ConfiguredProviders(l.cfg)
	}
	pending := ""
	for _, candidate := range providers {
		ok, err := l.authReady(candidate)
		if err != nil {
			return 0, nil, err
		}
		if !ok {
			pending = candidate
			break
		}
	}
	if pending == "" {
		return 0, map[string]any{"status": "ready", "providers": providers}, nil
	}
	label, known := ProviderLabels[pending]
	if !known {
		return 1, map[string]any{"status": "manual", "step": "provider",
			"reason": "no known /login label for provider " + pending, "screen_tail": ""}, nil
	}
	if previous := l.loadState(); previous != nil {
		l.cleanup(previous, true)
	}
	var state map[string]any
	fail := func(err error) (int, map[string]any, error) {
		var le *loginError
		if errors.As(err, &le) {
			l.cleanup(state, false)
			return 1, manual(le.step, le.reason, le.screen), nil
		}
		if ce, ok := asCallError(err); ok {
			l.cleanup(state, false)
			return 1, map[string]any{"status": "manual", "step": "herdr", "reason": ce.Error(), "screen_tail": ""}, nil
		}
		return 0, nil, err
	}
	if err := l.startServer(); err != nil {
		return fail(err)
	}
	payload, _, err := l.hx.CallPayload("workspace", "create", "--cwd", l.cwd, "--label", "pi login", "--no-focus")
	if err != nil {
		return fail(err)
	}
	workspace, _ := herdr.Find(payload, "workspace_id").(string)
	pane, _ := herdr.Find(payload, "pane_id").(string)
	if workspace == "" || pane == "" {
		return fail(&loginError{step: "workspace", reason: "herdr returned no pane"})
	}
	state = map[string]any{"pane": pane, "workspace": workspace, "provider": pending, "session": l.sname}
	if err := l.saveState(state); err != nil {
		return 0, nil, err
	}
	result, err := l.drive(state, label)
	if err != nil {
		return fail(err)
	}
	merged := map[string]any{}
	for k, v := range state {
		merged[k] = v
	}
	merged["method"] = result["method"]
	if err := l.saveState(merged); err != nil {
		return 0, nil, err
	}
	return 0, result, nil
}

var unsafeShellRE = regexp.MustCompile(`[^A-Za-z0-9_@%+=:,./-]`)

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !unsafeShellRE.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func urlScreen(s string) bool {
	if _, ok := ExtractURL(s); !ok {
		return false
	}
	if _, ok := ExtractCode(s); ok {
		return true
	}
	return WantsPaste(s) || strings.Contains(strings.ToLower(s), "waiting for authentication")
}

func (l *login) drive(state map[string]any, label string) (map[string]any, error) {
	pane, _ := state["pane"].(string)
	provider, _ := state["provider"].(string)
	pi, err := l.pi()
	if err != nil {
		return nil, err
	}
	_, overrides := tools.PiLoginEnv(l.cfg)
	names := make([]string, 0, len(overrides))
	for k := range overrides {
		names = append(names, k)
	}
	sort.Strings(names)
	var assignments []string
	for _, k := range names {
		assignments = append(assignments, k+"="+shellQuote(overrides[k]))
	}
	assignments = append(assignments, tools.PiSkipVersionCheck)
	if _, err := l.hx.Call("pane", "run", pane, strings.Join(append(assignments, shellQuote(pi)), " ")); err != nil {
		return nil, err
	}
	if _, err := l.waitScreen(pane, "start-pi", func(s string) bool { return strings.Contains(s, piReadyMarker) }, startTimeout); err != nil {
		return nil, err
	}

	if err := l.text(pane, "/login"); err != nil {
		return nil, err
	}
	l.sleep(time.Second)
	if err := l.keys(pane, "enter"); err != nil {
		return nil, err
	}
	if _, err := l.waitScreen(pane, "auth-method", func(s string) bool { return strings.Contains(s, "Select authentication method") }, stepTimeout); err != nil {
		return nil, err
	}
	screen, err := l.read(pane)
	if err != nil {
		return nil, err
	}
	if h, _ := Highlighted(screen); h != authMethodLabel {
		return nil, &loginError{step: "auth-method", screen: screen}
	}
	if err := l.keys(pane, "enter"); err != nil {
		return nil, err
	}

	if _, err := l.waitScreen(pane, "provider-list", func(s string) bool { return strings.Contains(s, "Select provider to configure") }, stepTimeout); err != nil {
		return nil, err
	}
	if err := l.text(pane, label); err != nil {
		return nil, err
	}
	deadline := l.now().Add(stepTimeout)
	for {
		screen, err = l.read(pane)
		if err != nil {
			return nil, err
		}
		if h, _ := Highlighted(screen); h == label {
			break
		}
		if !l.now().Before(deadline) {
			return nil, &loginError{step: "provider-select", screen: screen}
		}
		l.sleep(time.Second)
	}
	if err := l.keys(pane, "enter"); err != nil {
		return nil, err
	}

	screen, err = l.waitScreen(pane, "login-method", func(s string) bool {
		return urlScreen(s) || strings.Contains(s, "Device code")
	}, stepTimeout)
	if err != nil {
		return nil, err
	}
	if !urlScreen(screen) {
		deadline = l.now().Add(stepTimeout)
		for {
			if h, _ := Highlighted(screen); strings.HasPrefix(h, deviceLabel) {
				break
			}
			if !l.now().Before(deadline) {
				return nil, &loginError{step: "device-select", screen: screen}
			}
			if err := l.keys(pane, "down"); err != nil {
				return nil, err
			}
			l.sleep(time.Second)
			if screen, err = l.read(pane); err != nil {
				return nil, err
			}
		}
		if err := l.keys(pane, "enter"); err != nil {
			return nil, err
		}
		if screen, err = l.waitScreen(pane, "login-url", urlScreen, stepTimeout); err != nil {
			return nil, err
		}
	}
	url, ok := ExtractURL(screen)
	if !ok {
		return nil, &loginError{step: "login-url", screen: screen}
	}
	code, hasCode := ExtractCode(screen)
	method := "browser"
	if hasCode {
		method = "device"
	}
	result := map[string]any{"status": "waiting", "provider": provider, "method": method,
		"url": url, "paste_prompt": WantsPaste(screen), "session": l.sname}
	if hasCode {
		result["code"] = code
	}
	return result, nil
}

func noLogin() (int, map[string]any, error) {
	return 1, map[string]any{"status": "failed",
		"reason": "no login in progress (run `agent-exec pi login` first)", "screen_tail": ""}, nil
}

// alive: pi is still on its login screen (waiting, or asking for a paste).
func alive(screen string) bool {
	lower := strings.ToLower(screen)
	for _, marker := range []string{"waiting for authentication", "paste", "enter code", "https://", "select ", "device code"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func (l *login) wait(timeoutSeconds float64) (int, map[string]any, error) {
	state := l.loadState()
	if state == nil {
		return noLogin()
	}
	provider, _ := state["provider"].(string)
	pane, _ := state["pane"].(string)
	deadline := l.now().Add(secondsDuration(timeoutSeconds))
	screen := ""
	ready := func() (bool, error) {
		ok, err := l.authReady(provider)
		if ok {
			l.cleanup(state, false)
		}
		return ok, err
	}
	readyResult := map[string]any{"status": "ready", "provider": provider}
	for {
		ok, err := ready()
		if err != nil {
			return 0, nil, err
		}
		if ok {
			return 0, readyResult, nil
		}
		var rerr error
		screen, rerr = l.read(pane)
		if rerr != nil {
			if _, isCall := asCallError(rerr); !isCall {
				return 0, nil, rerr
			}
			if ok, err := ready(); err != nil || ok {
				return 0, readyResult, err
			}
			l.cleanup(state, false)
			return 1, map[string]any{"status": "failed", "reason": "the login pane is gone",
				"screen_tail": Tail(screen, 25)}, nil
		}
		if !alive(screen) {
			if ok, err := ready(); err != nil || ok {
				return 0, readyResult, err
			}
			l.cleanup(state, false)
			return 1, map[string]any{"status": "failed", "reason": "pi exited",
				"screen_tail": Tail(screen, 25)}, nil
		}
		if !l.now().Before(deadline) {
			return 1, map[string]any{"status": "timeout", "provider": provider}, nil
		}
		l.sleep(authPoll)
	}
}

func (l *login) paste(text string, timeoutSeconds float64) (int, map[string]any, error) {
	state := l.loadState()
	if state == nil {
		return noLogin()
	}
	pane, _ := state["pane"].(string)
	err := l.text(pane, text)
	if err == nil {
		err = l.keys(pane, "enter")
	}
	if err != nil {
		ce, isCall := asCallError(err)
		if !isCall {
			return 0, nil, err
		}
		l.cleanup(state, false)
		return 1, map[string]any{"status": "failed", "reason": ce.Error(), "screen_tail": ""}, nil
	}
	return l.wait(timeoutSeconds)
}

func (l *login) cancel() (int, map[string]any, error) {
	l.cleanup(nil, true)
	return 0, map[string]any{"status": "cancelled"}, nil
}

// Run performs one `pi login` step and returns its JSON result and exit
// code (0 ready/waiting/cancelled, 1 failed/manual/timeout).
func Run(o Options) (map[string]any, int) {
	l := newLogin(o)
	timeout := o.Timeout
	var (
		code   int
		result map[string]any
		err    error
	)
	switch o.Mode {
	case ModeWait:
		code, result, err = l.wait(timeout)
	case ModePaste:
		code, result, err = l.paste(o.Paste, timeout)
	case ModeCancel:
		code, result, err = l.cancel()
	default:
		code, result, err = l.start(o.Provider)
	}
	if err != nil {
		var le *loginError
		if errors.As(err, &le) {
			return map[string]any{"status": "manual", "step": le.step,
				"reason": le.reason, "screen_tail": Tail(le.screen, 25)}, 1
		}
		return map[string]any{"status": "manual", "step": "herdr", "reason": err.Error(), "screen_tail": ""}, 1
	}
	return result, code
}

// secondsDuration converts seconds, saturating (and reading NaN as "never")
// instead of overflowing.
func secondsDuration(s float64) time.Duration {
	const limit = 1e9
	if s != s || s > limit {
		s = limit
	}
	if s < -limit {
		s = -limit
	}
	return time.Duration(s * float64(time.Second))
}
