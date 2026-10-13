package ui

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/dispatch"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/state"
)

const testToken = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type env struct {
	t        *testing.T
	tmp      string
	home     string
	registry string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	tmp := t.TempDir()
	e := &env{t: t, tmp: tmp, home: filepath.Join(tmp, "home"), registry: filepath.Join(tmp, "waves.jsonl")}
	if err := os.MkdirAll(e.home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", e.home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "xdg-state"))
	t.Setenv("ORCHESTRA_WAVE_REGISTRY", e.registry)
	t.Setenv("ORCHESTRA_UI_STATE", filepath.Join(tmp, "ui.json"))
	t.Setenv("ORCHESTRA_ALIVE_DIR", filepath.Join(tmp, "alive"))
	ResetCaches()
	t.Cleanup(ResetCaches)
	return e
}

func (e *env) write(path, text string) string {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		e.t.Fatal(err)
	}
	return path
}

var testEvents = []map[string]any{
	{"at": 1.0, "pkg": "A-1", "from": nil, "to": nil, "event": "dispatch-start",
		"detail": `{"attempt": 1, "cls": "light", "kind": "implement"}`},
	{"at": 2.0, "pkg": nil, "from": nil, "to": nil, "event": "verify-end", "detail": "not json"},
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (e *env) register(statePath, repo string) {
	e.t.Helper()
	if repo == "" {
		repo = e.tmp
	}
	line := mustJSON(e.t, map[string]any{"state": statePath, "plan": "/x/plan.json", "repo": repo,
		"into": "wave-int", "registered_at": 1.0}) + "\n"
	f, err := os.OpenFile(e.registry, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		e.t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) stateDoc(detail string) map[string]any {
	now := float64(time.Now().Unix())
	return map[string]any{
		"version": 1, "plan": "/x/plan.json",
		"integration": map[string]any{"task": "wave-int", "path": nil, "base": nil},
		"wave":        3,
		"packages": map[string]any{
			"A-1": map[string]any{"status": "implementing", "since": now - 5, "attempts": 1,
				"tree": nil, "executor": "pi", "session": false, "files_changed": 2, "commit": nil, "detail": detail},
			"A-2": map[string]any{"status": "pending", "since": now, "attempts": 0,
				"tree": nil, "executor": nil, "session": false, "files_changed": nil, "commit": nil, "detail": ""},
		},
		"needs":   []any{map[string]any{"id": "A-1", "kind": "escalate", "detail": "line one\nline two", "at": 1.0}},
		"stopped": nil, "updated": now,
	}
}

func (e *env) makeWave(name, detail string, register bool) string {
	e.t.Helper()
	dir := filepath.Join(e.tmp, name)
	statePath := e.write(filepath.Join(dir, "state.json"), mustJSON(e.t, e.stateDoc(detail)))
	var events strings.Builder
	for _, rec := range testEvents {
		events.WriteString(mustJSON(e.t, rec) + "\n")
	}
	e.write(filepath.Join(dir, "state.events.jsonl"), events.String())
	if register {
		e.register(statePath, "")
	}
	return statePath
}

func (e *env) start(idle time.Duration) *Server {
	e.t.Helper()
	s, err := NewServer(0, testToken, idle)
	if err != nil {
		e.t.Fatal(err)
	}
	go func() { _ = s.Run() }()
	e.t.Cleanup(s.Shutdown)
	return s
}

type response struct {
	status int
	body   []byte
	header http.Header
}

func (e *env) request(s *Server, method, path string, headers map[string]string, body, host string) response {
	e.t.Helper()
	req, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", s.Port(), path), strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Host = host
	if host == "" {
		req.Host = fmt.Sprintf("127.0.0.1:%d", s.Port())
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return response{resp.StatusCode, data, resp.Header}
}

func (e *env) get(s *Server, path string) response {
	return e.request(s, "GET", path, nil, "", "")
}

func (e *env) snapshot(s *Server) map[string]any {
	e.t.Helper()
	r := e.get(s, "/api/snapshot?t="+testToken)
	if r.status != 200 {
		e.t.Fatalf("snapshot: %d %s", r.status, r.body)
	}
	var out map[string]any
	if err := json.Unmarshal(r.body, &out); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func TestAuthTokenRequired(t *testing.T) {
	e := newEnv(t)
	s := e.start(time.Minute)
	for path, want := range map[string]int{
		"/healthz":                              403,
		"/healthz?t=" + strings.Repeat("b", 32): 403,
		"/healthz?t=" + testToken:               200,
		"/api/snapshot":                         403,
		"/api/stream":                           403,
		"/api/file?path=/etc/hosts":             403,
		"/":                                     403,
	} {
		if got := e.get(s, path).status; got != want {
			t.Errorf("GET %s = %d, want %d", path, got, want)
		}
	}
	if got := e.request(s, "GET", "/healthz", map[string]string{"X-Orchestra-Token": testToken}, "", "").status; got != 200 {
		t.Errorf("header token = %d", got)
	}
	// An empty token never matches, and the query cannot override a wrong header.
	if got := e.request(s, "GET", "/healthz?t="+testToken, map[string]string{"X-Orchestra-Token": "x"}, "", "").status; got != 403 {
		t.Errorf("wrong header with right query = %d", got)
	}
}

func TestHostHeaderChecked(t *testing.T) {
	e := newEnv(t)
	s := e.start(time.Minute)
	for host, want := range map[string]int{
		fmt.Sprintf("evil.example:%d", s.Port()):   403,
		"127.0.0.1:1":                              403,
		fmt.Sprintf("localhost:%d", s.Port()):      200,
		fmt.Sprintf("LOCALHOST:%d", s.Port()):      200,
		fmt.Sprintf("127.0.0.1.evil:%d", s.Port()): 403,
	} {
		if got := e.request(s, "GET", "/healthz?t="+testToken, nil, "", host).status; got != want {
			t.Errorf("Host %s = %d, want %d", host, got, want)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t)
	s := e.start(time.Minute)
	r := e.get(s, "/healthz?t="+testToken)
	for k, v := range map[string]string{
		"Cache-Control": "no-store", "X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer",
		"Content-Type": "text/plain; charset=utf-8",
	} {
		if got := r.header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestSnapshotShape(t *testing.T) {
	e := newEnv(t)
	statePath := e.makeWave("w1", "", true)
	snap := e.snapshot(e.start(time.Minute))
	for _, key := range []string{"generated_at", "waves", "worktrees", "dispatch", "cooldown", "running", "usage"} {
		if _, ok := snap[key]; !ok {
			t.Errorf("snapshot lacks %s", key)
		}
	}
	waves := snap["waves"].([]any)
	if len(waves) != 1 {
		t.Fatalf("waves = %v", waves)
	}
	w := waves[0].(map[string]any)
	if w["state"] != statePath || w["into"] != "wave-int" || w["wave"] != float64(3) {
		t.Errorf("wave = %v", w)
	}
	if got := mustJSON(t, w["counts"]); got != `{"implementing":1,"pending":1}` {
		t.Errorf("counts = %s", got)
	}
	var a1 map[string]any
	for _, p := range w["packages"].([]any) {
		if p.(map[string]any)["id"] == "A-1" {
			a1 = p.(map[string]any)
		}
	}
	for _, k := range []string{"attempts", "context", "correction", "detail", "executor", "files_changed", "files_live", "id", "since", "spec", "status"} {
		if _, ok := a1[k]; !ok {
			t.Errorf("package lacks %s", k)
		}
	}
	if len(a1) != 11 {
		t.Errorf("package keys = %v", a1)
	}
	needs := w["needs"].([]any)
	if needs[0].(map[string]any)["kind"] != "escalate" {
		t.Errorf("needs = %v", needs)
	}
	events := w["events"].([]any)
	if events[0].(map[string]any)["detail"].(map[string]any)["kind"] != "implement" {
		t.Errorf("event 0 = %v", events[0])
	}
	if events[1].(map[string]any)["detail"] != "not json" {
		t.Errorf("event 1 = %v", events[1])
	}
	wt := snap["worktrees"].([]any)
	if len(wt) != 1 || wt[0].(map[string]any)["repo"] != e.tmp {
		t.Errorf("worktrees = %v", wt)
	}
	if got := mustJSON(t, snap["dispatch"]); got != "[]" {
		t.Errorf("dispatch = %s", got)
	}
	if got := mustJSON(t, snap["cooldown"]); got != "{}" {
		t.Errorf("cooldown = %s", got)
	}
}

func TestRegistryOrderAndDedupe(t *testing.T) {
	e := newEnv(t)
	a := e.makeWave("a", "", false)
	b := e.makeWave("b", "", false)
	e.register(a, "/old")
	e.register(b, "")
	e.register(a, "/new")
	e.register(filepath.Join(e.tmp, "gone", "state.json"), "")
	f, _ := os.OpenFile(e.registry, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString(`{"state": "/trunc`)
	f.Close()
	entries := registry()
	if len(entries) != 2 || entries[0].State != b || entries[1].State != a || entries[1].Repo != "/new" {
		t.Errorf("registry = %+v", entries)
	}
}

func TestDispatchAndCooldownFromHome(t *testing.T) {
	e := newEnv(t)
	e.makeWave("w1", "", true)
	runs := filepath.Join(e.home, ".claude", "orchestra", "runs", "sess")
	e.write(filepath.Join(runs, "r1.jsonl"), mustJSON(t, map[string]any{
		"executor": "pi", "model": "m", "cls": "light", "status": "ok", "paths": []string{"x"}, "duration_s": 1.5})+"\n")
	e.write(filepath.Join(e.home, ".claude", "orchestra", "executor-state.json"),
		mustJSON(t, map[string]any{"pi": map[string]any{"until": 1.0, "reason": "quota"}}))
	snap := e.snapshot(e.start(time.Minute))
	disp, ok := snap["dispatch"].([]any)
	if !ok || len(disp) != 1 {
		t.Fatalf("dispatch = %v", snap["dispatch"])
	}
	rec := disp[0].(map[string]any)
	if rec["executor"] != "pi" || rec["duration_s"] != 1.5 {
		t.Errorf("record = %v", rec)
	}
	if _, has := rec["paths"]; has {
		t.Errorf("paths leaked: %v", rec)
	}
	if got := mustJSON(t, snap["cooldown"]); got != `{"pi":{"reason":"quota","until":1}}` {
		t.Errorf("cooldown = %s", got)
	}
}

func TestFailingSourceBecomesErrorNot500(t *testing.T) {
	e := newEnv(t)
	e.makeWave("w1", "", true)
	e.write(filepath.Join(e.home, ".claude", "orchestra.yaml"), "a: [unclosed\n")
	snap := e.snapshot(e.start(time.Minute))
	for _, key := range []string{"dispatch", "cooldown", "usage"} {
		m, ok := snap[key].(map[string]any)
		if !ok || m["error"] == nil {
			t.Errorf("%s = %v, want an error object", key, snap[key])
		}
	}
	if len(snap["waves"].([]any)) != 1 {
		t.Errorf("waves = %v", snap["waves"])
	}
}

func TestBrokenStateFileIsPerWaveError(t *testing.T) {
	e := newEnv(t)
	statePath := e.makeWave("w1", "", true)
	e.write(statePath, "{broken")
	snap := e.snapshot(e.start(time.Minute))
	w := snap["waves"].([]any)[0].(map[string]any)
	if w["error"] == nil {
		t.Errorf("wave = %v", w)
	}
}

func TestWaveLevelNeedWithNullID(t *testing.T) {
	e := newEnv(t)
	statePath := e.makeWave("w1", "", true)
	doc := e.stateDoc("")
	doc["needs"] = append(doc["needs"].([]any), map[string]any{"id": nil, "kind": "environment", "detail": "ensure failed\nmore", "at": 2.0})
	e.write(statePath, mustJSON(t, doc))
	s := e.start(time.Minute)
	needs := e.snapshot(s)["waves"].([]any)[0].(map[string]any)["needs"].([]any)
	if got := mustJSON(t, needs[1]); got != `{"at":2,"detail":"ensure failed\nmore","id":null,"kind":"environment"}` {
		t.Errorf("need = %s", got)
	}
	page := string(e.get(s, "/?t="+testToken).body)
	if !strings.Contains(page, `n.id === null ? txt("(wave)")`) {
		t.Error("page does not label wave-level needs")
	}
}

func TestPageEscapesViaTextNodes(t *testing.T) {
	e := newEnv(t)
	statePath := e.makeWave("w1", "<script>alert(1)</script>", true)
	s := e.start(time.Minute)
	r := e.get(s, "/?t="+testToken)
	page := string(r.body)
	if r.status != 200 || r.header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("page: %d %s", r.status, r.header.Get("Content-Type"))
	}
	for _, want := range []string{"prefers-color-scheme", "createTextNode"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(page, "innerHTML") {
		t.Error("page uses innerHTML")
	}
	if strings.Contains(strings.ReplaceAll(page, "http://www.w3.org", ""), "http://") {
		t.Error("page references an http:// URL")
	}
	// the payload travels only as JSON data, never inside the HTML document
	if strings.Contains(page, "<script>alert(1)</script>") {
		t.Error("user data is inside the page")
	}
	snap := e.snapshot(s)
	w := snap["waves"].([]any)[0].(map[string]any)
	if w["state"] != statePath {
		t.Errorf("state = %v", w["state"])
	}
	for _, p := range w["packages"].([]any) {
		if p.(map[string]any)["id"] == "A-1" && p.(map[string]any)["detail"] != "<script>alert(1)</script>" {
			t.Errorf("detail = %v", p)
		}
	}
	// JSON is not HTML-escaped (Python's json.dumps does not either).
	if raw := e.get(s, "/api/snapshot?t="+testToken); !strings.Contains(string(raw.body), "<script>alert(1)</script>") {
		t.Error("snapshot body is HTML-escaped")
	}
}

func TestPageSafety(t *testing.T) {
	e := newEnv(t)
	s := e.start(time.Minute)
	r := e.get(s, "/?t="+testToken)
	page := string(r.body)
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write"} {
		if strings.Contains(page, bad) {
			t.Errorf("page contains %s", bad)
		}
	}
	for _, want := range []string{"/api/file", "/api/cooldown/clear", "/api/sweep", "Usage (24h)", "Running"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}
}

func readEvent(t *testing.T, r *bufio.Reader) map[string]any {
	t.Helper()
	data := ""
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended: %v", err)
		}
		if strings.HasPrefix(line, "data: ") {
			data = strings.TrimPrefix(line, "data: ")
		}
		if line == "\n" && data != "" {
			var out map[string]any
			if err := json.Unmarshal([]byte(data), &out); err != nil {
				t.Fatal(err)
			}
			return out
		}
	}
}

func TestStreamFirstSnapshotThenUpdateOnChange(t *testing.T) {
	e := newEnv(t)
	old := SSEPoll
	SSEPoll = 50 * time.Millisecond
	t.Cleanup(func() { SSEPoll = old })
	statePath := e.makeWave("w1", "", true)
	s := e.start(time.Minute)
	req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/api/stream?t=%s", s.Port(), testToken), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	reader := bufio.NewReader(resp.Body)
	first := readEvent(t, reader)
	counts := first["waves"].([]any)[0].(map[string]any)["counts"].(map[string]any)
	if counts["implementing"] != float64(1) {
		t.Errorf("first counts = %v", counts)
	}
	doc := e.stateDoc("")
	doc["packages"].(map[string]any)["A-1"].(map[string]any)["status"] = "ready"
	e.write(statePath, mustJSON(t, doc))
	second := readEvent(t, reader)
	counts = second["waves"].([]any)[0].(map[string]any)["counts"].(map[string]any)
	if counts["ready"] != float64(1) {
		t.Errorf("second counts = %v", counts)
	}
}

func TestStopRoute(t *testing.T) {
	e := newEnv(t)
	statePath := e.makeWave("w1", "", true)
	other := e.makeWave("w2", "", false)
	s := e.start(time.Minute)
	post := func(target string, header bool) response {
		path := "/api/stop"
		headers := map[string]string{"Content-Type": "application/json"}
		if header {
			headers["X-Orchestra-Token"] = testToken
		} else {
			path += "?t=" + testToken
		}
		return e.request(s, "POST", path, headers, mustJSON(t, map[string]any{"state": target}), "")
	}
	if r := post(statePath, false); r.status != 403 || state.StopRequested(statePath) {
		t.Errorf("query token: %d", r.status)
	}
	if r := post(other, true); r.status != 404 || state.StopRequested(other) {
		t.Errorf("unregistered: %d", r.status)
	}
	r := post(statePath, true)
	if r.status != 200 || string(r.body) != `{"stop_requested":true}` && string(r.body) != `{"stop_requested": true}` {
		t.Errorf("registered: %d %s", r.status, r.body)
	}
	if !state.StopRequested(statePath) {
		t.Error("STOP not written")
	}
}

func TestPostValidation(t *testing.T) {
	e := newEnv(t)
	s := e.start(time.Minute)
	h := map[string]string{"X-Orchestra-Token": testToken}
	if r := e.request(s, "POST", "/api/nope", h, "{}", ""); r.status != 404 {
		t.Errorf("unknown route = %d", r.status)
	}
	if r := e.request(s, "POST", "/api/stop", h, "{broken", ""); r.status != 400 {
		t.Errorf("bad body = %d", r.status)
	}
	if r := e.request(s, "POST", "/api/stop", nil, "{}", ""); r.status != 403 {
		t.Errorf("no token = %d", r.status)
	}
	if r := e.request(s, "POST", "/api/sweep", h, `{"repo": "/nowhere"}`, ""); r.status != 404 {
		t.Errorf("unregistered repo = %d", r.status)
	}
	if r := e.request(s, "POST", "/api/sweep?t="+testToken, nil, `{"repo": "/nowhere"}`, ""); r.status != 403 {
		t.Errorf("sweep with query token = %d", r.status)
	}
}

func TestCooldownClear(t *testing.T) {
	e := newEnv(t)
	e.makeWave("w1", "", true)
	path := filepath.Join(e.home, ".claude", "orchestra", "executor-state.json")
	plant := func() {
		e.write(path, mustJSON(t, map[string]any{
			"claude": map[string]any{"until": 4e9, "reason": "quota"},
			"pi":     map[string]any{"until": 4e9, "reason": "quota"}}))
	}
	loaded := func() map[string]any {
		var m map[string]any
		if raw, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(raw, &m)
		}
		return m
	}
	plant()
	s := e.start(time.Minute)
	h := map[string]string{"X-Orchestra-Token": testToken}
	r := e.request(s, "POST", "/api/cooldown/clear", h, `{"executor": "claude"}`, "")
	if r.status != 200 || !strings.Contains(string(r.body), `"cleared"`) || len(loaded()) != 1 || loaded()["pi"] == nil {
		t.Errorf("clear one: %d %s %v", r.status, r.body, loaded())
	}
	r = e.request(s, "POST", "/api/cooldown/clear", h, `{"executor": null}`, "")
	if r.status != 200 || len(loaded()) != 0 {
		t.Errorf("clear all: %d %s %v", r.status, r.body, loaded())
	}
	plant()
	if r = e.request(s, "POST", "/api/cooldown/clear", h, `{"executor": "nope"}`, ""); r.status != 400 || len(loaded()) != 2 {
		t.Errorf("unknown: %d %s", r.status, r.body)
	}
	if r = e.request(s, "POST", "/api/cooldown/clear?t="+testToken, nil, `{"executor": null}`, ""); r.status != 403 || len(loaded()) != 2 {
		t.Errorf("query token: %d", r.status)
	}
}

func TestFileRoute(t *testing.T) {
	e := newEnv(t)
	statePath := e.makeWave("w1", "", true)
	wdir := filepath.Dir(statePath)
	planDir := filepath.Join(e.tmp, "plan")
	plan := e.write(filepath.Join(planDir, "plan.json"),
		`{"preamble": ["preamble.md"], "packages": [{"id": "A-1", "spec": "specs/a1.md"}]}`)
	spec := e.write(filepath.Join(planDir, "specs", "a1.md"), "SPEC")
	preamble := e.write(filepath.Join(planDir, "preamble.md"), "PRE")
	unrelated := e.write(filepath.Join(e.tmp, "unrelated.md"), "SECRET")
	doc := e.stateDoc("")
	doc["plan"] = plan
	e.write(statePath, mustJSON(t, doc))
	for _, d := range []string{"context", "corrections", "carry"} {
		if err := os.MkdirAll(filepath.Join(wdir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s := e.start(time.Minute)
	fetch := func(p string) response {
		return e.get(s, "/api/file?t="+testToken+"&path="+strings.NewReplacer("/", "%2F", " ", "%20").Replace(p))
	}

	ctx := e.write(filepath.Join(wdir, "context", "A-1.md"), "CTX")
	cor := e.write(filepath.Join(wdir, "corrections", "A-1.md"), "COR")
	car := e.write(filepath.Join(wdir, "carry", "n.md"), "CAR")
	for p, want := range map[string]string{spec: "SPEC", preamble: "PRE", ctx: "CTX", cor: "COR", car: "CAR"} {
		if r := fetch(p); r.status != 200 || string(r.body) != want {
			t.Errorf("%s = %d %q", p, r.status, r.body)
		}
	}
	if ct := fetch(spec).header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("content type = %q", ct)
	}
	if r := e.get(s, "/api/file?path="+spec); r.status != 403 {
		t.Errorf("no token = %d", r.status)
	}
	for _, p := range []string{
		unrelated,
		filepath.Join(wdir, "context") + "/../../unrelated.md",
		planDir + "/specs/../specs/a1.md",
		plan,
		filepath.Join(wdir, "context"),
		planDir,
		"",
	} {
		if r := fetch(p); r.status != 404 {
			t.Errorf("%q = %d, want 404", p, r.status)
		}
	}
	link := filepath.Join(wdir, "context", "link.md")
	if err := os.Symlink(unrelated, link); err != nil {
		t.Fatal(err)
	}
	if r := fetch(link); r.status != 404 {
		t.Errorf("symlink escape = %d", r.status)
	}
	big := filepath.Join(wdir, "context", "big.md")
	e.write(big, strings.Repeat("x", 1024*1024+1))
	if r := fetch(big); r.status != 413 {
		t.Errorf("oversize = %d", r.status)
	}
	pkgs := map[string]map[string]any{}
	for _, p := range e.snapshot(s)["waves"].([]any)[0].(map[string]any)["packages"].([]any) {
		pkgs[p.(map[string]any)["id"].(string)] = p.(map[string]any)
	}
	if pkgs["A-1"]["spec"] != spec || pkgs["A-1"]["context"] != ctx || pkgs["A-1"]["correction"] != cor || pkgs["A-2"]["spec"] != nil {
		t.Errorf("links = %v", pkgs)
	}
}

func TestRunningSection(t *testing.T) {
	e := newEnv(t)
	statePath := e.makeWave("w1", "", true)
	e.write(filepath.Join(filepath.Dir(statePath), "orca-sessions.json"), mustJSON(t, []any{
		map[string]any{"wave_state": statePath, "pkg": "A-1", "terminal": "term-1", "worktree": "/wt/a1"}}))
	cfg, err := config.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	dir := dispatch.DetachDir(cfg)
	e.write(filepath.Join(dir, "dsp-00000000000a.json"), mustJSON(t, map[string]any{
		"pid": os.Getpid(), "token": "dsp-00000000000a", "started": 123.0, "executor": "pi"}))
	e.write(filepath.Join(dir, "dsp-00000000000b.json"), mustJSON(t, map[string]any{
		"pid": 1 << 22, "token": "dsp-00000000000b", "started": 100.0}))
	running := e.snapshot(e.start(time.Minute))["running"].(map[string]any)
	states := map[string]string{}
	for _, d := range running["dispatches"].([]any) {
		m := d.(map[string]any)
		states[m["token"].(string)] = m["state"].(string)
	}
	if len(states) != 2 || states["dsp-00000000000a"] != "running" || states["dsp-00000000000b"] != "dead-no-result" {
		t.Errorf("dispatches = %v", states)
	}
	if got := mustJSON(t, running["by_executor"]); got != `{"pi":1}` {
		t.Errorf("by_executor = %s", got)
	}
	if got := mustJSON(t, running["orca_sessions"]); !strings.Contains(got, `"terminal":"term-1"`) {
		t.Errorf("orca_sessions = %s", got)
	}
	if running["waves_in_flight"] != float64(1) {
		t.Errorf("waves_in_flight = %v", running["waves_in_flight"])
	}
}

func TestIdleShutdown(t *testing.T) {
	newEnv(t)
	s, err := NewServer(0, testToken, 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = s.Run(); close(done) }()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("server did not shut down when idle")
	}
}

func TestUsageIsCachedForAMinute(t *testing.T) {
	newEnv(t)
	cfg, err := config.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	sentinel := map[string]any{"cached": true}
	usageMu.Lock()
	usageAt, usageValue = time.Now(), sentinel
	usageMu.Unlock()
	if got := usageSnapshot(cfg, nil); mustJSON(t, got) != `{"cached":true}` {
		t.Errorf("a fresh cache entry was not used: %s", mustJSON(t, got))
	}
	usageMu.Lock()
	usageAt = time.Now().Add(-2 * usageTTL)
	usageMu.Unlock()
	if got := usageSnapshot(cfg, nil); mustJSON(t, got) == `{"cached":true}` {
		t.Error("a stale cache entry was used")
	}
}

func TestInfoFileRoundTrip(t *testing.T) {
	e := newEnv(t)
	path := InfoPath()
	if readInfo(path) != nil {
		t.Fatal("info before any server")
	}
	e.write(path, `{"pid": 5, "port": 6, "token": "tok", "started": 1.5}`)
	info := readInfo(path)
	if info == nil || info.pid != 5 || info.port != 6 || info.token != "tok" || info.url() != "http://127.0.0.1:6/?t=tok" {
		t.Errorf("info = %+v", info)
	}
	e.write(path, `{"pid": 5, "port": 6}`)
	if readInfo(path) != nil {
		t.Error("an info file without a token is not usable")
	}
	e.write(path, `[1]`)
	if readInfo(path) != nil {
		t.Error("a non-object info file is not usable")
	}
}
