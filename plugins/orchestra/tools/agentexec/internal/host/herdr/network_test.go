package herdr

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/sandbox"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/srt"
)

// netEnv runs the server under a stand-in srt (a script that runs the
// command it is given) with the host sandboxed by the real backend.
type netEnv struct {
	*env
	srt     string
	readers []int
	bufMu   sync.Mutex
	buf     []byte
}

func newNetEnv(t *testing.T) *netEnv {
	if p := sandbox.Probes(); !p.Seatbelt.OK && !p.Bwrap.OK {
		t.Skip("no sandbox backend for the herdr host here")
	}
	e := &netEnv{env: newEnv(t)}
	e.srt = filepath.Join(e.tmp, "srt")
	_ = os.WriteFile(e.srt, []byte("#!/bin/sh\nexec /bin/sh -c \"$7\"\n"), 0o755)
	t.Cleanup(func() {
		for _, fd := range e.readers {
			syscall.Close(fd)
		}
	})
	return e
}

const netCfg = `{"sandbox": {"mode": "auto"}}`

func (e *netEnv) executor(cfg string, srtPath string) *Executor {
	if cfg == "" {
		cfg = netCfg
	}
	x := e.env.executor(cfg)
	x.SrtStatus = func() srt.StatusInfo {
		if srtPath == "" {
			return srt.StatusInfo{Reason: "srt not available"}
		}
		return srt.StatusInfo{Path: srtPath}
	}
	return x
}

func (e *netEnv) srtDir() string {
	return filepath.Join(sandbox.StateDir(), sandbox.SrtSettingsDir)
}

func (e *netEnv) settings() (string, map[string]any) {
	path := filepath.Join(e.srtDir(), session+".json")
	var v map[string]any
	b, _ := os.ReadFile(path)
	_ = json.Unmarshal(b, &v)
	return path, v
}

// openReader stands in for srt holding the control FIFO: it drains the
// FIFO all the time (a settings line is larger than a pipe buffer).
func (e *netEnv) openReader() {
	fd, err := syscall.Open(filepath.Join(e.srtDir(), session+".ctl"), syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	e.readers = append(e.readers, fd)
	done := make(chan struct{})
	e.t.Cleanup(func() { close(done) })
	go func() {
		buf := make([]byte, 1<<16)
		for {
			select {
			case <-done:
				return
			default:
			}
			n, err := syscall.Read(fd, buf)
			if n > 0 {
				e.bufMu.Lock()
				e.buf = append(e.buf, buf[:n]...)
				e.bufMu.Unlock()
				continue
			}
			if err != nil && err != syscall.EAGAIN && err != syscall.EINTR {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
}

// lines are the complete lines srt received since the last call.
func (e *netEnv) lines() []map[string]any {
	time.Sleep(300 * time.Millisecond)
	e.bufMu.Lock()
	defer e.bufMu.Unlock()
	end := strings.LastIndexByte(string(e.buf), '\n')
	if end < 0 {
		return nil
	}
	data := string(e.buf[:end])
	e.buf = e.buf[end+1:]
	var out []map[string]any
	for _, l := range strings.Split(data, "\n") {
		var v map[string]any
		_ = json.Unmarshal([]byte(l), &v)
		out = append(out, v)
	}
	return out
}

func (e *netEnv) srtLog(lines ...string) {
	path := filepath.Join(e.home, "herdr", "sessions", session, "srt.log")
	f, _ := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	defer f.Close()
	for _, l := range lines {
		f.WriteString(l + "\n")
	}
}

func allowed(v map[string]any) any {
	return v["network"].(map[string]any)["allowedDomains"]
}

func TestServerRunsUnderSrtAloneWithTheHostPolicy(t *testing.T) {
	e := newNetEnv(t)
	e.scenario(map[string]any{}, true)
	x := e.executor("", e.srt)
	e.mustStart(x, "a")
	path, settings := e.settings()
	argv := e.spawns[0]
	if !reflect.DeepEqual(argv[:7], []string{e.srt, "-d", "-s", path, "--control-fd", "3", "-c"}) ||
		argv[7] != fakeBin+" --session "+session+" server" {
		t.Fatalf("argv %q", argv)
	}
	for _, a := range argv {
		if a == "/usr/bin/sandbox-exec" {
			t.Error("srt nested in seatbelt")
		}
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Error(st.Mode())
	}
	if st, _ := os.Stat(filepath.Dir(path)); st.Mode().Perm() != 0o700 {
		t.Error(st.Mode())
	}
	net := settings["network"].(map[string]any)
	sessions := filepath.Join(sandboxRealPath(e.home), "herdr", "sessions", session)
	if !reflect.DeepEqual(net["allowUnixSockets"], []any{filepath.Join(sessions, "herdr.sock"), filepath.Join(sessions, "herdr-client.sock")}) ||
		net["allowAllUnixSockets"] != false || !reflect.DeepEqual(net["allowedDomains"], []any{"api.anthropic.com"}) ||
		settings["allowPty"] != true {
		t.Errorf("network %v", net)
	}
	allow := settings["filesystem"].(map[string]any)["allowWrite"].([]any)
	for _, a := range allow {
		if a == e.repo {
			t.Error("the user's tree is writable")
		}
	}
	if !x.sandboxed || !x.netRestricted {
		t.Error("not restricted")
	}
	if e.spawnEnvs[0] == nil || !hasArg(e.spawnEnvs[0], "XDG_CONFIG_HOME="+e.home) {
		t.Error("server env")
	}
}

func TestSrtIsNotUsedWhenNetworkModeIsOffOrSrtIsMissing(t *testing.T) {
	for _, c := range []struct{ label, cfg, srt, why string }{
		{"mode off", `{"sandbox": {"mode": "auto", "network": {"mode": "off"}}}`, "srt", "sandbox.network.mode is off"},
		{"srt missing", "", "", "srt not available"},
	} {
		t.Run(c.label, func(t *testing.T) {
			e := newNetEnv(t)
			e.scenario(map[string]any{}, true)
			srtPath := ""
			if c.srt != "" {
				srtPath = e.srt
			}
			x := e.executor(c.cfg, srtPath)
			e.mustStart(x, "a")
			if hasArg(e.spawns[0], e.srt) || !x.sandboxed || x.netRestricted {
				t.Errorf("argv %v", e.spawns[0])
			}
			found := false
			for _, ev := range e.events {
				if ev["action"] == "network" && ev["restricted"] == false && ev["reason"] == "network not restricted: "+c.why && ev["terminal"] == nil {
					found = true
				}
			}
			if !found {
				t.Errorf("events %v", e.events)
			}
		})
	}
}

func TestBlockedConnectionsAreAuditedAndSurfacedOncePerHost(t *testing.T) {
	e := newNetEnv(t)
	e.scenario(map[string]any{"prompt_result": map[string]any{"status": "ok", "summary": "done"}}, true)
	x := e.executor("", e.srt)
	s := e.mustStart(x, "a")
	e.srtLog("[SandboxDebug] No matching config rule, denying: example.com:443",
		"[SandboxDebug] Connection blocked to example.com:443",
		"[SandboxDebug] Connection blocked to example.com:443",
		"[SandboxDebug] Connection blocked to mcp-proxy.anthropic.com:443",
		"[SandboxDebug] Connection blocked to [2001:db8::1]:8443")
	out, err := host.PromptAndWait(context.Background(), x, s, []string{"/p/one.md"}, e.result, 60e9)
	if err != nil {
		t.Fatal(err)
	}
	want := []any{map[string]any{"host": "example.com", "port": 443}, map[string]any{"host": "2001:db8::1", "port": 8443}}
	if !reflect.DeepEqual(out["network_denials"], want) {
		t.Errorf("denials %v", out["network_denials"])
	}
	audit := filepath.Join(sandbox.StateDir(), "audit", "network.jsonl")
	b, _ := os.ReadFile(audit)
	if n := strings.Count(string(b), "\n"); n != 4 || !strings.Contains(string(b), session) {
		t.Errorf("audit (%d lines): %s", n, b)
	}
	if got, mark := x.NetworkDenials(0); !reflect.DeepEqual(got, []string{"example.com:443", "[2001:db8::1]:8443"}) || mark != 4 {
		t.Errorf("NetworkDenials %v %d", got, mark)
	}
}

func TestAnApprovedDomainReachesTheRunningServerAsOneFullLine(t *testing.T) {
	e := newNetEnv(t)
	e.scenario(map[string]any{}, true)
	x := e.executor("", e.srt)
	e.mustStart(x, "a")
	e.openReader()
	_, initial := e.settings()
	x.NetworkTick()
	if l := e.lines(); len(l) != 0 {
		t.Fatalf("nothing changed, yet %v", l)
	}
	if err := sandbox.AddDomain("registry.npmjs.org"); err != nil {
		t.Fatal(err)
	}
	x.NetworkTick()
	lines := e.lines()
	initial["network"].(map[string]any)["allowedDomains"] = []any{"api.anthropic.com", "registry.npmjs.org"}
	if len(lines) != 1 || !reflect.DeepEqual(lines[0], initial) {
		t.Fatalf("lines %v", lines)
	}
	x.NetworkTick()
	if l := e.lines(); len(l) != 0 {
		t.Error("pushed twice")
	}
	_ = sandbox.ForgetDomain("registry.npmjs.org")
	x.NetworkTick()
	if l := e.lines(); len(l) != 1 || !reflect.DeepEqual(allowed(l[0]), []any{"api.anthropic.com"}) {
		t.Errorf("forget %v", l)
	}
}

func TestALaterRunPushesToTheServerAnEarlierRunLeftRunning(t *testing.T) {
	e := newNetEnv(t)
	e.scenario(map[string]any{}, true)
	first := e.executor("", e.srt)
	s := e.mustStart(first, "a")
	e.openReader()
	_ = first.Close(s, true)
	_, initial := e.settings()
	_ = sandbox.AddDomain("registry.npmjs.org")
	spawned := len(e.spawns)
	later := e.executor("", e.srt)
	e.mustStart(later, "a")
	if len(e.spawns) != spawned || e.has("server stop") || !later.netRestricted {
		t.Fatalf("spawns %d stop %v restricted %v", len(e.spawns)-spawned, e.has("server stop"), later.netRestricted)
	}
	initial["network"].(map[string]any)["allowedDomains"] = []any{"api.anthropic.com", "registry.npmjs.org"}
	if l := e.lines(); len(l) != 1 || !reflect.DeepEqual(l[0], initial) {
		t.Errorf("lines %v", l)
	}
	if _, onDisk := e.settings(); !reflect.DeepEqual(onDisk, initial) {
		t.Error("settings file not rewritten")
	}
	later.NetworkTick()
	if l := e.lines(); len(l) != 0 {
		t.Error("unchanged settings pushed again")
	}
}

func TestAServerWithoutALiveControlFIFOIsRestartedUnlessBusy(t *testing.T) {
	for _, c := range []struct {
		label     string
		status    string
		restarted bool
	}{{"idle", "idle", true}, {"mid-task", "working", false}} {
		t.Run(c.label, func(t *testing.T) {
			e := newNetEnv(t)
			e.scenario(map[string]any{"running": true, "statuses": []any{c.status}}, true)
			reg := map[string]any{"b": map[string]any{"pkg": "b", "worktree": e.tmp, "terminal": "p-b", "herdr_session": session}}
			_ = host.WriteRegistryFile(filepath.Join(e.state, RegistryName), reg)
			_ = os.MkdirAll(e.srtDir(), 0o700)
			_ = os.WriteFile(filepath.Join(e.srtDir(), session+".json"), []byte(`{"network": {"allowedDomains": []}}`), 0o600)
			_ = syscall.Mkfifo(filepath.Join(e.srtDir(), session+".ctl"), 0o600)
			x := e.executor("", e.srt)
			x.SetStateDir(e.state)
			e.mustStart(x, "a")
			if e.has("server stop") != c.restarted || (len(e.spawns) == 1) != c.restarted {
				t.Errorf("stop %v spawns %d", e.has("server stop"), len(e.spawns))
			}
			var reasons []string
			for _, ev := range e.events {
				if ev["action"] == "network" {
					reasons = append(reasons, ev["reason"].(string))
				}
			}
			if len(reasons) == 0 || !strings.Contains(reasons[0], "network settings stale") {
				t.Errorf("reasons %v", reasons)
			}
			if x.netRestricted != c.restarted {
				t.Errorf("restricted %v", x.netRestricted)
			}
		})
	}
}

func TestBypassNeedsSrtWithTheAllowlistAndTheHostSandbox(t *testing.T) {
	for _, c := range []struct{ label, cfg, want string }{
		{"permission_mode", `{"sandbox": {"mode": "auto"}, "herdr": {"permission_mode": "bypassPermissions"}}`, "bypassPermissions"},
		{"args", `{"sandbox": {"mode": "auto"}, "herdr": {"args": "--model {model} --dangerously-skip-permissions"}}`, "--dangerously-skip-permissions"},
	} {
		t.Run(c.label, func(t *testing.T) {
			e := newNetEnv(t)
			e.scenario(map[string]any{}, true)
			e.mustStart(e.executor(c.cfg, e.srt), "a")
			if !hasArg(e.starts()[0], c.want) {
				t.Error(e.starts())
			}
		})
	}
	bypass := `{"sandbox": {"mode": "auto"%s}, "herdr": {"permission_mode": "bypassPermissions"}}`
	for _, c := range []struct {
		label, net, srt, needle string
		running, force          bool
	}{
		{"srt missing", "", "", "network not restricted", false, false},
		{"network off", `, "network": {"mode": "off"}`, "srt", "network not restricted", false, false},
		{"no host sandbox", "", "srt", "host filesystem sandbox does not apply", false, true},
		{"server not ours", "", "srt", "host filesystem sandbox does not apply", true, false},
	} {
		t.Run(c.label, func(t *testing.T) {
			e := newNetEnv(t)
			if c.force {
				t.Setenv(sandbox.ForceEnv, "none")
			}
			e.scenario(map[string]any{"running": c.running}, true)
			if c.running {
				// an earlier run's server with its srt alive behind the FIFO
				_ = os.MkdirAll(e.srtDir(), 0o700)
				_ = os.WriteFile(filepath.Join(e.srtDir(), session+".json"), []byte(`{"network": {"allowedDomains": []}}`), 0o600)
				_ = syscall.Mkfifo(filepath.Join(e.srtDir(), session+".ctl"), 0o600)
				e.openReader()
			}
			srtPath := ""
			if c.srt != "" {
				srtPath = e.srt
			}
			_, err := e.start(e.executor(strings.Replace(bypass, "%s", c.net, 1), srtPath), "a")
			n := needOf(t, err)
			if err := containsAll(n.Detail, "bypass is not allowed", c.needle); err != nil || len(e.starts()) != 0 {
				t.Error(err, e.starts())
			}
		})
	}
}
