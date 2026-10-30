package hookrun

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/advise"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
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

//go:embed assets/advice.md
var sessionStartAdvice string

// sessionStartAdvicePlaceholder is replaced by one line per advice.
const sessionStartAdvicePlaceholder = "{{ADVICE}}"

// sessionStartAdviceBudget bounds the repo lookup and walk that find the advice,
// so a large checkout cannot hold SessionStart open.
const sessionStartAdviceBudget = 1500 * time.Millisecond

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
		context := "<orchestra-router>\n" + sessionStartRouter
		if advice := sessionStartAdviceFor(cwd); advice != "" {
			context += "\n" + advice
		}
		return context
	}
	return ""
}

// sessionStartAdviceFor is the advice block for the repository containing cwd,
// or "" when there is none, cwd is not in a repository, or the lookup misses
// sessionStartAdviceBudget. The lookup runs in a goroutine so the budget holds
// even if git or the walk blocks; a late result is dropped.
func sessionStartAdviceFor(cwd string) string {
	done := make(chan []advise.Advice, 1)
	go func() {
		repo, ok := gitx.RepoRoot(cwd)
		if !ok {
			done <- nil
			return
		}
		done <- advise.Repo(repo)
	}()
	var list []advise.Advice
	select {
	case list = <-done:
	case <-time.After(sessionStartAdviceBudget):
		return ""
	}
	if len(list) == 0 {
		return ""
	}
	lines := make([]string, 0, len(list))
	for _, a := range list {
		lines = append(lines, fmt.Sprintf("- %s: %s. Why: %s. Fix: %s.", a.Path, a.Problem, a.Why, a.Fix))
	}
	return strings.Replace(sessionStartAdvice, sessionStartAdvicePlaceholder, strings.Join(lines, "\n"), 1)
}

// sessionStartField is the input's string field key, or "" when absent or not
// a string.
func sessionStartField(in map[string]any, key string) string {
	s, _ := in[key].(string)
	return s
}
