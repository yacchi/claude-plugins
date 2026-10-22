package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/checks"
)

// The screen markers Claude Code's TUI shows. herdr matches the trust
// dialog case-insensitively on either marker; Orca on the exact question.
var (
	trustMarkers = []string{"trust this folder", "is this a project you"}
	// OrcaTrustMarker is the folder-trust question as Orca shows it.
	OrcaTrustMarker = "Is this a project you created or one you trust"

	trustYesRE = regexp.MustCompile(`^\s*❯\s*(?:\d+\.\s*)?Yes, I trust`)
	optionRE   = regexp.MustCompile(`^\s*(?:❯\s*)?(?:\d+\.\s*)?(?:Yes|No|Allow|Deny|Cancel|Approve|Reject)\b`)
	// Only a selected option can open a dialog block; plain option-looking
	// lines ("  No issues found") count only after a block has started.
	selectedOptionRE = regexp.MustCompile(`^\s*❯\s*(?:\d+\.\s*)?(?:Yes|No|Allow|Deny|Cancel|Approve|Reject)\b`)
	permissionRE     = regexp.MustCompile(`Do you want to `)
	// Every auth failure ends in a /login hint ("Not logged in · Please run
	// /login", "Invalid API key · Run /login") and the agent goes idle.
	loginRE = regexp.MustCompile(`·\s*(?:Please run|Run) /login\b`)
)

// IsLoginText reports Claude Code's "no usable login" reply in a screen's
// text: the agent is idle and will never write a result.
func IsLoginText(text string) bool { return loginRE.MatchString(text) }

// IsTrustText reports the folder-trust dialog in a screen's text (herdr).
func IsTrustText(text string) bool {
	lower := strings.ToLower(text)
	for _, m := range trustMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// IsTrust reports the folder-trust dialog in screen lines (Orca).
func IsTrust(screen []string) bool {
	for _, line := range screen {
		if strings.Contains(line, OrcaTrustMarker) {
			return true
		}
	}
	return false
}

// TrustYesSelected reports the trust dialog's cursor on "Yes, I trust".
func TrustYesSelected(screen []string) bool {
	for _, line := range screen {
		if trustYesRE.MatchString(line) {
			return true
		}
	}
	return false
}

// HasPromptBox reports Claude's input box ("❯ ") on screen.
func HasPromptBox(screen []string) bool {
	for _, line := range screen {
		if strings.HasPrefix(strings.TrimLeftFunc(line, unicode.IsSpace), "❯") {
			return true
		}
	}
	return false
}

func dialogStart(screen []string) int {
	for i, line := range screen {
		if permissionRE.MatchString(line) || selectedOptionRE.MatchString(line) {
			return i
		}
	}
	return -1
}

// IsDialog reports a permission question or a numbered choice dialog.
func IsDialog(screen []string) bool { return dialogStart(screen) >= 0 }

// DialogLines is the visible block of a permission/choice dialog.
func DialogLines(screen []string) []string {
	start := dialogStart(screen)
	if start < 0 {
		return nil
	}
	end := start
	for i := start + 1; i < len(screen); i++ {
		line := screen[i]
		if optionRE.MatchString(line) {
			end = i
		} else if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "❯") {
			break
		}
	}
	return screen[start : end+1]
}

// Excerpt is the failure excerpt of a host message (checks.FailureExcerpt).
func Excerpt(text string) string { return checks.FailureExcerpt(text) }

// TailLines is the excerpt of the last 40 screen lines.
func TailLines(screen []string) string {
	if len(screen) > 40 {
		screen = screen[len(screen)-40:]
	}
	return Excerpt(strings.Join(screen, "\n"))
}

// TailText is TailLines of a screen given as text.
func TailText(text string) string { return TailLines(SplitLines(text)) }

// SplitLines splits terminal screen text on newlines (CRLF-tolerant),
// dropping the trailing empty line.
func SplitLines(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// ReadResult is the result file at path when it holds a JSON object with
// status ok or escalate; nil otherwise (missing, partial, other status).
func ReadResult(path string) map[string]any {
	data, _ := ReadResultErr(path)
	return data
}

// ReadResultErr is ReadResult that says why a file that exists is not a
// result: (nil, nil) when it is missing, (nil, err) when it is not a JSON
// object or its status is not ok/escalate, (data, nil) when valid.
func ReadResultErr(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if data == nil {
		return nil, errors.New("invalid JSON: not an object")
	}
	if st, _ := data["status"].(string); st != "ok" && st != "escalate" {
		return nil, fmt.Errorf("status must be \"ok\" or \"escalate\", got %v", data["status"])
	}
	return data, nil
}

func isRuleLine(line string) bool {
	rules, other := 0, 0
	for _, r := range line {
		switch {
		case r == '─':
			rules++
		case !unicode.IsSpace(r):
			other++
		}
	}
	return rules >= 20 && rules > other
}

// PendingInput is the text sitting in Claude's input box: the lines
// between the LAST TWO rule lines, joined with spaces, the leading "❯"
// removed and whitespace collapsed. ok is false when the screen has fewer
// than two rule lines; text is "" for an empty box.
func PendingInput(lines []string) (text string, ok bool) {
	var rules []int
	for i, line := range lines {
		if isRuleLine(line) {
			rules = append(rules, i)
		}
	}
	if len(rules) < 2 {
		return "", false
	}
	box := lines[rules[len(rules)-2]+1 : rules[len(rules)-1]]
	joined := strings.Join(strings.Fields(strings.Join(box, " ")), " ")
	joined = strings.TrimSpace(strings.TrimPrefix(joined, "❯"))
	return joined, true
}

// ResetResult removes a stale result file and makes its directory.
func ResetResult(path string) {
	_ = os.Remove(path)
	if abs, err := filepath.Abs(path); err == nil {
		_ = os.MkdirAll(filepath.Dir(abs), 0o755)
	}
}

// PromptLine is the one line a package prompt sends.
func PromptLine(promptFiles []string, resultPath string) string {
	return "Read the worker prompt from " + strings.Join(promptFiles, " and ") +
		" and carry it out. You are a worker: do it yourself in your working tree; do not load orchestra:run, call the Workflow tool, or run agent-exec dispatch or agent-exec wave. When you are completely done, write " + resultPath +
		` containing JSON {"status": "ok"|"escalate", "summary": "<what you changed and anything a reviewer must know; no code or logs>"}.` +
		" If you must ESCALATE, put the reason in summary."
}

// ResultOutput is the dispatch-shaped result of a finished prompt:
// status ok, the summary as answer ("ESCALATE: " prefixed when the result
// escalated), the session handle, the worktree as isolation, and the
// network denials when there were any.
func ResultOutput(hostName string, s *Session, sessionID string, resumed bool, data map[string]any) map[string]any {
	summary, _ := data["summary"].(string)
	if _, isStr := data["summary"].(string); !isStr && Truthy(data["summary"]) {
		b, _ := json.Marshal(data["summary"])
		summary = string(b)
	}
	answer := summary
	if data["status"] == "escalate" {
		answer = "ESCALATE: " + summary
	}
	out := map[string]any{
		"status": "ok", "answer": answer, "session_id": nullable(sessionID), "resumed": resumed,
		"executor":  hostName,
		"isolation": map[string]any{"isolate": true, "path": s.Worktree, "workdir": s.Worktree},
	}
	if d, ok := data["network_denials"]; ok && Truthy(d) {
		out["network_denials"] = d
	}
	return out
}

// PromptAndWait is one dispatch to a live session: Prompt, then
// WaitResult; without keep_sessions the agent is closed after the wait
// (whatever its outcome). It returns ResultOutput or the *NeedError.
func PromptAndWait(ctx context.Context, h Host, s *Session, promptFiles []string, resultPath string, timeout time.Duration) (map[string]any, error) {
	if err := h.Prompt(ctx, s, promptFiles, resultPath, timeout); err != nil {
		return nil, err
	}
	name, resumed := s.Terminal, s.Resumed
	data, err := h.WaitResult(ctx, s, resultPath, timeout)
	keep := true
	if rt, ok := h.(Runtime); ok {
		keep = rt.Settings().KeepSessions
	}
	if !keep {
		_ = h.Close(s, false)
	}
	if err != nil {
		return nil, err
	}
	return ResultOutput(h.Name(), s, name, resumed, data), nil
}

// AsNeedOrNil is AsNeed that keeps a nil error nil (no typed-nil trap).
func AsNeedOrNil(err error) error {
	if err == nil {
		return nil
	}
	return AsNeed(err)
}

// AsNeed is err as a *NeedError (a context end becomes an error need).
func AsNeed(err error) *NeedError {
	var need *NeedError
	if errors.As(err, &need) {
		return need
	}
	if err == nil {
		return nil
	}
	return Need(KindError, err.Error())
}
