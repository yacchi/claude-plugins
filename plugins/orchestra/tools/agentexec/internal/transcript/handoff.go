package transcript

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

const (
	handoffGoalLines    = 40
	handoffAnswerLines  = 60
	handoffFailedKeep   = 5
	handoffListEntries  = 40
	handoffCommandRunes = 160
	handoffErrorRunes   = 200
)

// Handoff are the facts of one subagent transcript.
type Handoff struct {
	Goal    string          // text of the first "user" line whose content is a string or has text blocks: the agent's prompt
	Answer  string          // text blocks of the "assistant" lines after the last "user" line, joined with "\n"
	Failed  []FailedCommand // Bash tool_use calls whose matching tool_result (by tool_use_id) has is_error true: the last 5, oldest first
	Changed []string        // input file_path of Edit, Write and MultiEdit, and input notebook_path of NotebookEdit tool_use blocks: first-seen order, unique
	Read    []string        // input file_path of Read tool_use blocks not in Changed: first-seen order, unique
}

// FailedCommand: Command is the Bash input command; Error is the first
// non-empty line of the tool_result content (a string, or its text blocks).
type FailedCommand struct{ Command, Error string }

// HandoffPath is <paths.StateDir()>/handoffs/<session>/<agent>.md.
func HandoffPath(session, agent string) string {
	return filepath.Join(paths.StateDir(), "handoffs", session, agent+".md")
}

// ReadHandoff reads the JSONL transcript at path. Only lines whose type is
// "user" or "assistant" count (others, such as "api-request-blob", and lines
// that do not parse are skipped). ok is false when the file cannot be read.
func ReadHandoff(path string) (h Handoff, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return Handoff{}, false
	}
	defer f.Close()
	st := newHandoffState()
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			st.line(line)
		}
		if err == io.EOF {
			return st.finish(), true
		}
		if err != nil {
			return Handoff{}, false
		}
	}
}

// RenderHandoff is the file's markdown.
func RenderHandoff(h Handoff, agentType, agentID string, at time.Time) string {
	blocks := []string{
		fmt.Sprintf("# Handoff: %s %s", agentType, agentID),
		fmt.Sprintf("Written %s when this agent returned. Start a fresh %s with this file instead of resuming the agent: read it first, then do the next step your prompt gives.", at.UTC().Format(time.RFC3339), agentType),
		"## Goal\n" + handoffBody(h.Goal, handoffGoalLines),
		"## Answer\n" + handoffBody(h.Answer, handoffAnswerLines),
		"## Failed commands\n" + handoffFailedBody(h.Failed),
		"## Files changed\n" + handoffListBody(h.Changed),
		"## Files read\n" + handoffListBody(h.Read),
	}
	return strings.Join(blocks, "\n\n") + "\n"
}

// handoffLine is the part of one transcript line the handoff reads.
type handoffLine struct {
	Type    string `json:"type"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// handoffBlock is one entry of a content array; a block that does not
// decode is skipped on its own.
type handoffBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

// handoffInput holds the tool input fields the handoff uses.
type handoffInput struct {
	Command      string `json:"command"`
	FilePath     string `json:"file_path"`
	NotebookPath string `json:"notebook_path"`
}

// handoffState accumulates the facts while the transcript is read in order.
type handoffState struct {
	h           Handoff
	answer      []string
	goalSet     bool
	bash        map[string]string // Bash tool_use id -> command
	changedSeen map[string]bool
	readSeen    map[string]bool
	read        []string
}

func newHandoffState() *handoffState {
	return &handoffState{
		bash:        map[string]string{},
		changedSeen: map[string]bool{},
		readSeen:    map[string]bool{},
	}
}

func (st *handoffState) line(raw []byte) {
	var l handoffLine
	if json.Unmarshal(raw, &l) != nil {
		return
	}
	switch l.Type {
	case "user":
		st.answer = nil
		st.user(l.Message.Content)
	case "assistant":
		st.assistant(l.Message.Content)
	}
}

func (st *handoffState) user(content json.RawMessage) {
	if s, ok := handoffString(content); ok {
		st.setGoal(s)
		return
	}
	var texts []string
	for _, b := range handoffBlocks(content) {
		switch b.Type {
		case "text":
			texts = append(texts, b.Text)
		case "tool_result":
			if b.IsError {
				st.failed(b)
			}
		}
	}
	if len(texts) > 0 {
		st.setGoal(strings.Join(texts, "\n"))
	}
}

func (st *handoffState) setGoal(goal string) {
	if !st.goalSet {
		st.h.Goal = goal
		st.goalSet = true
	}
}

func (st *handoffState) failed(b handoffBlock) {
	cmd, ok := st.bash[b.ToolUseID]
	if !ok {
		return
	}
	st.h.Failed = append(st.h.Failed, FailedCommand{Command: cmd, Error: handoffErrorLine(b.Content)})
	if n := len(st.h.Failed); n > handoffFailedKeep {
		st.h.Failed = st.h.Failed[n-handoffFailedKeep:]
	}
}

func (st *handoffState) assistant(content json.RawMessage) {
	if s, ok := handoffString(content); ok {
		st.answer = append(st.answer, s)
		return
	}
	for _, b := range handoffBlocks(content) {
		switch b.Type {
		case "text":
			st.answer = append(st.answer, b.Text)
		case "tool_use":
			st.toolUse(b)
		}
	}
}

func (st *handoffState) toolUse(b handoffBlock) {
	var in handoffInput
	_ = json.Unmarshal(b.Input, &in)
	switch b.Name {
	case "Bash":
		st.bash[b.ID] = in.Command
	case "Edit", "Write", "MultiEdit":
		st.changed(in.FilePath)
	case "NotebookEdit":
		st.changed(in.NotebookPath)
	case "Read":
		if in.FilePath != "" && !st.readSeen[in.FilePath] {
			st.readSeen[in.FilePath] = true
			st.read = append(st.read, in.FilePath)
		}
	}
}

func (st *handoffState) changed(path string) {
	if path == "" || st.changedSeen[path] {
		return
	}
	st.changedSeen[path] = true
	st.h.Changed = append(st.h.Changed, path)
}

func (st *handoffState) finish() Handoff {
	h := st.h
	h.Answer = strings.Join(st.answer, "\n")
	for _, p := range st.read {
		if !st.changedSeen[p] {
			h.Read = append(h.Read, p)
		}
	}
	return h
}

// handoffString is the content when it is a JSON string.
func handoffString(raw json.RawMessage) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// handoffBlocks are the blocks of a content array that decode; nil for any
// other content.
func handoffBlocks(raw json.RawMessage) []handoffBlock {
	var raws []json.RawMessage
	if json.Unmarshal(raw, &raws) != nil {
		return nil
	}
	var out []handoffBlock
	for _, r := range raws {
		var b handoffBlock
		if json.Unmarshal(r, &b) == nil {
			out = append(out, b)
		}
	}
	return out
}

// handoffErrorLine is the first non-empty line of a tool_result's content,
// trimmed; "" when it has none.
func handoffErrorLine(content json.RawMessage) string {
	text, ok := handoffString(content)
	if !ok {
		var parts []string
		for _, b := range handoffBlocks(content) {
			if b.Type == "text" {
				parts = append(parts, b.Text)
			}
		}
		text = strings.Join(parts, "\n")
	}
	for _, line := range strings.Split(text, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// handoffBody is text trimmed of surrounding newlines, cut to max lines.
func handoffBody(text string, limit int) string {
	t := strings.Trim(text, "\n")
	if strings.TrimSpace(t) == "" {
		return "(none)"
	}
	lines := strings.Split(t, "\n")
	if len(lines) <= limit {
		return t
	}
	return strings.Join(lines[:limit], "\n") + fmt.Sprintf("\n… (%d more lines)", len(lines)-limit)
}

func handoffFailedBody(failed []FailedCommand) string {
	if len(failed) == 0 {
		return "(none)"
	}
	var lines []string
	for _, f := range failed {
		cmd := handoffCut(strings.SplitN(f.Command, "\n", 2)[0], handoffCommandRunes)
		lines = append(lines, fmt.Sprintf("- `%s`: %s", cmd, handoffCut(f.Error, handoffErrorRunes)))
	}
	return strings.Join(lines, "\n")
}

func handoffListBody(files []string) string {
	if len(files) == 0 {
		return "(none)"
	}
	if len(files) > handoffListEntries {
		files = files[:handoffListEntries]
	}
	lines := make([]string, 0, len(files))
	for _, p := range files {
		lines = append(lines, "- "+p)
	}
	return strings.Join(lines, "\n")
}

// handoffCut keeps the first n runes of s.
func handoffCut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
