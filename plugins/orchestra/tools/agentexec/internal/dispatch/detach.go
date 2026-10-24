package dispatch

import (
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
)

// A run can outlast its caller's per-call timeout (a relay's Bash call is
// capped near 600 s). `--detach` runs the token's dispatch as a background
// child whose stdout goes to a file; `dispatch wait` polls it for a bounded
// window and persists the final result, so a caller retries `wait` instead
// of losing the run.
//
// Files, all under DetachDir: <T>.json (state: pid, token, started,
// wait_calls, out[, collected, finished]), <T>.out (the child's output),
// <T>.result.json (the collected result), <T>.answer.txt (its full answer),
// <T>.lock (serializes the collect step of concurrent waits).

// SummaryAnswerChars is how many code points of `answer` a --summary keeps.
const SummaryAnswerChars = 400

// DefaultMaxWait is `dispatch wait`'s default polling window, under a relay's
// Bash limit.
const DefaultMaxWait = 560 * time.Second

// relayRawChars is how much of an unparsable child output a relay-failed
// result keeps (its tail, in code points).
const relayRawChars = 2000

// collectLockTimeout bounds the wait for another `wait` collecting the same
// token; past it the collect proceeds unlocked (best effort, as before).
const collectLockTimeout = 30 * time.Second

// beforeCollect runs just before a wait reads the child's output (a seam
// for the concurrent-collect test).
var beforeCollect = func() {}

// DetachDir is the directory of the detach files.
func DetachDir(cfg *config.Resolved) string { return filepath.Join(TokenDir(cfg), "detach") }

func detachFile(cfg *config.Resolved, token, suffix string) string {
	return filepath.Join(DetachDir(cfg), token+suffix)
}

func readObject(path string) map[string]any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	v, err := fsutil.DecodeJSON(raw)
	if err != nil {
		return nil
	}
	o, _ := v.(map[string]any)
	return o
}

// writeText is an atomic write that creates the directory.
func writeText(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(path, data, 0o600)
}

func writeState(path string, state map[string]any) {
	if b, err := fsutil.MarshalJSON(state); err == nil {
		_ = writeText(path, b)
	}
}

func nowSeconds() float64 { return float64(time.Now().UnixMicro()) / 1e6 }

// Detach starts `agent-exec dispatch <args>` (args: the token's resolved,
// --token-free flags; see Request.Args) as a detached child in its own
// session. Idempotent per token: while the child runs it reports running;
// once it finished but the result was not collected, done (the output stays
// for `wait`); after a collected result it is a deliberate re-dispatch.
func Detach(cfg *config.Resolved, token string, args []string) map[string]any {
	statePath := detachFile(cfg, token, ".json")
	outPath := detachFile(cfg, token, ".out")
	_ = os.MkdirAll(DetachDir(cfg), 0o777)
	if _, err := os.Stat(statePath); err == nil {
		state := readObject(statePath)
		if state == nil {
			state = map[string]any{}
		}
		pv := state["pid"]
		if pid := pidOf(pv); pidAlive(pid) {
			return map[string]any{"status": "running", "token": token, "pid": pv}
		}
		if c, _ := state["collected"]; c != true {
			return map[string]any{"status": "done", "token": token}
		}
		for _, suffix := range []string{".result.json", ".answer.txt", ".out"} {
			_ = os.Remove(detachFile(cfg, token, suffix))
		}
	}
	pid, err := spawnDetached(outPath, args)
	if err != nil {
		return map[string]any{
			"status": "relay-failed",
			"token":  token,
			"raw":    "could not start the detached dispatch: " + err.Error(),
		}
	}
	writeState(statePath, map[string]any{
		"pid":        int64(pid),
		"token":      token,
		"started":    nowSeconds(),
		"wait_calls": int64(0),
		"out":        outPath,
	})
	return map[string]any{"status": "detached", "token": token, "pid": int64(pid)}
}

// spawnDetached re-runs this binary as `dispatch <args>` with stdout and
// stderr in outPath, stdin from /dev/null, in a new session.
func spawnDetached(outPath string, args []string) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	out, err := os.Create(outPath)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	cmd := exec.Command(exe, append([]string{"dispatch"}, args...)...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = out, out, nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}

// Wait polls token's detached child for at most maxWait and returns its
// final result (persisted, so every later Wait returns the same object), a
// `running` progress object, or `unknown-token` when nothing was detached.
// summary cuts the answer (Summarize). The token must be well formed (the
// caller rejects anything else as a usage error). exit is always 0.
func Wait(cfg *config.Resolved, token string, maxWait time.Duration, summary bool) (map[string]any, int) {
	statePath := detachFile(cfg, token, ".json")
	resultPath := detachFile(cfg, token, ".result.json")
	state := readObject(statePath)
	if state != nil {
		calls := state["wait_calls"]
		n, _ := calls.(int64)
		state["wait_calls"] = n + 1
		writeState(statePath, state)
	}
	present := func(o map[string]any) map[string]any {
		if summary {
			return Summarize(o)
		}
		return o
	}
	if cached := readObject(resultPath); cached != nil {
		if state != nil {
			if c, _ := state["collected"]; c != true {
				state["collected"] = true
				if _, has := state["finished"]; !has {
					state["finished"] = fsutil.NowISO()
				}
				writeState(statePath, state)
			}
		}
		return present(cached), 0
	}
	if state == nil {
		return map[string]any{"status": "unknown-token", "token": token}, 0
	}
	pv := state["pid"]
	pid := pidOf(pv)
	outPath := detachFile(cfg, token, ".out")
	if o, _ := state["out"]; o != nil {
		if s, ok := o.(string); ok {
			outPath = s
		}
	}
	start := time.Now()
	deadline := start.Add(max(maxWait, 0))
	for pidAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(min(2*time.Second, max(50*time.Millisecond, time.Until(deadline))))
	}
	if pidAlive(pid) {
		calls := state["wait_calls"]
		return map[string]any{
			"status":     "running",
			"token":      token,
			"pid":        pv,
			"waited_s":   int64(time.Since(start).Seconds()),
			"wait_calls": calls,
		}, 0
	}
	// Collect under a per-token lock: of two concurrent waits on a finished
	// child, the loser reads the winner's result instead of finding .out
	// gone and persisting a relay failure over it.
	if unlock, err := fsutil.Lock(detachFile(cfg, token, ".lock"), fsutil.Exclusive, collectLockTimeout); err == nil {
		defer unlock()
		if cached := readObject(resultPath); cached != nil {
			return present(cached), 0
		}
	}
	beforeCollect()
	raw, _ := os.ReadFile(outPath)
	result, ok := extractObject(string(raw))
	if !ok {
		result = map[string]any{
			"status": "relay-failed",
			"token":  token,
			"raw":    tailRunes(string(raw), relayRawChars),
		}
	}
	AttachAnswerPath(cfg, token, result)
	if b, err := fsutil.MarshalJSON(result); err == nil {
		_ = writeText(resultPath, b)
	}
	state["finished"] = fsutil.NowISO()
	state["collected"] = true
	writeState(statePath, state)
	_ = os.Remove(outPath)
	return present(result), 0
}

func tailRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

// AttachAnswerPath writes a non-empty string `answer` to the token's answer
// file and records its path as `answer_path` (best effort).
func AttachAnswerPath(cfg *config.Resolved, token string, result map[string]any) {
	a := result["answer"]
	answer, ok := a.(string)
	if !ok || answer == "" {
		return
	}
	path := abs(detachFile(cfg, token, ".answer.txt"))
	if writeText(path, []byte(answer)) == nil {
		result["answer_path"] = path
	}
}

// Summarize is a copy of result with `answer` cut to its first
// SummaryAnswerChars code points (and answer_truncated: true) when longer.
func Summarize(result map[string]any) map[string]any {
	out := maps.Clone(result)
	a := out["answer"]
	if answer, ok := a.(string); ok {
		if r := []rune(answer); len(r) > SummaryAnswerChars {
			out["answer"] = string(r[:SummaryAnswerChars])
			out["answer_truncated"] = true
		}
	}
	return out
}

// CompactLine is the one-line --summary rendering (no spaces after the
// separators).
func CompactLine(o map[string]any) ([]byte, error) { return compactJSON(o) }

// extractObject is ExtractJSONObject keeping key order: the whole trimmed
// text, else its outermost {...} span.
func extractObject(raw string) (map[string]any, bool) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return nil, false
	}
	if v, err := fsutil.DecodeJSON([]byte(text)); err == nil {
		o, ok := v.(map[string]any)
		return o, ok && o != nil
	}
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return nil, false
	}
	v, err := fsutil.DecodeJSON([]byte(text[start : end+1]))
	if err != nil {
		return nil, false
	}
	o, ok := v.(map[string]any)
	return o, ok && o != nil
}

// ExtractJSONObject is best-effort extraction of the JSON object a CLI
// printed, possibly among banner/log lines: the whole trimmed text, else
// its outermost {...} span. false when there is no object.
func ExtractJSONObject(raw string) (map[string]any, bool) {
	o, ok := extractObject(raw)
	if !ok {
		return nil, false
	}
	b, err := fsutil.MarshalJSON(o)
	if err != nil {
		return nil, false
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return nil, false
	}
	return m, true
}
