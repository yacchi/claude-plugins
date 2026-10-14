package isolate

import (
	"fmt"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
)

// RemoveResult is `isolate remove`'s output, for one task or (IsSession,
// Removed set) for every tree of a session.
type RemoveResult struct {
	Status    string // removed | dirty | absent | unadopted | error
	Task      string
	Path      string
	Branch    string
	Session   string
	Reason    string
	Files     []string // dirty only
	IsSession bool
	Removed   []RemoveResult
}

func (r RemoveResult) MarshalJSON() ([]byte, error) {
	o := map[string]any{}
	switch {
	case r.IsSession:
		removed := r.Removed
		if removed == nil {
			removed = []RemoveResult{}
		}
		o["status"] = r.Status
		o["session"] = r.Session
		o["removed"] = removed
	case r.Status == "dirty":
		o["files"] = nonNil(r.Files)
		o["reason"] = r.Reason
		o["status"] = r.Status
		o["task"] = r.Task
		o["path"] = r.Path
	case r.Status == "error" && r.Task == "":
		o["status"] = r.Status
		o["reason"] = r.Reason
	case r.Status == "error":
		o["status"] = r.Status
		o["task"] = r.Task
		o["reason"] = r.Reason
	case r.Status == "absent":
		o["status"] = r.Status
		o["task"] = r.Task
	case r.Status == "unadopted":
		o["status"] = r.Status
		o["task"] = r.Task
		o["path"] = r.Path
	default:
		o["status"] = r.Status
		o["task"] = r.Task
		o["path"] = r.Path
		o["branch"] = nullable(r.Branch)
		o["session"] = nullable(r.Session)
	}
	return fsutil.MarshalJSON(o)
}

// uncollected says why the tree at path still holds unreviewed work, or
// returns ok=false when it is safe to discard. Shared by remove's refusal
// and sweep's dry-run preview so the two can never disagree.
func uncollected(root, path, task, session string) (files []string, reason string, dirty bool, err error) {
	// The patch is only needed to recompute a collected digest, which can
	// only match when a marker exists: the never-collected common case costs
	// one `git diff --name-only` instead of a full binary diff.
	stored := readCollected(path)
	diff, err := Diff(root, task, session, stored != "")
	if err != nil {
		return nil, "", false, err
	}
	files = nonNil(diff.Files)
	ignored := IgnoredEntries(path)
	matches := stored != "" && stored == CollectedDigest(files, diff.Patch, ignoredForDigest(path))
	trackedOK := len(files) == 0 || matches
	// Nearly every tree carries some ignored surface by design (the carried
	// node_modules); only a surface that differs from what create left
	// counts. An integration tree's surface changes routinely.
	created := readCreatedIgnored(path)
	ignoredOK := (created != "" && created == ignoredDigest(ignored)) || matches ||
		ReadRole(path) == RoleIntegration
	switch {
	case !trackedOK:
		return files, fmt.Sprintf("worktree holds %d changed file(s) not yet collected; "+
			"collect the diff or pass --force", len(files)), true, nil
	case !ignoredOK:
		return files, "worktree's ignored files changed since it was created " +
			"and were never collected; collect the diff or pass --force", true, nil
	}
	return nil, "", false, nil
}

// Remove deletes task's worktree and branch, refusing (status "dirty") to
// discard changes nobody collected unless force. An adopted tree is never
// deleted: only its adoption is dropped. session "" is the calling session.
func Remove(root, task string, force bool, session string) (RemoveResult, error) {
	resolved, ok := gitx.RepoRoot(root)
	if !ok {
		return RemoveResult{Status: "error", Reason: "not a git repository"}, nil
	}
	task, err := SanitizeTaskID(task)
	if err != nil {
		return RemoveResult{}, err
	}
	wt, found, err := resolveWorktree(resolved, task, session)
	if err != nil {
		return RemoveResult{}, err
	}
	if !found {
		return RemoveResult{Status: "absent", Task: task}, nil
	}
	if _, adopted := readAdoption(wt.Path); adopted {
		_ = ClearAdopted(wt.Path)
		return RemoveResult{Status: "unadopted", Task: task, Path: wt.Path}, nil
	}
	if !force {
		files, reason, dirty, err := uncollected(resolved, wt.Path, task, session)
		if err != nil {
			return RemoveResult{}, err
		}
		if dirty {
			return RemoveResult{Status: "dirty", Task: task, Path: wt.Path, Files: files, Reason: reason}, nil
		}
		// Vetted by the review gate above; `git worktree remove` would still
		// refuse untracked files without --force, a rule orthogonal to it.
	}
	if _, err := gitx.Run(resolved, "worktree", "remove", wt.Path, "--force"); err != nil {
		return RemoveResult{Status: "error", Task: task, Reason: "git worktree remove failed"}, nil
	}
	_, _ = gitx.Run(resolved, "branch", "-D", wt.Branch)
	return RemoveResult{
		Status: "removed", Task: task, Path: wt.Path, Branch: wt.Branch, Session: branchSession(wt.Branch),
	}, nil
}

// RemoveSession removes every worktree of session (the SessionEnd hook's
// `isolate remove --session S --repo R`); dirty ones are reported and kept.
func RemoveSession(root, session string, force bool) (RemoveResult, error) {
	wanted := sessionComponent(session)
	if wanted == "" {
		return RemoveResult{}, errInvalidSession
	}
	entries, _ := List(root, "")
	res := RemoveResult{Status: "removed", Session: wanted, IsSession: true, Removed: []RemoveResult{}}
	for _, e := range entries {
		if string(e.Session) != wanted {
			continue
		}
		one, err := Remove(root, e.Task, force, wanted)
		if err != nil {
			return RemoveResult{}, err
		}
		res.Removed = append(res.Removed, one)
	}
	return res, nil
}
