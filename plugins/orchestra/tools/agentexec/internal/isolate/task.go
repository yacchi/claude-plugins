// Package isolate gives every task its own git worktree: create, list,
// diff, collect, remove and sweep, plus the markers kept in each worktree's
// git dir (baseline, collected digest, created-ignored digest, role,
// adoption) and the per-session heartbeat sweep uses for liveness.
//
// Failures that describe the repository (not a repository, a worktree that
// could not be made, a refusal to discard work) are reported in the result's
// Status, as the CLI prints them. A non-nil error always means the caller's
// input was unusable (a task id with no usable characters, an invalid
// session id, an ambiguous task): the CLI's usage-error exit.
package isolate

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// BranchPrefix namespaces every branch orchestra creates.
const BranchPrefix = "orchestra/"

var (
	reSeparators = regexp.MustCompile(`[/\\]+`)
	reUnsafe     = regexp.MustCompile(`[^A-Za-z0-9._-]`)
	reDashes     = regexp.MustCompile(`-{2,}`)
	reSession    = regexp.MustCompile(`^[a-z0-9]{8}$`)
)

// SanitizeTaskID turns a task id into something safe as both a branch name
// and a path: separators split it into segments, "."/".."/empty segments are
// dropped (no traversal survives), anything outside [A-Za-z0-9._-] becomes
// "-".
func SanitizeTaskID(raw string) (string, error) {
	var segments []string
	for _, seg := range reSeparators.Split(raw, -1) {
		seg = reUnsafe.ReplaceAllString(seg, "-")
		seg = strings.Trim(reDashes.ReplaceAllString(seg, "-"), "-")
		if seg == "" || seg == "." || seg == ".." {
			continue
		}
		segments = append(segments, seg)
	}
	out := strings.Trim(reDashes.ReplaceAllString(strings.Join(segments, "-"), "-"), "-.")
	if out == "" {
		return "", fmt.Errorf("task id %s has no usable characters", pyRepr(raw))
	}
	return out, nil
}

// sessionComponent is the 8-character branch segment of a session id, or ""
// when the id cannot provide one (too short, not alphanumeric).
func sessionComponent(session string) string {
	v := session
	if len([]rune(v)) > 8 {
		v = string([]rune(v)[:8])
	}
	v = strings.ToLower(v)
	if reSession.MatchString(v) {
		return v
	}
	return ""
}

// Branch is the branch name of task for session ("" or an unusable id
// gives the legacy session-less shape). task must already be sanitized or
// sanitizable; an unusable one yields "".
func Branch(task, session string) string {
	t, err := SanitizeTaskID(task)
	if err != nil {
		return ""
	}
	return branchName(t, sessionComponent(session))
}

func branchName(task, component string) string {
	if component == "" {
		return BranchPrefix + task
	}
	return BranchPrefix + component + "/" + task
}

// branchSession is the session segment of an orchestra branch, or "" for a
// legacy, adopted (external) or detached one.
func branchSession(branch string) string {
	if !strings.HasPrefix(branch, BranchPrefix) {
		return ""
	}
	first, _, ok := strings.Cut(strings.TrimPrefix(branch, BranchPrefix), "/")
	if ok && sessionComponent(first) == first {
		return first
	}
	return ""
}

// branchTask is the task part of an orchestra branch.
func branchTask(branch string) string {
	rest := strings.TrimPrefix(branch, BranchPrefix)
	if i := strings.Index(rest, "/"); i >= 0 {
		return rest[i+1:]
	}
	return rest
}

// currentSession is the calling Claude Code session's id ("" outside one).
func currentSession() string {
	v, _ := paths.SessionID()
	return v
}

// orCurrent is session, or the calling session's id when session is "".
func orCurrent(session string) string {
	if session == "" {
		return currentSession()
	}
	return session
}

// Home is where plain-git worktrees of root go: a sibling directory
// "<repo>-orchestra", never inside the repository (where they would show up
// as untracked noise in the very tree isolation keeps clean).
func Home(root string) string {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	return filepath.Join(filepath.Dir(abs), filepath.Base(abs)+"-orchestra")
}

// pyRepr renders s as Python's repr() of a str, for messages that quote
// user input the way the Python implementation did.
func pyRepr(s string) string {
	quote := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, "\"") {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == rune(quote):
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r > 0x7f && !unicode.IsPrint(r):
			if r > 0xffff {
				fmt.Fprintf(&b, `\U%08x`, r)
			} else if r > 0xff {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				fmt.Fprintf(&b, `\x%02x`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// isDir is os.path.isdir (symlinks followed).
func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// lexists is os.path.lexists.
func lexists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}
