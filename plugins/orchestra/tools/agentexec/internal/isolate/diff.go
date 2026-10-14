package isolate

import (
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
)

// DiffResult is `isolate diff`'s output: what the worker changed, measured
// from the worktree's baseline.
type DiffResult struct {
	Status   string // ok | absent | error
	Reason   string // error only
	Task     string
	Path     string
	Baseline string
	Files    []string
	Patch    string
	Branch   string // "" for a detached adopted tree
	Session  string
	// OmitPatch drops the patch key from the JSON (`--names-only`).
	OmitPatch bool
}

func (r DiffResult) MarshalJSON() ([]byte, error) {
	o := map[string]any{"status": r.Status}
	switch r.Status {
	case "error":
		o["reason"] = r.Reason
	case "absent":
		o["task"] = r.Task
		o["files"] = nonNil(r.Files)
		if !r.OmitPatch {
			o["patch"] = r.Patch
		}
	default:
		o["task"] = r.Task
		o["path"] = r.Path
		o["baseline"] = r.Baseline
		o["files"] = nonNil(r.Files)
		if !r.OmitPatch {
			o["patch"] = r.Patch
		}
		o["branch"] = nullable(r.Branch)
		o["session"] = nullable(r.Session)
	}
	return fsutil.MarshalJSON(o)
}

// Diff reports task's changes in its worktree. withPatch=false skips the
// expensive `git diff --binary` (Patch stays ""): callers that only need to
// know whether something changed. session "" is the calling session.
func Diff(root, task, session string, withPatch bool) (DiffResult, error) {
	resolved, ok := gitx.RepoRoot(root)
	if !ok {
		return DiffResult{Status: "error", Reason: "not a git repository"}, nil
	}
	task, err := SanitizeTaskID(task)
	if err != nil {
		return DiffResult{}, err
	}
	wt, found, err := resolveWorktree(resolved, task, session)
	if err != nil {
		return DiffResult{}, err
	}
	if !found {
		return DiffResult{Status: "absent", Task: task, Files: []string{}}, nil
	}
	baseline, ok := ReadBaseline(wt.Path)
	if !ok {
		return DiffResult{Status: "error", Reason: "no baseline recorded for " + task}, nil
	}
	// -N makes new files visible to `git diff` without staging their
	// content; ignored dirs (the carried node_modules) stay invisible.
	_, _ = gitx.Run(wt.Path, "add", "-A", "-N")
	files := []string{}
	if names, err := gitx.Run(wt.Path, "diff", "--name-only", baseline); err == nil {
		for _, f := range strings.Split(names, "\n") {
			if strings.TrimSpace(f) != "" {
				files = append(files, f)
			}
		}
	}
	patch := ""
	if withPatch {
		if out, err := gitx.Run(wt.Path, "diff", "--binary", baseline); err == nil {
			patch = out
		}
	}
	return DiffResult{
		Status: "ok", Task: task, Path: wt.Path, Baseline: baseline,
		Files: files, Patch: patch, Branch: wt.Branch, Session: branchSession(wt.Branch),
	}, nil
}

// DiffAndCollect is `isolate diff`: the diff, after which the content it
// reported counts as collected (remove's refusal tells the caller to
// "collect the diff", so a real diff marks it done).
func DiffAndCollect(root, task string) (DiffResult, error) {
	res, err := Diff(root, task, "", true)
	if err == nil && res.Status == "ok" {
		_, _ = MarkCollected(res.Path, res.Files, res.Patch)
	}
	return res, err
}
