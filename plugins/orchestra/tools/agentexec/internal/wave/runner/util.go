package runner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/trace"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/state"
)

const notifyTimeout = 30 * time.Second

// emit appends one mechanical-stage event; its detail is the object as
// sorted-key JSON. Observability never takes the loop down.
func (r *runner) emit(name, pkg string, obj map[string]any) {
	detail, err := fsutil.MarshalJSON(obj)
	if err != nil {
		return
	}
	r.store.Event(name, map[string]any{"pkg": nullable(pkg), "detail": string(detail)})
}

// event appends a plain-text event.
func (r *runner) event(name, pkg, detail string) {
	r.store.Event(name, map[string]any{"pkg": nullable(pkg), "detail": detail})
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// notify runs --notify-cmd in the background with the payload on stdin and
// WAVE_EVENT / WAVE_STATE in the environment; Run waits for them all.
func (r *runner) notify(kind string, payload map[string]any) {
	cmd := r.opts.NotifyCmd
	if cmd == "" {
		return
	}
	data, err := fsutil.MarshalJSON(payload)
	if err != nil {
		return
	}
	env := append(os.Environ(), "WAVE_EVENT="+kind, "WAVE_STATE="+r.statePath)
	r.notifyWG.Add(1)
	go func() {
		defer r.notifyWG.Done()
		ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
		defer cancel()
		c := exec.CommandContext(ctx, "/bin/sh", "-c", cmd)
		c.Env = env
		c.Stdin = bytes.NewReader(data)
		end := trace.Begin(trace.KindExec, "wave notify", trace.Attrs{})
		end(exitCode(c.Run()))
	}()
}

// need records a need ("" pid: wave-level) and notifies.
func (r *runner) need(pid, kind, detail string) {
	if err := r.store.AddNeed(pid, kind, detail); err != nil {
		fmt.Fprintf(r.stderr, "agent-exec: wave: could not record need %s for %s: %v\n", kind, pid, err)
	}
	r.notify("need", map[string]any{
		"event":  "need",
		"pkg":    nullable(pid),
		"kind":   kind,
		"detail": detail,
		"at":     r.clock(),
	})
}

// stop records the first stop reason (later ones are ignored).
func (r *runner) stop(reason string) {
	r.mu.Lock()
	if r.stopReason != nil {
		r.mu.Unlock()
		return
	}
	r.stopReason = &reason
	r.mu.Unlock()
	_ = r.store.MarkStopped(reason)
	r.notify("stopped", map[string]any{"event": "stopped", "reason": reason, "at": r.clock()})
}

func (r *runner) stopped() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopReason != nil
}

func (r *runner) isUnavailable() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.unavailable
}

// stopCheck is why the run must stop now ("" = carry on).
func (r *runner) stopCheck() string {
	r.mu.Lock()
	if r.stopReason != nil {
		reason := *r.stopReason
		r.mu.Unlock()
		return reason
	}
	waves, started := r.waves, r.started
	r.mu.Unlock()
	switch {
	case state.StopRequested(r.statePath):
		return "stop requested"
	case r.opts.StopAt != nil && r.clock() >= *r.opts.StopAt:
		return "stop-at reached"
	case r.opts.MaxWaves != nil && waves >= *r.opts.MaxWaves:
		return "max waves reached"
	case r.opts.MaxPackages != nil && started >= *r.opts.MaxPackages:
		return "max packages reached"
	}
	return ""
}

// --- git -----------------------------------------------------------------------

func gitRun(dir string, args ...string) (string, error) { return gitx.Run(dir, args...) }

func gitRepoRoot(p string) (string, bool) { return gitx.RepoRoot(p) }

func (r *runner) revParse(dir, ref string) string {
	out, err := gitRun(dir, "rev-parse", ref)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func (r *runner) intHead() string { return r.revParse(r.intPath, "HEAD") }

// cleanIntegration resets whatever a command left in the integration tree.
func (r *runner) cleanIntegration(after string) int {
	out, err := gitRun(r.intPath, "status", "--porcelain")
	files := 0
	if err == nil {
		files = len(splitLines(out))
	}
	if files > 0 {
		_, _ = gitRun(r.intPath, "reset", "-q", "--hard", "HEAD")
		_, _ = gitRun(r.intPath, "clean", "-fdq")
		r.emit("integration-dirty", "", map[string]any{"files": files, "after": after})
	}
	return files
}

// --- text helpers ------------------------------------------------------------

// splitLines splits command output on newlines (CRLF-tolerant), dropping the
// trailing newline.
func splitLines(s string) []string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func nonBlank(lines []string) []string {
	var out []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func firstLine(text string) string {
	for _, line := range splitLines(text) {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

func containsStr(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

// valText renders a decoded JSON value for text output; "-" for null.
func valText(v any) string {
	switch x := v.(type) {
	case nil:
		return "-"
	case string:
		return x
	}
	b, _ := fsutil.MarshalJSON(v)
	return string(b)
}

// truthy reports whether a decoded JSON value is set: non-null, non-zero,
// non-empty.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case bool:
		return x
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asList(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case []map[string]any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = e
		}
		return out
	case []string:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = e
		}
		return out
	}
	return nil
}

// word is \w for Unicode: letters, digits, _.
const word = `\p{L}\p{N}_`

var (
	pathChars = regexp.MustCompile(`^[` + word + `@.+*?\[\]/-]+$`)
	pathTail1 = regexp.MustCompile(`[` + word + `-]{2,}\.[A-Za-z0-9]+$`)
	pathTail2 = regexp.MustCompile(`[` + word + `-]+\.[A-Za-z0-9]{2,}$`)
)

// pathLike: a repo-relative path contains a "/", or is name.ext with a
// letters/digits extension where name or ext has 2+ chars (so "naming."
// and "e.g." are not paths).
func pathLike(s string) bool {
	if !pathChars.MatchString(s) {
		return false
	}
	return strings.Contains(s, "/") || pathTail1.MatchString(s) || pathTail2.MatchString(s)
}

// autoMaxInFlight is the width for a herdr or orca host: min(16, cpus - 2,
// GiB of RAM / 2), each at least 2; defaultMaxInFlight when the machine size
// is unreadable.
func autoMaxInFlight() int {
	return autoWidth(runtime.NumCPU(), physicalMemory())
}

func autoWidth(cpus int, memory uint64) int {
	if cpus <= 0 || memory == 0 {
		return defaultMaxInFlight
	}
	gib := int(memory / (1 << 30))
	return min(autoMaxInFlightCap, max(2, cpus-2), max(2, gib/2))
}
