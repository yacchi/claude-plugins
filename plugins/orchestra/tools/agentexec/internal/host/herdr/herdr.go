// Package herdr is the herdr host of `wave run`: Claude Code sessions that
// stay alive, hosted by herdr (a terminal multiplexer with an `agent` API).
//
// Every herdr call carries `--session <S>` (S = "orchestra-" + the first 8
// characters of the Claude session id, falling back to the run id), so the
// user's own herdr session is never touched, and XDG_CONFIG_HOME points it
// at `<orchestra state dir>/hx` so the user's herdr config is never read.
// The server is started detached when it is not running and stopped by
// Shutdown once no workspace is left open in it, whoever started it. Each
// package gets an orchestra-owned worktree (task `pkg-<id>`), one workspace
// and one named agent in it, recorded in the tree (isolate.WriteHostSession)
// so a removal that knows nothing about herdr closes it; completion is
// signalled only by the result file.
//
// The server runs under the executor sandbox (Seatbelt / bubblewrap), or
// under sandbox-runtime (srt) with `sandbox.network.mode: allowlist`: the
// same host policy plus an outbound domain allowlist, blocked connections
// surfaced per package as network_denials, approved domains pushed live
// through srt's control FIFO. Only there may `herdr.permission_mode` be
// bypassPermissions. Sessions persist in `<state dir>/herdr-sessions.json`.
package herdr

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/srt"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/tools"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/trace"
)

// RegistryName is the session registry file in the wave state dir.
const RegistryName = "herdr-sessions.json"

// WarmKey is the registry key of the warm reader records; never a package.
const WarmKey = "_warm"

const (
	statusTimeout   = 15 * time.Second
	callTimeout     = 60 * time.Second
	promptRetryWait = 2 * time.Second
	inputSettle     = 1 * time.Second
	shellRetries    = 3
	bypassMode      = "bypassPermissions"
	sockLimit       = 100 // a unix socket path must stay under sun_path
	homeDir         = "hx"
)

var permissionModes = map[string]bool{"acceptEdits": true, "auto": true, "default": true, "plan": true}

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// ConfigTOML is the herdr config orchestra pins in its own herdr home: no
// first-run onboarding, toasts, sounds, update checks or agent resume.
const ConfigTOML = `onboarding = false

[update]
version_check = false

[ui.toast]
delivery = "off"

[ui.sound]
enabled = false

[session]
resume_agents_on_restore = false

[experimental]
pane_history = false
`

// CallError is one failed herdr call; Code is herdr's error code, if any.
type CallError struct {
	Msg     string
	Code    string
	Payload map[string]any
}

func (e *CallError) Error() string { return e.Msg }

// Executor runs packages in herdr-hosted Claude Code sessions.
//
// The exported fields are seams for tests and for pilogin; New fills the
// defaults.
type Executor struct {
	Binary string   // the herdr binary; "" resolves `herdr` through tools
	Env    []string // the environment herdr calls start from; nil = os.Environ()
	Home   string   // XDG_CONFIG_HOME of every herdr call; "" = ResolveHome
	// SrtStatus says whether sandbox-runtime can confine the server; nil
	// resolves it from the config (srt.ResolveStatus).
	SrtStatus func() srt.StatusInfo
	Clock     host.Clock
	Poll      time.Duration
	// OnSpawn observes the server's argv and env right before it starts.
	OnSpawn func(argv, env []string)
	Stderr  io.Writer

	cfg      host.Config
	tools    *config.Resolved
	reg      host.Registry
	onEvent  host.EventFunc
	mu       sync.Mutex // server state
	warmMu   sync.Mutex
	stateMu  sync.Mutex
	sname    string
	runID    string // the wave run id, set once by Start
	stateDir string
	repo     string
	homeName string
	homePath string

	sandboxed, serverReady bool
	unsandboxed            string
	netRestricted          bool
	netReason              string
	netMu                  sync.Mutex // guards srtRun and every call on it
	srtRun                 *srt.Run
	homeMu                 sync.Mutex

	warmPaths  []string
	warmCounts map[string]int
	warmFailed map[string]bool
}

var (
	_ host.Host    = (*Executor)(nil)
	_ host.Warm    = (*Executor)(nil)
	_ host.Network = (*Executor)(nil)
	_ host.Runtime = (*Executor)(nil)
)

// New is the herdr host under cfg (nil: the defaults).
func New(cfg *config.Resolved) *Executor {
	e := &Executor{
		cfg: host.MergeConfig(cfg, "herdr"), tools: cfg,
		reg: host.Registry{FileName: RegistryName}, Clock: host.RealClock(),
		Poll: 2 * time.Second, Stderr: os.Stderr, warmFailed: map[string]bool{},
	}
	e.sname = SessionName("", nil)
	return e
}

// Name is "herdr".
func (e *Executor) Name() string { return "herdr" }

// Enabled is the normalized `herdr.enabled`.
func (e *Executor) Enabled() host.Tri { return e.cfg.Enabled }

// Config is the merged `herdr` section.
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

// StateDir is the state dir in use ("" before any was named).
func (e *Executor) StateDir() string {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	return e.stateDir
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

// SessionName is the herdr session every call uses ("" until known).
func (e *Executor) SessionName() string { return e.sname }

// SetSessionName overrides the herdr session (pilogin uses its own).
func (e *Executor) SetSessionName(name string) { e.sname = name }

// ModelFor is `herdr.models[cls]`, else opus.
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
	fmt.Fprintf(e.Stderr, "agent-exec: herdr: "+format+"\n", args...)
}

func (e *Executor) now() time.Time { return e.Clock.Now() }

func (e *Executor) sleep(ctx context.Context, d time.Duration) error { return e.Clock.Sleep(ctx, d) }

// pollShort is min(poll, 1s) (0 when polling is off).
func (e *Executor) pollShort() time.Duration {
	return min(e.Poll, time.Second)
}

// --- names and environment ----------------------------------------------------

func sanitize(text string) string {
	s := regexp.MustCompile(`[^a-z0-9_-]+`).ReplaceAllString(strings.ToLower(text), "-")
	return strings.Trim(s, "-_")
}

func sha8(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

// AgentName is a herdr agent name ([a-z][a-z0-9_-]{0,31}) unique per package.
func AgentName(pkg string) string {
	name := "p-" + sanitize(pkg)
	if len(name) > 32 {
		name = name[:23] + "-" + sha8(pkg)
	}
	if nameRE.MatchString(name) {
		return name
	}
	return "p-" + sha8(pkg)
}

var sessionRE = regexp.MustCompile(`^[a-z0-9]{8}$`)

// SessionName is "orchestra-" + 8 chars of the Claude session id
// (CLAUDE_CODE_SESSION_ID of env, nil = the process env), else of the run
// id; "" when neither gives one.
func SessionName(runID string, env []string) string {
	var sid string
	if env == nil {
		sid = os.Getenv("CLAUDE_CODE_SESSION_ID")
	} else {
		sid = envValue(env, "CLAUDE_CODE_SESSION_ID")
	}
	r := []rune(sid)
	if len(r) > 8 {
		r = r[:8]
	}
	if c := strings.ToLower(string(r)); sessionRE.MatchString(c) {
		return "orchestra-" + c
	}
	c := regexp.MustCompile(`[^a-z0-9]`).ReplaceAllString(strings.ToLower(runID), "")
	if len(c) > 8 {
		c = c[:8]
	}
	if c == "" {
		return ""
	}
	return "orchestra-" + c
}

func envValue(env []string, key string) string {
	v := ""
	for _, kv := range env {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v = val
		}
	}
	return v
}

// ServerEnv is env without Claude Code session markers (a child Claude
// inherits them and stops saving transcripts) and nothing of the user's own
// herdr. CLAUDE_CONFIG_DIR stays.
func ServerEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "CLAUDE_CODE_") || strings.HasPrefix(k, "HERDR_") {
			continue
		}
		switch k {
		case "CLAUDECODE", "CLAUDE_PID", "CLAUDE_EFFORT", "XDG_CONFIG_HOME":
			continue
		}
		out = append(out, kv)
	}
	return out
}

func (e *Executor) baseEnv() []string {
	if e.Env != nil {
		return e.Env
	}
	return os.Environ()
}

func (e *Executor) binary() string {
	if e.Binary != "" {
		return e.Binary
	}
	return tools.Resolve("herdr", e.tools).Path
}

// Argv is the herdr argv prefix of session sname ("" = this executor's).
func (e *Executor) argv(sname string, args ...string) ([]string, error) {
	bin := e.binary()
	if sname == "" {
		sname = e.sname
	}
	if bin == "" || sname == "" {
		return nil, &CallError{Msg: "herdr is not configured (binary or session name missing)"}
	}
	return append([]string{bin, "--session", sname}, args...), nil
}

// HomeDir is the herdr home of the executor's session.
func (e *Executor) HomeDir() (string, error) { return e.homeFor(e.sname) }

func (e *Executor) homeFor(sname string) (string, error) {
	if e.Home != "" {
		return e.Home, nil
	}
	e.homeMu.Lock()
	defer e.homeMu.Unlock()
	if e.homePath == "" || e.homeName != sname {
		home, err := ResolveHome(sname, "", "", -1, true)
		if err != nil {
			return "", err
		}
		e.homeName, e.homePath = sname, home
	}
	return e.homePath, nil
}

// ProcessEnv is the environment of every herdr process of session sname
// ("" = the executor's): scrubbed, and pointed at orchestra's herdr home.
func (e *Executor) ProcessEnv(sname string) ([]string, error) {
	if sname == "" {
		sname = e.sname
	}
	home, err := e.homeFor(sname)
	if err != nil {
		return nil, err
	}
	return append(ServerEnv(e.baseEnv()), "XDG_CONFIG_HOME="+home), nil
}

// --- calls ----------------------------------------------------------------------

// Available is (true, "") when herdr is enabled, resolvable and answers.
func (e *Executor) Available() (bool, string) {
	if e.cfg.Enabled == host.TriFalse {
		return false, "disabled by config (herdr.enabled: false)"
	}
	bin := e.binary()
	if bin == "" {
		return false, "herdr binary not found"
	}
	sname := e.sname
	if sname == "" {
		sname = "orchestra-probe"
	}
	env, err := e.ProcessEnv(sname)
	if err != nil {
		return false, host.AsNeed(err).Detail
	}
	stdout, stderr, code, err := run(context.Background(), []string{bin, "--session", sname, "--version"}, env, statusTimeout)
	if err != nil {
		return false, "could not run herdr: " + err.Error()
	}
	if code != 0 {
		msg := stderr
		if msg == "" {
			msg = stdout
		}
		return false, "herdr --version failed: " + strings.TrimSpace(msg)
	}
	return true, ""
}

var errTimeout = errors.New("timeout")

func run(ctx context.Context, argv, env []string, timeout time.Duration) (string, string, int, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	cmd.WaitDelay = time.Second
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return out.String(), errb.String(), -1, errTimeout
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out.String(), errb.String(), ee.ExitCode(), nil
	}
	if err != nil {
		return "", "", -1, err
	}
	return out.String(), errb.String(), 0, nil
}

// Call runs one herdr call of the executor's session and returns its raw
// stdout; a failure is a *CallError (pilogin drives panes through it).
func (e *Executor) Call(args ...string) (string, error) {
	_, out, err := e.call(context.Background(), callTimeout, "", args...)
	return out, err
}

// CallTimeout is Call with a timeout.
func (e *Executor) CallTimeout(timeout time.Duration, args ...string) (string, error) {
	_, out, err := e.call(context.Background(), timeout, "", args...)
	return out, err
}

// CallPayload is Call returning the parsed payload as well (an ordered
// an object, empty when stdout was not a JSON object).
func (e *Executor) CallPayload(args ...string) (map[string]any, string, error) {
	return e.call(context.Background(), callTimeout, "", args...)
}

func (e *Executor) call(ctx context.Context, timeout time.Duration, sname string, args ...string) (map[string]any, string, error) {
	argv, err := e.argv(sname, args...)
	if err != nil {
		return nil, "", err
	}
	env, err := e.ProcessEnv(sname)
	if err != nil {
		return nil, "", &CallError{Msg: host.AsNeed(err).Detail}
	}
	label := strings.Join(args[:min(2, len(args))], " ")
	// An agent prompt's text is not traced, only the agent's name.
	detail := strings.Join(args, " ")
	if len(args) >= 2 && args[0] == "agent" && args[1] == "prompt" {
		detail = ""
		if len(args) >= 3 {
			detail = args[2]
		}
	}
	end := trace.Begin(trace.KindHerdr, "herdr "+label, trace.Attrs{Detail: detail})
	stdout, stderr, code, err := run(ctx, argv, env, timeout)
	end(code)
	if errors.Is(err, errTimeout) {
		return nil, "", &CallError{Msg: fmt.Sprintf("herdr %s timed out after %ss", label, host.Seconds(timeout))}
	}
	if err != nil {
		return nil, "", &CallError{Msg: "could not run herdr: " + err.Error()}
	}
	// herdr prints its error object on stderr and leaves stdout empty.
	var obj map[string]any
	for _, text := range []string{stdout, stderr} {
		if v, err := fsutil.DecodeJSON([]byte(text)); err == nil {
			obj, _ = v.(map[string]any)
			break
		}
	}
	var errVal any
	if obj != nil {
		errVal = obj["error"]
	}
	if code != 0 || host.Truthy(errVal) {
		errObj, _ := errVal.(map[string]any)
		codeStr, _ := get(errObj, "code").(string)
		msg, _ := get(errObj, "message").(string)
		if msg == "" {
			msg = strings.TrimSpace(stderr)
			if msg == "" {
				msg = strings.TrimSpace(stdout)
			}
		}
		if msg == "" {
			msg = fmt.Sprintf("exit %d", code)
		}
		return nil, stdout, &CallError{Msg: "herdr " + label + " failed: " + host.Excerpt(msg), Code: codeStr, Payload: obj}
	}
	if obj == nil {
		obj = map[string]any{}
	}
	return obj, stdout, nil
}

func get(o map[string]any, k string) any {
	if o == nil {
		return nil
	}
	v := o[k]
	return v
}

// Find is the first value stored under key anywhere in a parsed payload
// (objects in their key order, depth first).
func Find(payload any, key string) any {
	switch x := payload.(type) {
	case map[string]any:
		if x == nil {
			return nil
		}
		if v, ok := x[key]; ok {
			return v
		}
		for _, k := range slices.Sorted(maps.Keys(x)) {
			v := x[k]
			if found := Find(v, key); found != nil {
				return found
			}
		}
	case []any:
		for _, v := range x {
			if found := Find(v, key); found != nil {
				return found
			}
		}
	}
	return nil
}

// Strings is every string in a parsed payload, depth first.
func Strings(payload any) []string {
	switch x := payload.(type) {
	case string:
		return []string{x}
	case map[string]any:
		if x == nil {
			return nil
		}
		var out []string
		for _, k := range slices.Sorted(maps.Keys(x)) {
			v := x[k]
			out = append(out, Strings(v)...)
		}
		return out
	case []any:
		var out []string
		for _, v := range x {
			out = append(out, Strings(v)...)
		}
		return out
	}
	return nil
}

var notRunningRE = regexp.MustCompile(`not running|not started|stopped|no server`)

// ServerRunning reads `herdr status` output: a "running"/"server_running"
// boolean anywhere in its JSON, else the words of its text.
func ServerRunning(out string) bool {
	payload, err := fsutil.DecodeJSON([]byte(out))
	isJSON := err == nil && payload != nil
	if isJSON {
		for _, key := range []string{"running", "server_running"} {
			if b, ok := Find(payload, key).(bool); ok {
				return b
			}
		}
	}
	text := out
	if isJSON {
		if joined := strings.Join(Strings(payload), " "); joined != "" {
			text = joined
		}
	}
	text = strings.ToLower(text)
	if notRunningRE.MatchString(text) {
		return false
	}
	return strings.Contains(text, "running")
}

// StatusRunning asks herdr whether the session's server runs.
func (e *Executor) StatusRunning() bool {
	_, out, err := e.call(context.Background(), statusTimeout, "", "status")
	if err != nil {
		return false
	}
	return ServerRunning(out)
}
