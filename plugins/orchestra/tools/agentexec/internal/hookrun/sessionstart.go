package hookrun

import (
	_ "embed"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
)

// The texts injected at SessionStart. They live in assets/ rather than in Go
// strings so the protocol reads as prose; the launcher's source hash covers
// them, so an edit here reaches installed copies.

//go:embed assets/router.md
var sessionStartRouter string

//go:embed assets/selfgate.md
var sessionStartSelfGate string

//go:embed assets/worker.md
var sessionStartWorker string

// sessionStartTreePlaceholder is replaced by the task worktree's path in the
// worker notice.
const sessionStartTreePlaceholder = "{{TREE}}"

// sessionStartSources are the SessionStart `source` values that inject. A
// resumed, cleared or compacted session re-enters the protocol because the
// injected context is otherwise lost; `fork` and anything unknown do not.
var sessionStartSources = map[string]bool{
	"startup": true,
	"resume":  true,
	"clear":   true,
	"compact": true,
}

// sessionStartInstructor matches the main models that run the protocol
// without the self-gate paragraph: the gate is decided here, once.
var sessionStartInstructor = regexp.MustCompile(`(?i)opus|fable`)

// sessionStartOutput is the JSON line Claude Code reads from a SessionStart
// command hook's stdout.
type sessionStartOutput struct {
	HookSpecificOutput sessionStartHook `json:"hookSpecificOutput"`
}

type sessionStartHook struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

// SessionStart answers Claude Code's SessionStart command hook. raw is the
// hook's stdin JSON; the result is the JSON line to print, or nil to print
// nothing. It never fails: unreadable input means nil. As a side effect it
// records EventSessionStart for the input's session_id (best effort), so
// doctor can tell the hook ran even when nothing is injected.
//
// The injection moved here from the hooks module because Claude Code's
// managed-settings path skips the module's classic.SessionStart listener;
// command hooks from a plugin's hooks.json still run.
func SessionStart(raw []byte, now time.Time) []byte {
	var in map[string]any
	if json.Unmarshal(raw, &in) != nil {
		return nil
	}
	if session := sessionStartField(in, "session_id"); session != "" {
		_ = Record(session, EventSessionStart, now)
	}
	context := sessionStartContext(in)
	if context == "" {
		return nil
	}
	out, err := json.Marshal(sessionStartOutput{HookSpecificOutput: sessionStartHook{
		HookEventName:     "SessionStart",
		AdditionalContext: context,
	}})
	if err != nil {
		return nil
	}
	return out
}

// sessionStartContext is the text to inject for the input, or "" for none.
// A task worktree is a hosted worker whatever its model, since the
// instructor already routed it; otherwise the model decides.
func sessionStartContext(in map[string]any) string {
	if !sessionStartSources[sessionStartField(in, "source")] {
		return ""
	}
	cwd := sessionStartField(in, "cwd")
	if cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		}
	}
	if top, ok := isolate.TaskTree(cwd); ok {
		return strings.ReplaceAll(sessionStartWorker, sessionStartTreePlaceholder, top)
	}
	model := strings.TrimSpace(sessionStartField(in, "model"))
	switch {
	case model == "":
		// The model is omittable in the input: the full self-gated text is
		// the safe fallback.
		return sessionStartSelfGate + sessionStartRouter
	case sessionStartInstructor.MatchString(model):
		return "<orchestra-router>\n" + sessionStartRouter
	}
	return ""
}

// sessionStartField is the input's string field key, or "" when absent or not
// a string.
func sessionStartField(in map[string]any, key string) string {
	s, _ := in[key].(string)
	return s
}
