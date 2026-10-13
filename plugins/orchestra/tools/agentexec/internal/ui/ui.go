// Package ui is `agent-exec ui`: a zero-token local dashboard for orchestra.
//
// `agent-exec ui` starts (or reuses) a detached HTTP server bound to
// 127.0.0.1 that renders every registered `agent-exec wave` run, the
// worktrees, recent dispatches and cooldowns from files already on disk.
// Nothing here calls a model; the page is one self-contained HTML document
// fed by Server-Sent Events.
//
// Security: every request needs a random token (query `t` or the
// X-Orchestra-Token header) and a loopback Host header. The mutating routes
// (POST) need the token in the header, never the query.
package ui

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// IdleMinutesDefault is how long an unused dashboard stays up.
const IdleMinutesDefault = 30.0

const startWait = 10 * time.Second

// Info is a running dashboard: its URL (carrying the token), its pid and
// whether this call reused it.
type Info struct {
	URL    string `json:"url"`
	PID    int    `json:"pid"`
	Reused bool   `json:"reused"`
}

// Executable is the binary a dashboard server is started from (tests).
var Executable = os.Executable

// InfoPath is where the running server records itself (ui.json).
func InfoPath() string { return paths.UIStatePath() }

// LogPath is the server's log file.
func LogPath() string {
	if os.Getenv("ORCHESTRA_UI_STATE") != "" {
		return filepath.Join(filepath.Dir(InfoPath()), "ui.log")
	}
	return filepath.Join(paths.OrchestraHome(), "ui.log")
}

// --- server side ----------------------------------------------------------------

func writeInfo(path string, info *jsonx.Object) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := jsonx.Marshal(info)
	if err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(path, data, 0o600)
}

func removeInfoIfOurs(path string) {
	if info := readInfo(path); info != nil && info.pid == os.Getpid() {
		_ = os.Remove(path)
	}
}

// Serve runs the dashboard in the foreground until it has been idle for
// idleMinutes (or SIGTERM/SIGINT); port 0 picks a free port. It records
// itself in ui.json for the client side.
func Serve(port, idleMinutes int) error {
	return ServeIdle(port, float64(idleMinutes))
}

// ServeIdle is Serve with a fractional idle time.
func ServeIdle(port int, idleMinutes float64) error {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	token := hex.EncodeToString(raw)
	idle := time.Duration(idleMinutes * 60 * float64(time.Second))
	server, err := NewServer(port, token, idle)
	if err != nil {
		return err
	}
	path := InfoPath()
	info := jsonx.NewObject().Set("pid", os.Getpid()).Set("port", server.Port()).
		Set("token", token).Set("started", float64(time.Now().UnixMicro())/1e6)
	if err := writeInfo(path, info); err != nil {
		return err
	}
	defer removeInfoIfOurs(path)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sig)
	go func() {
		if _, ok := <-sig; ok {
			server.Shutdown()
		}
	}()
	return server.Run()
}

// --- client side ----------------------------------------------------------------

type liveInfo struct {
	pid, port int
	token     string
}

func intValue(v any) (int, bool) {
	n, ok := v.(int64)
	return int(n), ok
}

func readInfo(path string) *liveInfo {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	doc, err := jsonx.Decode(raw)
	if err != nil {
		return nil
	}
	obj, ok := doc.(*jsonx.Object)
	if !ok {
		return nil
	}
	pid, okPID := intValue(get(obj, "pid"))
	port, okPort := intValue(get(obj, "port"))
	token, okTok := get(obj, "token").(string)
	if _, has := obj.Get("pid"); !has {
		return nil
	}
	if _, has := obj.Get("port"); !has {
		return nil
	}
	if _, has := obj.Get("token"); !has {
		return nil
	}
	if !okPID {
		pid = -1 // present but not an int: not alive
	}
	if !okPort || !okTok {
		return nil
	}
	return &liveInfo{pid: pid, port: port, token: token}
}

func (i *liveInfo) url() string {
	return fmt.Sprintf("http://127.0.0.1:%d/?t=%s", i.port, i.token)
}

func (i *liveInfo) healthy() bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz?t=%s", i.port, i.token))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func liveServer() *liveInfo {
	info := readInfo(InfoPath())
	if info != nil && fsutil.PIDAlive(info.pid) && info.healthy() {
		return info
	}
	return nil
}

// StartOrReuse starts the dashboard if none is up and returns its live
// connection info; open also opens the URL in the browser. port 0 picks a
// free port.
func StartOrReuse(open bool, port, idleMinutes int) (Info, error) {
	return StartOrReuseIdle(open, port, float64(idleMinutes))
}

// StartOrReuseIdle is StartOrReuse with a fractional idle time.
func StartOrReuseIdle(open bool, port int, idleMinutes float64) (Info, error) {
	live := liveServer()
	reused := live != nil
	if live == nil {
		if err := spawnServer(port, idleMinutes); err != nil {
			return Info{}, fmt.Errorf("server did not start (see %s)", LogPath())
		}
		deadline := time.Now().Add(startWait)
		for time.Now().Before(deadline) {
			if live = liveServer(); live != nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	if live == nil {
		return Info{}, fmt.Errorf("server did not start (see %s)", LogPath())
	}
	info := Info{URL: live.url(), PID: live.pid, Reused: reused}
	if open {
		OpenURL(info.URL)
	}
	return info, nil
}

func spawnServer(port int, idleMinutes float64) error {
	exe, err := Executable()
	if err != nil {
		return err
	}
	logPath := LogPath()
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer devnull.Close()
	cmd := exec.Command(exe, "ui", "--serve", "--port", strconv.Itoa(port),
		"--idle-minutes", strconv.FormatFloat(idleMinutes, 'f', -1, 64))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, logFile, logFile
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }() // reap if it exits while we are still here
	return nil
}

// OpenURL asks the desktop to open url; a failure is reported on stderr and
// otherwise ignored.
func OpenURL(url string) {
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	cmd := exec.Command(opener, url)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "agent-exec ui: could not run %s: %v\n", opener, err)
		return
	}
	go func() { _ = cmd.Wait() }()
}

// Stop ends the running dashboard, if any, and removes its record. The
// result is {"stopped": bool}.
func Stop() (map[string]any, error) {
	path := InfoPath()
	info := readInfo(path)
	stopped := false
	if info != nil && fsutil.PIDAlive(info.pid) {
		if err := syscall.Kill(info.pid, syscall.SIGTERM); err == nil {
			stopped = true
		}
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) && fsutil.PIDAlive(info.pid) {
			time.Sleep(50 * time.Millisecond)
		}
	}
	_ = os.Remove(path)
	return map[string]any{"stopped": stopped}, nil
}
