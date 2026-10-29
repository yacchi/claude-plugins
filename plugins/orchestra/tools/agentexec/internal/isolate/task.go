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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

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
		return "", fmt.Errorf("task id %q has no usable characters", raw)
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

// worktreesDirName is the state dir's directory of the work trees that
// belong to no request: <state dir>/worktrees/<RepoKey>/.
const worktreesDirName = "worktrees"

// Home is where root's work trees that belong to no request go:
// <state dir>/worktrees/<RepoKey(common)>, common being the real path of
// root's git common dir (root's own real path outside a repository). Every
// checkout and linked worktree of one repository shares it, it is never
// inside or beside a checkout, and refresh keeps its patch files in it.
func Home(root string) string {
	common, err := RepoCommon(root)
	if err != nil {
		common = paths.RealPath(root)
	}
	return filepath.Join(paths.StateDir(), worktreesDirName, RepoKey(common))
}

// RepoKey is the directory name, under <state dir>/worktrees, of the
// repository whose git common dir is common: a readable name, "-", and the
// first 8 hex digits of the SHA-256 of common, so two repositories with the
// same name never share a directory. The name is the base name of common's
// parent when common's base name is ".git", else common's base name without
// a ".git" suffix, sanitized like a task id; "repo" when nothing usable is
// left. It never touches the filesystem.
func RepoKey(common string) string {
	name := filepath.Base(common)
	if name == ".git" {
		name = filepath.Base(filepath.Dir(common))
	}
	name = strings.TrimSuffix(name, ".git")
	clean, err := SanitizeTaskID(name)
	if err != nil {
		clean = "repo"
	}
	sum := sha256.Sum256([]byte(common))
	return clean + "-" + hex.EncodeToString(sum[:4])
}

// WorktreeBases are the directories orchestra creates root's trees in:
// Home(root), then the request root when it resolves (a request's work
// trees live in its workspace). The herdr host sandbox makes them writable.
func WorktreeBases(root string) []string {
	bases := []string{Home(root)}
	if req, err := RequestRoot(); err == nil {
		bases = append(bases, req)
	}
	return bases
}

// isDir reports whether p is a directory (symlinks followed).
func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// lexists reports whether p exists, without following a final symlink.
func lexists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}
