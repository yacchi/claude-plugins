// Package transcript reads the facts agent-exec needs from Claude Code
// transcripts (where a session's JSONL lives and how much context its last
// response carried) and keeps the index of sessions hosted for a run. It is
// a leaf so the hook guard, the wave runner and the usage report share one
// definition of those facts.
package transcript

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

var (
	// idPattern bounds session and agent ids to a single path element, so
	// an id taken from a caller can never walk out of a project root.
	idPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
	// runPattern bounds run ids to a file-name-safe token.
	runPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	// uuidPattern is the canonical lowercase form Claude Code writes.
	uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// Roots are the existing Claude project roots: <configDir>/projects first
// (configDir "" = $CLAUDE_CONFIG_DIR), then <home>/.claude/projects (home ""
// = $HOME, else os.UserHomeDir), de-duplicated by real path, keeping only
// existing directories. A root reached through two spellings is listed once
// so a transcript is never found twice.
func Roots(home, configDir string) []string {
	if configDir == "" {
		configDir = os.Getenv("CLAUDE_CONFIG_DIR")
	}
	var candidates []string
	if configDir != "" {
		candidates = append(candidates, filepath.Join(configDir, "projects"))
	}
	candidates = append(candidates, filepath.Join(homeDir(home), ".claude", "projects"))
	var roots []string
	seen := map[string]bool{}
	for _, root := range candidates {
		real := paths.RealPath(root)
		if seen[real] || !isDir(root) {
			continue
		}
		seen[real] = true
		roots = append(roots, root)
	}
	return roots
}

// Find is the transcript of session sessionID (agentID == ""): the first
// <root>/<project>/<sessionID>.jsonl; or of its subagent agentID: the first
// agent-<agentID>.jsonl anywhere below <root>/<project>/<sessionID>/subagents/,
// nested workflow directories included. Symlinked directories are not
// entered, so a transcript outside the roots is never picked up. ok is false
// when none exists or an id is malformed.
func Find(roots []string, sessionID, agentID string) (path string, ok bool) {
	if !validID(sessionID) || (agentID != "" && !validID(agentID)) {
		return "", false
	}
	for _, root := range roots {
		for _, project := range projectDirs(root) {
			if agentID == "" {
				file := filepath.Join(root, project, sessionID+".jsonl")
				if isFile(file) {
					return file, true
				}
				continue
			}
			subagents := filepath.Join(root, project, sessionID, "subagents")
			if file, found := findAgent(subagents, "agent-"+agentID+".jsonl"); found {
				return file, true
			}
		}
	}
	return "", false
}

// LastContext is the context the last response of the transcript at path
// carried; see LastUsage. ok is false when no line has usage or the file
// cannot be read.
func LastContext(path string) (tokens int64, ok bool) {
	tokens, _, ok = LastUsage(path)
	return tokens, ok
}

// LastUsage is the context and the model of the last line of the transcript
// at path whose message.usage exists: tokens is input_tokens +
// cache_read_input_tokens + cache_creation_input_tokens. The last response
// alone is the context the next turn resumes from, so earlier responses are
// not summed; its message.model (may be "") picks the model's resume limit.
// Lines are read whole, however long, because a single tool result can run
// to megabytes. ok is false when no line has usage or the file cannot be
// read.
func LastUsage(path string) (tokens int64, model string, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", false
	}
	defer f.Close()
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			if t, m, found := lineUsage(line); found {
				tokens, model, ok = t, m, true
			}
		}
		if err == io.EOF {
			return tokens, model, ok
		}
		if err != nil {
			return 0, "", false
		}
	}
}

// NewSessionID is a random RFC 4122 version-4 UUID (lowercase), the form
// `claude --session-id` accepts. The caller picks the id, so the hosted
// session can be recorded before Claude Code has written any transcript.
func NewSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand does not fail on the supported platforms
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// RecordHosted appends sessionID to the hosted-session index of runID,
// <paths.StateDir()>/hosted/<runID>.txt, one id per line (dirs 0o755, file
// 0o644, O_APPEND). It does nothing for a runID not matching
// ^[A-Za-z0-9_-]{1,64}$ or a sessionID that is not a UUID, and never fails
// the caller: the index only lets later steps find the run's sessions, so a
// failed write must not stop the hosted worker.
func RecordHosted(runID, sessionID string) {
	if !runPattern.MatchString(runID) || !uuidPattern.MatchString(sessionID) {
		return
	}
	dir := filepath.Join(paths.StateDir(), "hosted")
	if err := fsutil.EnsureDir(dir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(hostedPath(runID), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return
	}
	_, _ = f.WriteString(sessionID + "\n")
	_ = f.Close()
}

// HostedSessions are the distinct valid UUID lines recorded for runID, in
// first-recorded order; nil when there are none or runID is invalid. A
// session recorded twice is reported once, so callers can count the run's
// sessions without de-duplicating.
func HostedSessions(runID string) []string {
	if !runPattern.MatchString(runID) {
		return nil
	}
	data, err := os.ReadFile(hostedPath(runID))
	if err != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		id := strings.TrimSpace(line)
		if !uuidPattern.MatchString(id) || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func hostedPath(runID string) string {
	return filepath.Join(paths.StateDir(), "hosted", runID+".txt")
}

// lineUsage returns the context sum and message.model of one transcript line,
// if it carries message.usage. Lines without the word "usage" are skipped before decoding,
// since most transcript lines are large and carry no usage at all.
func lineUsage(line []byte) (int64, string, bool) {
	if !bytes.Contains(line, []byte(`"usage"`)) {
		return 0, "", false
	}
	var entry struct {
		Message struct {
			Model string `json:"model"`
			Usage *struct {
				Input       int64 `json:"input_tokens"`
				CacheRead   int64 `json:"cache_read_input_tokens"`
				CacheCreate int64 `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &entry); err != nil || entry.Message.Usage == nil {
		return 0, "", false
	}
	u := entry.Message.Usage
	return u.Input + u.CacheRead + u.CacheCreate, entry.Message.Model, true
}

// findAgent is the first file named name below dir, in lexical walk order.
// WalkDir does not follow symlinks, so a symlinked subagents directory or
// workflow directory is not entered.
func findAgent(dir, name string) (string, bool) {
	var found string
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fs.SkipDir
		}
		if !d.IsDir() && d.Name() == name {
			found = path
			return fs.SkipAll
		}
		return nil
	})
	return found, found != ""
}

// projectDirs are the real project directories directly under root, in
// lexical order. A symlinked project is skipped, like any symlinked
// directory.
func projectDirs(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

func validID(id string) bool {
	return idPattern.MatchString(id) && id != "." && id != ".."
}

func homeDir(home string) string {
	if home != "" {
		return home
	}
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return h
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

func isFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}
