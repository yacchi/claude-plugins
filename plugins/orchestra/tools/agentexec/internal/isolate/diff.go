package isolate

import (
	"fmt"
	"slices"
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
	// Added counts Files that are new relative to the baseline. Excluded
	// counts the files the ChangeFilter kept out of the change set and
	// ExcludedDirs names the matching entries (at most maxExcludedDirs);
	// both are measured only with a patch.
	Added        int
	Excluded     int
	ExcludedDirs []string
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
		if r.Added > 0 {
			o["added"] = r.Added
		}
		if r.Excluded > 0 {
			o["excluded"] = r.Excluded
		}
		if len(r.ExcludedDirs) > 0 {
			o["excluded_dirs"] = r.ExcludedDirs
		}
	}
	return fsutil.MarshalJSON(o)
}

// maxExcludedDirs caps DiffResult.ExcludedDirs.
const maxExcludedDirs = 5

// Diff reports task's changes in its worktree, filtered by CurrentFilter.
// withPatch=false skips the expensive `git diff --binary` (Patch stays "")
// and the excluded-file count: callers that only need to know whether
// something changed. session "" is the calling session.
func Diff(root, task, session string, withPatch bool) (DiffResult, error) {
	return DiffWith(root, task, session, withPatch, CurrentFilter())
}

// DiffWith is Diff with an explicit filter: excluded paths are never
// intent-added, listed or patched.
func DiffWith(root, task, session string, withPatch bool, f ChangeFilter) (DiffResult, error) {
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
	spec := f.Pathspec()
	// -N makes new files visible to `git diff` without staging their
	// content; ignored dirs (the carried node_modules) stay invisible, and
	// the excluded ones are never even walked into the index.
	_, _ = gitx.Run(wt.Path, append([]string{"add", "-A", "-N"}, spec...)...)
	// A diff git could not compute is an error, never "nothing changed":
	// remove's review gate would read empty files as clean.
	names, err := gitx.Run(wt.Path, append([]string{"diff", "--name-status", baseline}, spec...)...)
	if err != nil {
		return DiffResult{Status: "error", Reason: diffFailed(task, baseline, err)}, nil
	}
	files, added := []string{}, 0
	for _, line := range strings.Split(names, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		// "<status>\t<path>", or "<status>\t<old>\t<new>" for a rename or
		// copy: the path is the last field (a tab inside one is quoted).
		fields := strings.Split(line, "\t")
		files = append(files, fields[len(fields)-1])
		if fields[0] == "A" {
			added++
		}
	}
	res := DiffResult{
		Status: "ok", Task: task, Path: wt.Path, Baseline: baseline,
		Files: files, Added: added, Branch: wt.Branch, Session: branchSession(wt.Branch),
	}
	if withPatch {
		if res.Patch, err = gitx.Run(wt.Path, append([]string{"diff", "--binary", baseline}, spec...)...); err != nil {
			return DiffResult{Status: "error", Reason: diffFailed(task, baseline, err)}, nil
		}
		res.Excluded, res.ExcludedDirs = excludedFiles(wt.Path, baseline, f)
	}
	return res, nil
}

// excludedFiles counts the files under f's excluded directories that would
// otherwise have joined the change set (untracked, or changed since
// baseline) and names the entries they matched.
func excludedFiles(wt, baseline string, f ChangeFilter) (int, []string) {
	spec := f.includeSpec()
	if spec == nil {
		return 0, nil
	}
	var paths []string
	for _, args := range [][]string{
		append([]string{"ls-files", "--others", "--exclude-standard"}, spec...),
		append([]string{"diff", "--name-only", "--no-renames", baseline}, spec...),
	} {
		out, err := gitx.Run(wt, args...)
		if err != nil {
			continue
		}
		for _, p := range strings.Split(out, "\n") {
			if p = strings.TrimSpace(p); p != "" {
				paths = append(paths, p)
			}
		}
	}
	var dirs []string
	for _, p := range paths {
		if d, ok := f.match(p); ok && len(dirs) < maxExcludedDirs && !slices.Contains(dirs, d) {
			dirs = append(dirs, d)
		}
	}
	return len(paths), dirs
}

func diffFailed(task, baseline string, err error) string {
	return fmt.Sprintf("git diff against baseline %s failed for %s: %v", baseline, task, err)
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
