package isolate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
)

// Marker files kept in a worktree's own git dir (never in the tree, so no
// diff ever shows them). The names are the on-disk contract shared with the
// Python implementation.
const (
	// baselineFile: the commit the worker started from; every diff is
	// measured against it.
	baselineFile = "orchestra-baseline"
	// collectedFile: digest of the changes as last collected (diff,
	// integrate or collect), which is what lets remove reclaim the tree.
	collectedFile = "orchestra-collected"
	// createdIgnoredFile: digest of the ignored-entry surface as create left
	// it (after carried dependency dirs landed), so remove can tell what
	// orchestra put there from what the worker added.
	createdIgnoredFile = "orchestra-created-ignored"
	// roleFile: "integration" for an integrate target, whose ignored
	// surface is expected to change across rounds.
	roleFile = "orchestra-role"
	// adoptedFile: JSON {"task","session","adopted_at"} on a worktree
	// orchestra did not create but was told to treat as a task's.
	adoptedFile = "orchestra-adopted"
)

// RoleIntegration marks an `isolate integrate` target worktree.
const RoleIntegration = "integration"

var errNoGitDir = errors.New("not a git worktree")

func markerPath(wt, name string) (string, error) {
	dir, err := gitx.GitDir(wt)
	if err != nil {
		return "", errNoGitDir
	}
	return filepath.Join(dir, name), nil
}

func writeMarker(wt, name, value string) error {
	p, err := markerPath(wt, name)
	if err != nil {
		return err
	}
	return os.WriteFile(p, []byte(value+"\n"), 0o644)
}

// readMarker is the stripped content of a marker, "" when absent, empty or
// unreadable (all of which mean "no marker").
func readMarker(wt, name string) string {
	p, err := markerPath(wt, name)
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func removeMarkers(wt string, names ...string) error {
	dir, err := gitx.GitDir(wt)
	if err != nil {
		return errNoGitDir
	}
	for _, name := range names {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// WriteBaseline records sha as the worktree's baseline.
func WriteBaseline(wt, sha string) error { return writeMarker(wt, baselineFile, sha) }

// ReadBaseline is the recorded baseline, falling back to the branch tip
// (correct as long as nobody committed, and workers must not commit).
func ReadBaseline(wt string) (string, bool) {
	if sha := readMarker(wt, baselineFile); sha != "" {
		return sha, true
	}
	out, err := gitx.Run(wt, "rev-parse", "HEAD")
	if err != nil {
		return "", false
	}
	sha := strings.TrimSpace(out)
	return sha, sha != ""
}

// WriteRole records the worktree's role (RoleIntegration).
func WriteRole(wt, role string) error { return writeMarker(wt, roleFile, role) }

// ReadRole is the recorded role, "" for an ordinary task worktree.
func ReadRole(wt string) string { return readMarker(wt, roleFile) }

// WriteCollected records digest as the worktree's collected state.
func WriteCollected(wt, digest string) error { return writeMarker(wt, collectedFile, digest) }

// ClearCollected forgets that the worktree's changes were collected.
func ClearCollected(wt string) error { return removeMarkers(wt, collectedFile) }

func readCollected(wt string) string { return readMarker(wt, collectedFile) }

func writeCreatedIgnored(wt, digest string) error {
	return writeMarker(wt, createdIgnoredFile, digest)
}

func readCreatedIgnored(wt string) string { return readMarker(wt, createdIgnoredFile) }

// adoption is the content of the adopted marker.
type adoption struct {
	Task    string
	Session string // 8-character component, "" when none was recorded
}

func readAdoption(wt string) (adoption, bool) {
	p, err := markerPath(wt, adoptedFile)
	if err != nil {
		return adoption{}, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return adoption{}, false
	}
	var rec map[string]any
	if json.Unmarshal(b, &rec) != nil {
		return adoption{}, false
	}
	task, ok := rec["task"].(string)
	if !ok {
		return adoption{}, false
	}
	session, _ := rec["session"].(string)
	return adoption{Task: task, Session: sessionComponent(session)}, true
}

// writeAdoption writes the adopted marker for task on behalf of session
// ("" = the calling session).
func writeAdoption(wt, task, session string) error {
	p, err := markerPath(wt, adoptedFile)
	if err != nil {
		return err
	}
	var sess any
	if c := sessionComponent(orCurrent(session)); c != "" {
		sess = c
	}
	rec := map[string]any{
		"task":       task,
		"session":    sess,
		"adopted_at": float64(time.Now().UnixNano()) / 1e9,
	}
	b, err := fsutil.MarshalJSON(rec)
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(b, '\n'), 0o644)
}

// ReadAdopted is the task an adopted worktree was registered as.
func ReadAdopted(wt string) (string, bool) {
	a, ok := readAdoption(wt)
	return a.Task, ok
}

// WriteAdopted registers wt as task's worktree for the calling session.
func WriteAdopted(wt, task string) error { return writeAdoption(wt, task, "") }

// ClearAdopted drops the adoption together with the baseline and collected
// markers. Files and the worktree itself are never touched.
func ClearAdopted(wt string) error {
	return removeMarkers(wt, adoptedFile, baselineFile, collectedFile)
}

// IgnoredEntries are the gitignored paths of wt in git's traditional
// --ignored mode, which collapses a wholly-ignored directory to one entry
// ("node_modules/"): stable across installs and small however large the
// directory is. Sorted.
func IgnoredEntries(wt string) []string {
	out, err := gitx.Run(wt, "status", "--porcelain", "--ignored")
	if err != nil {
		return nil
	}
	var entries []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "!! ") {
			entries = append(entries, strings.TrimSpace(line[3:]))
		}
	}
	sort.Strings(entries)
	return entries
}

// ignoredForDigest is the ignored surface folded into a collected digest:
// none for an integration worktree, whose ignored surface changes routinely.
func ignoredForDigest(wt string) []string {
	if ReadRole(wt) == RoleIntegration {
		return nil
	}
	return IgnoredEntries(wt)
}

// CollectedDigest is the content digest of a task's changes: the changed
// file list, the patch text and the ignored entries. Content- rather than
// event-based, so collection and removal reproduce it from the same inputs.
func CollectedDigest(files []string, patch string, ignored []string) string {
	payload := strings.Join(sortedCopy(files), "\x00") + "\x00" + patch + "\x00" +
		strings.Join(sortedCopy(ignored), "\x00")
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

// MarkCollected records the worktree's current changes (files and patch as
// a diff of it reported them) as collected.
func MarkCollected(wt string, files []string, patch string) (string, error) {
	digest := CollectedDigest(files, patch, ignoredForDigest(wt))
	return digest, WriteCollected(wt, digest)
}

func ignoredDigest(entries []string) string {
	sum := sha256.Sum256([]byte(strings.Join(sortedCopy(entries), "\x00")))
	return hex.EncodeToString(sum[:])
}

func sortedCopy(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}
