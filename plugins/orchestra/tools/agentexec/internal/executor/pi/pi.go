// Package pi knows the `pi` CLI executor's shapes: the headless argv and
// the parse of its `--mode json` output into the normalized result every
// dispatch consumer reads.
package pi

import (
	"math"
	"regexp"
	"strings"
	"unicode"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
)

// SkipVersionCheckEnv keeps a pi child from stalling on an update check.
const SkipVersionCheckEnv = "PI_SKIP_VERSION_CHECK=1"

// BuildArgv is the argv of one headless pi run:
//
//	pi -p --mode json --no-approve --no-extensions --no-skills
//	   --no-prompt-templates --no-themes [--no-context-files]
//	   [--model M] [--thinking E] [--session SID]
//
// The prompt is never in argv (it goes on stdin), pi has no workdir flag
// (the workdir is the child's cwd) and json is the only output mode the
// parser reads, so workdir, prompt and outFmt are accepted for signature
// parity and ignored. resume "" is a fresh session.
func BuildArgv(exe, model, effort, workdir, prompt, resume, outFmt string, contextFiles bool) []string {
	_, _, _ = workdir, prompt, outFmt
	argv := []string{exe, "-p", "--mode", "json", "--no-approve", "--no-extensions",
		"--no-skills", "--no-prompt-templates", "--no-themes"}
	if !contextFiles {
		argv = append(argv, "--no-context-files")
	}
	if model != "" {
		argv = append(argv, "--model", model)
	}
	if effort != "" {
		argv = append(argv, "--thinking", effort)
	}
	if resume != "" {
		argv = append(argv, "--session", resume)
	}
	return argv
}

type pattern struct {
	reason string
	re     *regexp.Regexp
}

// unavailablePatterns classify a failure, first match wins; the last one
// is pi's own "No API key found for <x>." on stderr.
var unavailablePatterns = []pattern{
	{"quota", regexp.MustCompile(`(?i)quota|usage limit|premium request`)},
	{"rate-limit", regexp.MustCompile(`(?i)rate limit|rate-limit| 429`)},
	{"credits", regexp.MustCompile(`(?i)insufficient|credit|out of credits`)},
	{"auth", regexp.MustCompile(`(?i)unauthor|authenticat| 401|not logged in|login`)},
	{"auth", regexp.MustCompile(`(?i)no api key`)},
}

// contentEventPrefixes are event types that embed worker/tool content:
// never scanned for availability signal, whatever fields they carry.
var contentEventPrefixes = []string{"tool", "message", "turn", "agent", "session"}

// IsErrorBearingEvent is the default-deny gate of the availability scan: an
// event is the CLI reporting on itself only when its type contains
// "error", it carries a truthy top-level `error`, or it is a `result`
// event that says the run did not succeed.
func IsErrorBearingEvent(event map[string]any) bool {
	etype := event["type"]
	if s, ok := etype.(string); ok && strings.Contains(config.PyLower(s), "error") {
		return true
	}
	if v, _ := event["error"]; truthy(v) {
		return true
	}
	if etype == "result" {
		if v, _ := event["success"]; v == false {
			return true
		}
		if v, _ := event["ok"]; v == false {
			return true
		}
		if v, _ := event["isError"]; truthy(v) {
			return true
		}
	}
	return false
}

// truthy is Python's bool() of a decoded JSON value.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case int64:
		return x != 0
	case float64:
		return x != 0
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return x != nil && len(x) > 0
	}
	return true
}

func nonNegInt(v any) (int64, bool) {
	n, ok := v.(int64)
	return n, ok && n >= 0
}

func accumulate(tokens map[string]any, key string, v any) {
	n, ok := nonNegInt(v)
	if !ok {
		return
	}
	prev := tokens[key]
	p, _ := prev.(int64)
	tokens[key] = p + n
}

// AccumulateInt adds v to target[key] iff v is a non-negative integer.
func AccumulateInt(target map[string]any, key string, v any) { accumulate(target, key, v) }

func textOf(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, block := range c {
			b, ok := block.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := b["type"]; t != "text" {
				continue
			}
			if text, ok := mustGet(b, "text").(string); ok {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func mustGet(o map[string]any, k string) any {
	v := o[k]
	return v
}

// isSpace is Python's str.isspace for one rune.
func isSpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

func blank(s string) bool { return strings.TrimFunc(s, isSpace) == "" }

// ParseJSONL turns `pi -p --mode json` stdout + stderr + exit code into
// the normalized result: status, answer, session_id, resumed, reason,
// exit_code, usage.
//
// Records are split on "\n" only (U+2028/U+2029 are legal inside JSON
// strings). The session id is the first `session` event's id. An
// assistant `message_end` is authoritative: its usage is summed and the
// answer is the text of the LAST one; `message_update` and non-assistant
// messages are ignored. A final stopReason `error` is a failure even at
// exit 0. The availability scan is default-deny: only an assistant
// errorMessage, stderr, unparseable lines and error-bearing events are
// scanned, never tool output or message content.
func ParseJSONL(stdout, stderr string, exitCode int, resumed bool) map[string]any {
	var sessionID any
	tokens := map[string]any{}
	costUSD, sawCost := 0.0, false
	var lastAssistant map[string]any
	var scannable []string

	for _, raw := range strings.Split(stdout, "\n") {
		line := strings.TrimSuffix(raw, "\r")
		if blank(line) {
			continue
		}
		v, err := fsutil.DecodeJSON([]byte(line))
		if err != nil {
			scannable = append(scannable, line)
			continue
		}
		event, ok := v.(map[string]any)
		if !ok {
			continue
		}
		etype := event["type"]
		switch etype {
		case "session":
			if sid, ok := mustGet(event, "id").(string); ok && sessionID == nil && sid != "" {
				sessionID = sid
			}
			continue
		case "message_end":
			message, ok := mustGet(event, "message").(map[string]any)
			if !ok || mustGet(message, "role") != "assistant" {
				continue
			}
			lastAssistant = message
			if usage, ok := mustGet(message, "usage").(map[string]any); ok {
				accumulate(tokens, "input_tokens", mustGet(usage, "input"))
				accumulate(tokens, "output_tokens", mustGet(usage, "output"))
				accumulate(tokens, "cached_input_tokens", mustGet(usage, "cacheRead"))
				accumulate(tokens, "cache_write_input_tokens", mustGet(usage, "cacheWrite"))
				accumulate(tokens, "reasoning_output_tokens", mustGet(usage, "reasoning"))
				if cost, ok := mustGet(usage, "cost").(map[string]any); ok {
					switch t := mustGet(cost, "total").(type) {
					case int64:
						if t >= 0 {
							costUSD += float64(t)
							sawCost = true
						}
					case float64:
						if t >= 0 {
							costUSD += t
							sawCost = true
						}
					}
				}
			}
			if e, ok := mustGet(message, "errorMessage").(string); ok && e != "" {
				scannable = append(scannable, e)
			}
			continue
		}
		if s, ok := etype.(string); ok && hasAnyPrefix(s, contentEventPrefixes) {
			continue
		}
		if IsErrorBearingEvent(event) {
			scannable = append(scannable, line)
		}
	}

	var answer, stopReason any
	if lastAssistant != nil {
		stopReason = mustGet(lastAssistant, "stopReason")
		if text := textOf(mustGet(lastAssistant, "content")); !blank(text) {
			answer = text
		}
	}

	combined := strings.Join(scannable, "\n") + "\n" + stderr
	var reason any
	for _, p := range unavailablePatterns {
		if p.re.MatchString(combined) {
			reason = p.reason
			break
		}
	}
	status := "ok"
	switch {
	case reason != nil:
		status = "unavailable"
	case exitCode != 0:
		status, reason = "unavailable", "nonzero-exit"
	case stopReason == "error" || answer == nil:
		status, reason = "unavailable", "error"
	}

	var usage any
	if len(tokens) > 0 || sawCost {
		u := map[string]any{}
		if len(tokens) > 0 {
			u["tokens"] = tokens
		}
		if sawCost {
			u["cost_micro_usd"] = int64(math.RoundToEven(costUSD * 1000000))
		}
		usage = u
	}
	return map[string]any{
		"status":     status,
		"answer":     answer,
		"session_id": sessionID,
		"resumed":    resumed,
		"reason":     reason,
		"exit_code":  int64(exitCode),
		"usage":      usage,
	}
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
