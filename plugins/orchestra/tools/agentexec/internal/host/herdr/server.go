package herdr

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/sandbox"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/srt"
)

// ResolveHome is the directory used as XDG_CONFIG_HOME for orchestra's
// herdr: base (default `<orchestra state dir>/hx`), or
// `<fallbackRoot>/orchestra-<uid>/hx` (default root /private/tmp on macOS,
// /tmp elsewhere) when the longest socket path under base would not fit a
// unix socket name. The fallback parent must be a private directory of
// this user; uid < 0 is the current user's.
func ResolveHome(sname, base, fallbackRoot string, uid int, create bool) (string, error) {
	if base == "" {
		base = filepath.Join(sandbox.StateDir(), homeDir)
	}
	longest := filepath.Join(base, "herdr", "sessions", sname, "herdr-client.sock")
	if len(longest) < sockLimit {
		return base, nil
	}
	if uid < 0 {
		uid = os.Getuid()
	}
	root := fallbackRoot
	if root == "" {
		root = "/tmp"
		if runtime.GOOS == "darwin" {
			root = "/private/tmp"
		}
	}
	parent := filepath.Join(root, "orchestra-"+strconv.Itoa(uid))
	if _, err := os.Lstat(parent); create && os.IsNotExist(err) {
		if err := os.Mkdir(parent, 0o700); err != nil && !os.IsExist(err) {
			return "", host.Need(host.KindError, err.Error())
		}
	}
	if info, err := os.Lstat(parent); err == nil {
		st, _ := info.Sys().(*syscall.Stat_t)
		if !info.IsDir() || st == nil || int(st.Uid) != uid || info.Mode().Perm()&0o022 != 0 {
			return "", host.Need(host.KindError, parent+" is not a private directory of this user (herdr home fallback)")
		}
	}
	return filepath.Join(parent, homeDir), nil
}

// networkPlan is (srt path, reason): srt confines the server only when the
// host is sandboxed, the network mode is allowlist and srt is usable;
// reason says why the network is not restricted otherwise.
func networkPlan(cfg *config.Resolved, spec sandbox.Spec, status func() srt.StatusInfo) (string, string) {
	if !(spec.Enforced && spec.Policy != nil) {
		reason := spec.Reason
		if reason == "" {
			reason = "unknown"
		}
		return "", "host not sandboxed: " + reason
	}
	if sandbox.ConfigOf(cfg).Network.Mode != "allowlist" {
		return "", "sandbox.network.mode is off"
	}
	st := status()
	return st.Path, st.Reason
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// HostReport is the doctor view of the herdr host: its home, whether the
// server would be sandboxed (and so whether auto_trust could apply), and
// whether its network would be restricted by srt.
func HostReport(cfg *config.Resolved) any {
	return hostReport(cfg, func() srt.StatusInfo { return srt.ResolveStatus(cfg) })
}

func hostReport(cfg *config.Resolved, status func() srt.StatusInfo) map[string]any {
	cwd, _ := os.Getwd()
	home, err := ResolveHome("orchestra-00000000", "", "", -1, false)
	if err != nil {
		return map[string]any{"home": nil, "sandboxed": false, "reason": host.AsNeed(err).Detail}
	}
	spec := sandbox.HostPolicy(cfg, sandbox.HostInput{Repo: cwd, WorktreeBases: isolate.WorktreeBases(cwd), HerdrHome: home}).Spec
	net := sandbox.ConfigOf(cfg).Network
	st := status()
	path, reason := networkPlan(cfg, spec, func() srt.StatusInfo { return st })
	netReason := ""
	if path == "" {
		netReason = "network not restricted: " + reason
	}
	allowed := net.AllowedDomains
	if allowed == nil {
		allowed = []string{}
	}
	return map[string]any{
		"home":      home,
		"sandboxed": spec.Enforced && spec.Policy != nil,
		"backend":   spec.Backend,
		"reason":    spec.Reason,
		"network": map[string]any{
			"mode":            net.Mode,
			"allowed_domains": allowed,
			"srt": map[string]any{
				"path":    nullable(st.Path),
				"version": nullable(st.Version),
				"reason":  st.Reason,
			},
			"restricted": path != "",
			"reason":     netReason,
		},
	}
}

func (e *Executor) srtStatus() srt.StatusInfo {
	if e.SrtStatus != nil {
		return e.SrtStatus()
	}
	return srt.ResolveStatus(e.tools)
}

// --- server -----------------------------------------------------------------

func (e *Executor) statusRunning(ctx context.Context) bool {
	_, out, err := e.call(ctx, statusTimeout, "", "status")
	if err != nil {
		return false
	}
	return ServerRunning(out)
}

// restartServer drops the server latch and starts or attaches the server
// again. openAgent calls it when a workspace create fails with the server
// gone, since ensureServer alone would trust the latch.
func (e *Executor) restartServer(ctx context.Context) error {
	e.mu.Lock()
	e.serverReady = false
	e.closeControl()
	e.mu.Unlock()
	return e.ensureServer(ctx)
}

func (e *Executor) ensureServer(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.serverReady {
		if e.statusRunning(ctx) {
			return nil
		}
		// Another run's refresh may have stopped the server this latch
		// still believes in; forget it and attach or start again.
		e.serverReady = false
		e.closeControl()
	}
	if e.statusRunning(ctx) {
		attached, err := e.attach(ctx)
		if err != nil {
			return err
		}
		if attached {
			e.serverReady = true
			return nil
		}
	}
	home, err := e.homeFor(e.sname)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(home, "herdr"), 0o700); err != nil {
		return host.Need(host.KindError, "could not start the herdr server: "+err.Error())
	}
	if err := os.WriteFile(filepath.Join(home, "herdr", "config.toml"), []byte(ConfigTOML), 0o644); err != nil {
		return host.Need(host.KindError, "could not start the herdr server: "+err.Error())
	}
	spec, err := e.hostSpec(home)
	if err != nil {
		return err
	}
	procEnv, err := e.ProcessEnv("")
	if err != nil {
		return err
	}
	env := sandbox.ChildEnv(spec, procEnv)
	e.setSandboxed(&spec, "")
	srtPath, why := networkPlan(e.tools, spec, e.srtStatus)
	stateDir := e.StateDir()
	if stateDir == "" {
		stateDir, _ = os.Getwd()
	}
	logDir := filepath.Join(stateDir, "herdr")
	_ = os.MkdirAll(logDir, 0o755)
	log, err := os.OpenFile(filepath.Join(logDir, e.sname+".server.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return host.Need(host.KindError, "could not start the herdr server: "+err.Error())
	}
	defer log.Close()
	if srtPath != "" {
		err = e.spawnSrt(srtPath, spec, home, env, log)
	} else {
		e.setNetwork(false, why)
		argv, _ := e.argv("", "server")
		cwd, _ := os.Getwd()
		err = e.start(e.command(sandbox.WrapArgv(argv, cwd, spec), env, log, log))
	}
	if err != nil {
		e.closeControl()
		return host.Need(host.KindError, "could not start the herdr server: "+err.Error())
	}
	deadline := e.now().Add(e.cfg.Seconds("startup_timeout", 90))
	for {
		if e.statusRunning(ctx) {
			e.serverReady = true
			return nil
		}
		if !e.now().Before(deadline) {
			return host.Need(host.KindError, "herdr server did not start")
		}
		if err := e.sleep(ctx, e.pollShort()); err != nil {
			return host.AsNeed(err)
		}
	}
}

// command is a detached server process: own session, no stdin.
func (e *Executor) command(argv, env []string, stdout, stderr *os.File) *exec.Cmd {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd
}

func (e *Executor) start(cmd *exec.Cmd) error {
	if e.OnSpawn != nil {
		e.OnSpawn(cmd.Args, cmd.Env)
	}
	err := cmd.Start()
	if err == nil {
		go func() { _ = cmd.Wait() }() // reap; the server outlives nothing of ours
	}
	return err
}

// attach adopts the server an earlier run left running. Its network
// settings stay live through its control FIFO; when that is unusable
// (missing, not a FIFO, nobody reading) its settings are stale, and the
// server is stopped for a fresh start when srt is usable and no registered
// session of this state is mid-task. false: it was stopped.
func (e *Executor) attach(ctx context.Context) (bool, error) {
	net := sandbox.ConfigOf(e.tools).Network
	why := "the server was already running"
	if net.Mode == "allowlist" {
		settingsPath, controlPath := e.srtPaths()
		run := srt.NewRun(srt.RunOptions{Domains: e.staticDomains(), SettingsPath: settingsPath,
			Session: e.sname, Quiet: net.QuietDomains, Label: "herdr", ControlPath: controlPath})
		base := readSettings(settingsPath)
		stale := "no settings file of the running server"
		if base != nil {
			stale = srt.PushLine(controlPath, nil)
		}
		if stale == "" {
			run.Base = base
			return true, e.attached(run, "")
		}
		if e.srtStatus().Path != "" {
			why = "network settings stale: " + stale
			if !e.busy() {
				message := why + "; restarting the herdr server"
				e.warnf("%s", message)
				e.emit("", "network", "", map[string]any{"restricted": false, "reason": message})
				if err := e.stopServer(ctx); err != nil {
					return false, err
				}
				return false, nil
			}
		}
	}
	return true, e.attached(srt.NewRun(srt.RunOptions{Session: e.sname, Label: "herdr"}), why)
}

func readSettings(path string) map[string]any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var v map[string]any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return v
}

// attached adopts a running server: run watches its srt log (and pushes,
// when it has settings); unrestricted says why its network is not
// restricted, "" when its srt is live.
func (e *Executor) attached(run *srt.Run, unrestricted string) error {
	// Not started here: nothing says it runs under the sandbox.
	e.setSandboxed(nil, "the server was already running")
	home, err := e.homeFor(e.sname)
	if err != nil {
		return err
	}
	run.Watch(e.srtLogPath(home), true)
	e.setRun(run)
	e.setNetwork(unrestricted == "", unrestricted)
	if unrestricted == "" {
		e.NetworkTick() // a domain approved since that server started
	}
	return nil
}

// busy reports a registered session of this state, in this herdr session,
// that is mid-task (its agent working or on a dialog).
func (e *Executor) busy() bool {
	stateDir := e.StateDir()
	if stateDir == "" {
		return false
	}
	for k, v := range e.reg.Load(stateDir) {
		rec, ok := v.(map[string]any)
		if k == WarmKey || !ok || !host.Truthy(rec["terminal"]) {
			continue
		}
		hs, _ := rec["herdr_session"].(string)
		if hs == "" {
			hs = e.sname
		}
		if hs != e.sname {
			continue
		}
		term, _ := rec["terminal"].(string)
		status, err := e.status(context.Background(), term, "")
		if err != nil {
			continue
		}
		if status == "working" || status == "blocked" {
			return true
		}
	}
	return false
}

func (e *Executor) stopServer(ctx context.Context) error {
	if _, _, err := e.call(ctx, callTimeout, "", "server", "stop"); err != nil {
		e.warnf("%s", err)
	}
	deadline := e.now().Add(e.cfg.Seconds("startup_timeout", 90))
	for e.statusRunning(ctx) {
		if !e.now().Before(deadline) {
			return host.Need(host.KindError, "the stale herdr server did not stop")
		}
		if err := e.sleep(ctx, e.pollShort()); err != nil {
			return host.AsNeed(err)
		}
	}
	return nil
}

func (e *Executor) hostSpec(home string) (sandbox.Spec, error) {
	repo := e.repo
	if repo == "" {
		repo, _ = os.Getwd()
	}
	var extra []string
	if sd := e.StateDir(); sd != "" {
		extra = []string{sd}
	}
	spec := sandbox.HostPolicy(e.tools, sandbox.HostInput{Repo: repo, WorktreeBases: isolate.WorktreeBases(repo),
		HerdrHome: home, ExtraWritable: extra}).Spec
	if spec.Refuse {
		return spec, host.Need(host.KindError, spec.Reason)
	}
	return spec, nil
}

// --- network sandbox (srt) ----------------------------------------------------

// sockets are herdr's own two sockets of this session; nothing else is
// reachable.
func (e *Executor) sockets(home string) []string {
	dir := filepath.Join(sandboxRealPath(home), "herdr", "sessions", e.sname)
	return []string{filepath.Join(dir, "herdr.sock"), filepath.Join(dir, "herdr-client.sock")}
}

func sandboxRealPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	abs, _ := filepath.Abs(p)
	return abs
}

func (e *Executor) srtLogPath(home string) string {
	return filepath.Join(home, "herdr", "sessions", e.sname, "srt.log")
}

// srtPaths is (settings file, control FIFO) of this session's srt: in the
// state dir, which no hosted process may write.
func (e *Executor) srtPaths() (string, string) {
	dir := filepath.Join(sandbox.StateDir(), sandbox.SrtSettingsDir)
	return filepath.Join(dir, e.sname+".json"), filepath.Join(dir, e.sname+".ctl")
}

// staticDomains is allowed_domains plus the registry presets of the repo.
func (e *Executor) staticDomains() []string {
	net := sandbox.ConfigOf(e.tools).Network
	domains := append([]string{}, net.AllowedDomains...)
	if net.Ecosystem {
		repo := e.repo
		if repo == "" {
			repo, _ = os.Getwd()
		}
		domains = append(domains, srt.EcosystemDomains(repo)...)
	}
	seen := map[string]bool{}
	out := []string{}
	for _, d := range domains {
		if d != "" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}

// spawnSrt starts the server under sandbox-runtime: srt's own profile
// built from the host policy (never nested in orchestra's), the domain
// allowlist, and the control FIFO on fd 3 for live domain updates. srt's
// debug output goes to the session's srt.log, which NetworkTick tails.
func (e *Executor) spawnSrt(srtPath string, spec sandbox.Spec, home string, env []string, log *os.File) error {
	net := sandbox.ConfigOf(e.tools).Network
	domains := e.staticDomains()
	settings := sandbox.SrtSettings(spec.Policy, e.sockets(home), domains, net.DeniedDomains, true)
	settingsPath, controlPath := e.srtPaths()
	run := srt.NewRun(srt.RunOptions{Settings: settings, Domains: domains, SettingsPath: settingsPath,
		Session: e.sname, Quiet: net.QuietDomains, Label: "herdr", ControlPath: controlPath})
	path, err := run.Prepare()
	if err != nil {
		return err
	}
	logPath := e.srtLogPath(home)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return err
	}
	run.Watch(logPath, true)
	e.setRun(run)
	argv, _ := e.argv("", "server")
	srtLog, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer srtLog.Close()
	cmd := e.command(sandbox.SrtArgv(srtPath, path, argv), env, log, srtLog)
	if e.OnSpawn != nil {
		e.OnSpawn(cmd.Args, cmd.Env)
	}
	if err := run.Spawn(cmd); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	e.setNetwork(true, "")
	return nil
}

// setNetwork remembers whether the server's network is restricted and
// says so (stderr and an event) when it is not.
func (e *Executor) setNetwork(restricted bool, reason string) {
	if restricted {
		e.netRestricted, e.netReason = true, ""
		return
	}
	if reason == "" {
		reason = "unknown"
	}
	e.netRestricted, e.netReason = false, reason
	message := "network not restricted: " + reason
	e.warnf("%s", message)
	e.emit("", "network", "", map[string]any{"restricted": false, "reason": message})
}

// setRun installs the srt run of the server; every use of it goes through
// withRun, so the runner's goroutines never touch it concurrently.
func (e *Executor) setRun(r *srt.Run) {
	e.netMu.Lock()
	e.srtRun = r
	e.netMu.Unlock()
}

func (e *Executor) withRun(fn func(r *srt.Run)) {
	e.netMu.Lock()
	defer e.netMu.Unlock()
	if e.srtRun != nil {
		fn(e.srtRun)
	}
}

func (e *Executor) closeControl() { e.withRun(func(r *srt.Run) { r.Close() }) }

// NetworkTick tails srt's log for blocked connections and pushes newly
// approved domains to the running server. Never fails.
func (e *Executor) NetworkTick() { e.withRun(func(r *srt.Run) { r.Tick() }) }

// NetworkDenials is the blocked connections since mark as "host:port"
// (bracketed IPv6) or "host", and the current mark.
func (e *Executor) NetworkDenials(mark int) ([]string, int) {
	out := []string{}
	for _, d := range e.denials(mark) {
		out = append(out, formatDenial(d))
	}
	next := 0
	e.withRun(func(r *srt.Run) { next = r.Mark() })
	return out, next
}

func formatDenial(d srt.NetDenial) string {
	if d.Port == nil {
		return d.Host
	}
	h := d.Host
	if containsColon(h) {
		h = "[" + h + "]"
	}
	return fmt.Sprintf("%s:%d", h, *d.Port)
}

func containsColon(s string) bool {
	for _, c := range s {
		if c == ':' {
			return true
		}
	}
	return false
}

func (e *Executor) denials(mark int) []srt.NetDenial {
	quiet := sandbox.ConfigOf(e.tools).Network.QuietDomains
	if quiet == nil {
		quiet = []string{}
	}
	var out []srt.NetDenial
	e.withRun(func(r *srt.Run) { out = r.Denials(mark, quiet) })
	return out
}

// denialValues is the network_denials list as results carry it.
func denialValues(ds []srt.NetDenial) []any {
	out := make([]any, 0, len(ds))
	for _, d := range ds {
		var port any
		if d.Port != nil {
			port = *d.Port
		}
		out = append(out, map[string]any{"host": d.Host, "port": port})
	}
	return out
}

// setSandboxed remembers whether the server confines its panes and says
// so once when it does not.
func (e *Executor) setSandboxed(spec *sandbox.Spec, reason string) {
	if spec != nil && spec.Enforced && spec.Policy != nil {
		e.sandboxed, e.unsandboxed = true, ""
		return
	}
	e.sandboxed = false
	if reason == "" && spec != nil {
		reason = spec.Reason
	}
	e.unsandboxed = reason
	fmt.Fprintf(e.Stderr, "agent-exec: herdr not sandboxed: %s\n", reason)
}

// Shutdown stops the server once no workspace is left open in it, whoever
// started it (a run that attached to an earlier run's server stops it too).
// With keep_sessions a registered session left to resume keeps it; without,
// this run's own package agents are closed first. Warm worktrees are
// removed either way; warm records alone never keep the server, and a run
// that never opened an agent still stops an idle server it finds running.
func (e *Executor) Shutdown() {
	e.warmCleanup()
	e.mu.Lock()
	ready := e.serverReady
	e.mu.Unlock()
	if !ready && !e.statusRunning(context.Background()) {
		return
	}
	if sd := e.StateDir(); sd != "" {
		if e.cfg.Truthy("keep_sessions") {
			if len(e.packageSessions(sd)) > 0 {
				return
			}
		} else {
			e.closeOwn(sd)
		}
	}
	if open, err := e.openWorkspaces(); err != nil || len(open) > 0 {
		return // another run's package is still open in it, or nobody can tell
	}
	e.mu.Lock()
	e.serverReady = false
	e.mu.Unlock()
	if _, _, err := e.call(context.Background(), callTimeout, "", "server", "stop"); err != nil {
		e.warnf("%s", err)
	}
	e.closeControl()
}

// closeOwn closes the agents of the packages of stateDir registered in this
// herdr session (nothing resumes them without keep_sessions).
func (e *Executor) closeOwn(stateDir string) {
	for pkg, v := range e.packageSessions(stateDir) {
		rec, ok := v.(map[string]any)
		if !ok {
			continue
		}
		s := host.SessionFromRecord(e.Name(), rec)
		if hs := s.Str("herdr_session"); hs != "" && hs != e.sname {
			continue
		}
		s.Pkg = pkg
		s.Set("state_dir", stateDir)
		e.closeAgent(s)
	}
}

// openWorkspaces is the workspace ids open in the session's server.
func (e *Executor) openWorkspaces() ([]string, error) {
	payload, _, err := e.call(context.Background(), statusTimeout, "", "workspace", "list")
	if err != nil {
		return nil, err
	}
	list, ok := Find(payload, "workspaces").([]any)
	if !ok {
		return nil, &CallError{Msg: "herdr workspace list returned no workspaces"}
	}
	ids := make([]string, 0, len(list))
	for _, w := range list {
		id, _ := Find(w, "workspace_id").(string)
		ids = append(ids, id)
	}
	return ids, nil
}
