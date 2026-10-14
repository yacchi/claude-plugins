package ui

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/cooldown"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/state"
)

//go:embed assets/page.html
var pageHTML string

const (
	maxFileBytes = 1024 * 1024
	maxPostBytes = 65536
	tokenHeader  = "X-Orchestra-Token"
	stopReason   = "stopped from ui"
	idleCheckMax = time.Second
	idleCheckMin = 20 * time.Millisecond
)

// SSEPoll and SSEPing are how often a stream rebuilds the snapshot and how
// long it may stay silent before a keep-alive comment (variables for tests).
var (
	SSEPoll = 2 * time.Second
	SSEPing = 15 * time.Second
)

// Server is the dashboard's HTTP server: loopback only, every route behind a
// random token.
type Server struct {
	token string
	idle  time.Duration
	ln    net.Listener
	http  *http.Server

	mu          sync.Mutex
	lastActive  time.Time
	openStreams int
}

// NewServer listens on 127.0.0.1:port (0: any free port).
func NewServer(port int, token string, idle time.Duration) (*Server, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	s := &Server{token: token, idle: idle, ln: ln, lastActive: time.Now()}
	s.http = &http.Server{Handler: s, ReadHeaderTimeout: 30 * time.Second}
	return s, nil
}

// Port is the port the server listens on.
func (s *Server) Port() int { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *Server) touch() {
	s.mu.Lock()
	s.lastActive = time.Now()
	s.mu.Unlock()
}

func (s *Server) streamDelta(delta int) {
	s.mu.Lock()
	s.openStreams += delta
	s.mu.Unlock()
	s.touch()
}

func (s *Server) idleExpired() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.openStreams == 0 && time.Since(s.lastActive) >= s.idle
}

// Run serves until Shutdown or until the server has been idle (no request and
// no open stream) for its idle time.
func (s *Server) Run() error {
	done := make(chan struct{})
	defer close(done)
	interval := min(idleCheckMax, max(idleCheckMin, s.idle/4))
	go func() {
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				if s.idleExpired() {
					s.Shutdown()
					return
				}
			}
		}
	}()
	err := s.http.Serve(s.ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown stops the server, ending open streams.
func (s *Server) Shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.http.Shutdown(ctx)
	_ = s.http.Close()
}

// --- request handling -----------------------------------------------------------

func (s *Server) hostOK(r *http.Request) bool {
	host := strings.ToLower(strings.TrimSpace(r.Host))
	port := strconv.Itoa(s.Port())
	return host == "127.0.0.1:"+port || host == "localhost:"+port
}

func (s *Server) authorized(r *http.Request, headerOnly bool) bool {
	supplied := r.Header.Get(tokenHeader)
	if supplied == "" && !headerOnly {
		if values := r.URL.Query()["t"]; len(values) > 0 {
			supplied = values[0]
		}
	}
	return subtle.ConstantTimeCompare([]byte(supplied), []byte(s.token)) == 1
}

func send(w http.ResponseWriter, code int, body []byte, contentType string) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

const (
	textType = "text/plain; charset=utf-8"
	jsonType = "application/json; charset=utf-8"
	htmlType = "text/html; charset=utf-8"
)

func sendText(w http.ResponseWriter, code int, body string) { send(w, code, []byte(body), textType) }

func sendJSON(w http.ResponseWriter, code int, v any) {
	send(w, code, []byte(Body(v)), jsonType)
}

func (s *Server) gate(w http.ResponseWriter, r *http.Request, headerOnly bool) bool {
	s.touch()
	if !s.hostOK(r) || !s.authorized(r, headerOnly) {
		sendText(w, http.StatusForbidden, "forbidden")
		return false
	}
	return true
}

// ServeHTTP routes a request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.doGet(w, r)
	case http.MethodPost:
		s.doPost(w, r)
	default:
		sendText(w, http.StatusNotImplemented, "unsupported method")
	}
}

func snapshotNow() map[string]any {
	cfg, err := config.Resolve()
	return buildSnapshot(cfg, err)
}

func (s *Server) doGet(w http.ResponseWriter, r *http.Request) {
	if !s.gate(w, r, false) {
		return
	}
	switch r.URL.Path {
	case "/healthz":
		sendText(w, http.StatusOK, "ok")
	case "/":
		send(w, http.StatusOK, []byte(pageHTML), htmlType)
	case "/api/snapshot":
		sendJSON(w, http.StatusOK, snapshotNow())
	case "/api/stream":
		s.stream(w, r)
	case "/api/file":
		s.file(w, r.URL.Query().Get("path"))
	default:
		sendText(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) file(w http.ResponseWriter, requested string) {
	allowed, ok := allowedFile(requested)
	if !ok {
		sendText(w, http.StatusNotFound, "not found")
		return
	}
	info, err := os.Stat(allowed)
	if err != nil {
		sendText(w, http.StatusNotFound, "not found")
		return
	}
	if info.Size() > maxFileBytes {
		sendText(w, http.StatusRequestEntityTooLarge, "file too large")
		return
	}
	data, err := os.ReadFile(allowed)
	if err != nil {
		sendText(w, http.StatusNotFound, "not found")
		return
	}
	send(w, http.StatusOK, data, textType)
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		sendText(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "close")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	s.streamDelta(1)
	defer s.streamDelta(-1)
	lastHash := ""
	lastPing := time.Now()
	for {
		snap := snapshotNow()
		digest := snapshotHash(snap)
		if digest != lastHash {
			lastHash = digest
			if _, err := fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", Body(snap)); err != nil {
				return
			}
			flusher.Flush()
			lastPing = time.Now()
		} else if time.Since(lastPing) >= SSEPing {
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
			lastPing = time.Now()
		}
		s.touch()
		select {
		case <-r.Context().Done():
			return
		case <-time.After(SSEPoll):
		}
	}
}

func (s *Server) doPost(w http.ResponseWriter, r *http.Request) {
	if !s.gate(w, r, true) {
		return
	}
	route := r.URL.Path
	if route != "/api/stop" && route != "/api/cooldown/clear" && route != "/api/sweep" {
		sendText(w, http.StatusNotFound, "not found")
		return
	}
	length, _ := strconv.Atoi(strings.TrimSpace(r.Header.Get("Content-Length")))
	body, _ := io.ReadAll(io.LimitReader(r.Body, int64(min(max(length, 0), maxPostBytes))))
	var payload any = map[string]any{}
	if text := strings.TrimSpace(string(body)); text != "" {
		if err := json.Unmarshal([]byte(text), &payload); err != nil {
			sendJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body"})
			return
		}
	}
	obj, _ := payload.(map[string]any)
	switch route {
	case "/api/cooldown/clear":
		s.clearCooldown(w, obj)
	case "/api/sweep":
		s.sweep(w, obj)
	default:
		s.stop(w, obj)
	}
}

func (s *Server) clearCooldown(w http.ResponseWriter, payload map[string]any) {
	cfg, err := config.Resolve()
	if err != nil {
		sendJSON(w, http.StatusInternalServerError, errorBody(err))
		return
	}
	executor, given := payload["executor"]
	if !given || executor == nil {
		if _, err := cooldown.Clear(cfg, ""); err != nil {
			sendJSON(w, http.StatusInternalServerError, errorBody(err))
			return
		}
		sendJSON(w, http.StatusOK, map[string]any{"cleared": "all"})
		return
	}
	name, isString := executor.(string)
	if !isString || name == "" {
		sendJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("unknown executor: %v", executor)})
		return
	}
	if _, err := cooldown.Clear(cfg, name); err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	sendJSON(w, http.StatusOK, map[string]any{"cleared": name})
}

func (s *Server) sweep(w http.ResponseWriter, payload map[string]any) {
	repo, _ := payload["repo"].(string)
	known := false
	if repo != "" {
		want := paths.RealPath(repo)
		for _, e := range state.ListWaves() {
			if e.Repo != "" && paths.RealPath(e.Repo) == want {
				known = true
			}
		}
	}
	if !known {
		sendJSON(w, http.StatusNotFound, map[string]any{"error": "unknown repo"})
		return
	}
	apply := false
	switch v := payload["apply"].(type) {
	case bool:
		apply = v
	case string:
		apply = v != ""
	case float64:
		apply = v != 0
	case []any:
		apply = len(v) > 0
	case map[string]any:
		apply = len(v) > 0
	}
	result, err := isolate.Sweep(repo, isolate.SweepOptions{
		DryRun: !apply, LiveWindow: isolate.DefaultLiveWindow,
	})
	if err != nil {
		sendJSON(w, http.StatusInternalServerError, errorBody(err))
		return
	}
	sendJSON(w, http.StatusOK, plain(result))
}

func (s *Server) stop(w http.ResponseWriter, payload map[string]any) {
	target, _ := payload["state"].(string)
	known := false
	for _, e := range state.ListWaves() {
		if e.State == target && target != "" {
			known = true
		}
	}
	if !known {
		sendJSON(w, http.StatusNotFound, map[string]any{"error": "unknown state"})
		return
	}
	if err := state.RequestStop(target, stopReason); err != nil {
		sendJSON(w, http.StatusInternalServerError, errorBody(err))
		return
	}
	sendJSON(w, http.StatusOK, map[string]any{"stop_requested": true})
}

// --- /api/file allow-list ----------------------------------------------------------

func expandUser(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + p[1:]
		}
	}
	return p
}

// allowedFile is path when it is a plan's spec or preamble, or a file
// directly inside a registered wave's context, corrections or carry
// directory; anything else (symlink escapes, relative or unnormalized
// spellings, directories) is refused.
func allowedFile(path string) (string, bool) {
	candidate, err := filepath.Abs(expandUser(path))
	if err != nil || candidate != path || !isFile(candidate) {
		return "", false
	}
	real := paths.RealPath(candidate)
	for _, e := range state.ListWaves() {
		st, err := loadJSONObject(e.State)
		if err != nil {
			continue
		}
		plan, ok := get(st, "plan").(string)
		if !ok {
			continue
		}
		data, err := loadJSONObject(plan)
		if err != nil {
			continue
		}
		planDir := filepath.Dir(plan)
		resolve := func(v string) string {
			return paths.RealPath(pyJoinAbs(planDir, v))
		}
		var allowed []string
		switch pre := get(data, "preamble").(type) {
		case string:
			allowed = append(allowed, resolve(pre))
		case []any:
			for _, v := range pre {
				if s, ok := v.(string); ok {
					allowed = append(allowed, resolve(s))
				}
			}
		}
		if s, ok := get(data, "spec").(string); ok {
			allowed = append(allowed, resolve(s))
		}
		if pkgs, ok := get(data, "packages").([]any); ok {
			for _, p := range pkgs {
				if spec, ok := get(asObject(p), "spec").(string); ok {
					allowed = append(allowed, resolve(spec))
				}
			}
		}
		stateDir := filepath.Dir(pyJoinAbs("", e.State))
		for _, dirname := range []string{"context", "corrections", "carry"} {
			if filepath.Dir(real) == paths.RealPath(filepath.Join(stateDir, dirname)) {
				allowed = append(allowed, real)
			}
		}
		for _, a := range allowed {
			if a == real {
				return candidate, true
			}
		}
	}
	return "", false
}

func asObject(v any) map[string]any {
	o, _ := v.(map[string]any)
	return o
}
