package isolate

import (
	"fmt"
	"sort"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
)

// NullString renders as JSON null when empty (a session or branch that does
// not exist, as opposed to an absent key).
type NullString string

func (s NullString) MarshalJSON() ([]byte, error) {
	if s == "" {
		return []byte("null"), nil
	}
	return fsutil.MarshalJSON(string(s))
}

// Entry is one orchestra-created worktree as `isolate list` reports it.
type Entry struct {
	Task    string     `json:"task"`
	Branch  string     `json:"branch"`
	Path    string     `json:"path"`
	Session NullString `json:"session"`
	Current bool       `json:"current"`
}

// worktrees is `git worktree list` of root, empty when git fails.
func worktrees(root string) []gitx.Worktree {
	list, err := gitx.WorktreeList(root)
	if err != nil {
		return nil
	}
	return list
}

// List reports the orchestra worktrees of the repository containing root.
// "current" is relative to session ("" = the calling session); a non-empty
// session also filters the list to that session's trees.
func List(root, session string) ([]Entry, error) {
	resolved, ok := gitx.RepoRoot(root)
	if !ok {
		return []Entry{}, nil
	}
	current := sessionComponent(orCurrent(session))
	out := []Entry{}
	for _, wt := range worktrees(resolved) {
		if !strings.HasPrefix(wt.Branch, BranchPrefix) {
			continue
		}
		owner := branchSession(wt.Branch)
		out = append(out, Entry{
			Task:    branchTask(wt.Branch),
			Branch:  wt.Branch,
			Path:    wt.Path,
			Session: NullString(owner),
			Current: owner != "" && owner == current,
		})
	}
	if session == "" {
		return out, nil
	}
	filtered := []Entry{}
	for _, e := range out {
		if string(e.Session) == current {
			filtered = append(filtered, e)
		}
	}
	return filtered, nil
}

func worktreeForBranch(root, branch string) string {
	for _, wt := range worktrees(root) {
		if wt.Branch == branch {
			return wt.Path
		}
	}
	return ""
}

// ResolveWorktree finds task's worktree in the repository root: the calling
// (or given) session's own branch first, else a unique orchestra branch of
// any session (legacy session-less included), else a unique adopted tree.
// ok is false when there is none, or when the choice would be a guess.
func ResolveWorktree(root, task, session string) (path, branch string, ok bool) {
	wt, found, err := resolveWorktree(root, task, session)
	if err != nil || !found {
		return "", "", false
	}
	return wt.Path, wt.Branch, true
}

// resolveWorktree is ResolveWorktree reporting ambiguity as an error. task
// is sanitized; session "" means the calling session.
func resolveWorktree(root, task, session string) (gitx.Worktree, bool, error) {
	task, err := SanitizeTaskID(task)
	if err != nil {
		return gitx.Worktree{}, false, err
	}
	current := sessionComponent(orCurrent(session))
	preferred := ""
	if current != "" {
		preferred = branchName(task, current)
	}
	all := worktrees(root)
	var matches []gitx.Worktree
	for _, wt := range all {
		if wt.Branch == branchName(task, "") ||
			(strings.HasPrefix(wt.Branch, BranchPrefix) && strings.HasSuffix(wt.Branch, "/"+task)) {
			matches = append(matches, wt)
		}
	}
	for _, wt := range matches {
		if wt.Branch == preferred {
			return wt, true, nil
		}
	}
	if len(matches) > 1 {
		names := make([]string, len(matches))
		for i, wt := range matches {
			names[i] = wt.Branch
		}
		sort.Strings(names)
		return gitx.Worktree{}, false, fmt.Errorf("ambiguous worktrees for task %s: %s", task, strings.Join(names, ", "))
	}
	if len(matches) == 1 {
		return matches[0], true, nil
	}
	return resolveAdopted(all, task, current)
}

// resolveAdopted finds the adopted tree registered as task. A marker of the
// calling session wins when several trees carry the same task id; otherwise
// more than one is ambiguous.
func resolveAdopted(all []gitx.Worktree, task, current string) (gitx.Worktree, bool, error) {
	type match struct {
		wt      gitx.Worktree
		session string
	}
	var matches []match
	for _, wt := range all {
		if a, ok := readAdoption(wt.Path); ok && a.Task == task {
			matches = append(matches, match{wt, a.Session})
		}
	}
	for _, m := range matches {
		if current != "" && m.session == current {
			return m.wt, true, nil
		}
	}
	if len(matches) > 1 {
		names := make([]string, len(matches))
		for i, m := range matches {
			names[i] = m.wt.Path
		}
		sort.Strings(names)
		return gitx.Worktree{}, false, fmt.Errorf("ambiguous adopted worktrees for task %s: %s", task, strings.Join(names, ", "))
	}
	if len(matches) == 1 {
		return matches[0].wt, true, nil
	}
	return gitx.Worktree{}, false, nil
}
