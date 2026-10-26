package isolate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
)

// landedFile: the integration tree's tip as last landed, so remove can
// reclaim a tree whose squash no longer matches any branch's content once
// the target moved on. Kept in the tree's git dir like the other markers.
const landedFile = "orchestra-landed"

// landOwnSubject matches the subjects orchestra writes on integration
// branches; they must never reach the user's history.
var landOwnSubject = regexp.MustCompile(`(?i)^orchestra (integrate|revert|baseline)\b`)

const landOwnReason = "write a commit message in the repository's convention; " +
	"orchestra's own subjects stay on the integration branch"

// LandResult is `isolate land`'s output.
type LandResult struct {
	Status string // landed | empty | dirty | conflict | refused | absent | error
	Task   string
	Path   string   // the --path source; "" for --task
	Branch string   // the target branch, short name
	Commit string   // landed: the new commit
	Base   string   // landed: the target branch tip it was made on
	Files  []string // landed: changed paths; dirty: the dirty paths; conflict: conflicted paths; refused: offending paths (may be empty)
	Reason string   // every status except landed and empty
}

// MarshalJSON always writes "status" and "task", and "path" for a --path
// source; landed adds "branch", "commit", "base" and "files"; empty adds
// "branch"; the others add "reason" and, when non-empty, "files".
func (r LandResult) MarshalJSON() ([]byte, error) {
	o := map[string]any{"status": r.Status, "task": r.Task}
	if r.Path != "" {
		o["path"] = r.Path
	}
	switch r.Status {
	case "landed":
		o["branch"] = r.Branch
		o["commit"] = r.Commit
		o["base"] = r.Base
		o["files"] = nonNil(r.Files)
	case "empty":
		o["branch"] = r.Branch
	default:
		o["reason"] = r.Reason
		if len(r.Files) > 0 {
			o["files"] = r.Files
		}
	}
	return fsutil.MarshalJSON(o)
}

// Land squashes task's integration branch (baseline..HEAD of its worktree)
// into one commit on the branch checked out at root, authored by the
// repository's own identity with message as written: the per-package
// `orchestra integrate` commits stay on the integration branch, where the
// runner, bisect and fix-ups read them. message is the full commit message
// (subject, optional blank line + body). A non-nil error is unusable input
// only (an unusable task id, an ambiguous task, an empty message), as
// everywhere in this package.
func Land(root, task, message string) (LandResult, error) {
	return landWith(root, task, message, CurrentFilter())
}

func landWith(root, task, message string, f ChangeFilter) (LandResult, error) {
	resolved, ok := gitx.RepoRoot(root)
	if !ok {
		return LandResult{Status: "error", Reason: "not a git repository"}, nil
	}
	task, err := SanitizeTaskID(task)
	if err != nil {
		return LandResult{}, err
	}
	if strings.TrimSpace(message) == "" {
		return LandResult{}, errors.New("land needs a commit message")
	}
	if landOwnMessage(message) {
		return LandResult{Status: "refused", Task: task, Reason: landOwnReason}, nil
	}
	wt, found, err := resolveWorktree(resolved, task, "")
	if err != nil {
		return LandResult{}, err
	}
	if !found {
		return LandResult{Status: "absent", Task: task}, nil
	}
	fail := func(reason string) (LandResult, error) {
		return LandResult{Status: "error", Task: task, Reason: reason}, nil
	}

	branch, reason := landBranch(resolved)
	if reason != "" {
		return LandResult{Status: "refused", Task: task, Branch: branch, Reason: reason}, nil
	}

	pending, err := uncommittedWith(wt.Path, f)
	if err != nil {
		return fail(fmt.Sprintf("cannot list the integration tree's uncommitted changes: %v", err))
	}
	if len(pending) > 0 {
		return LandResult{Status: "dirty", Task: task, Branch: branch, Files: pending,
			Reason: "the integration tree has uncommitted changes that would not be landed"}, nil
	}

	baseline := readMarker(wt.Path, baselineFile)
	if baseline == "" {
		return fail("the integration tree has no baseline marker")
	}
	tip, err := landRevParse(wt.Path, "HEAD")
	if err != nil {
		return fail(fmt.Sprintf("cannot read the integration tree's HEAD: %v", err))
	}

	res, err := landOnto(resolved, task, branch, baseline, tip, message, f)
	if err != nil || res.Status != "landed" {
		return res, err
	}
	if err := writeMarker(wt.Path, landedFile, tip); err != nil {
		return fail(fmt.Sprintf("landed %s, but could not record it in the integration tree: %v", res.Commit, err))
	}
	return res, nil
}

// LandPath lands the Agent-tool worktree at path (any linked worktree of
// the repository other than the checkout): its whole content, committed and
// uncommitted, measured from its fork point with the checkout or from
// baseline, and squashed the way Land squashes an integration branch.
func LandPath(root, path, baseline, message string) (LandResult, error) {
	return landPathWith(root, path, baseline, message, CurrentFilter())
}

func landPathWith(root, path, baseline, message string, f ChangeFilter) (LandResult, error) {
	resolved, ok := gitx.RepoRoot(root)
	if !ok {
		return LandResult{Status: "error", Reason: "not a git repository"}, nil
	}
	if strings.TrimSpace(message) == "" {
		return LandResult{}, errors.New("land needs a commit message")
	}
	if landOwnMessage(message) {
		return LandResult{Status: "refused", Reason: landOwnReason}, nil
	}
	branch, reason := landBranch(resolved)
	if reason != "" {
		return LandResult{Status: "refused", Branch: branch, Reason: reason}, nil
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return LandResult{Status: "absent", Path: path,
			Reason: fmt.Sprintf("%s is not a linked worktree of this repository", path)}, nil
	}
	real := realPath(abs)
	res, err := landPathSource(resolved, branch, real, baseline, message, f)
	if err != nil {
		return res, err
	}
	res.Path = real
	return res, nil
}

// landPathSource lands the linked worktree at real (symlinks resolved) onto
// branch, the branch checked out at resolved.
func landPathSource(resolved, branch, real, baseline, message string, f ChangeFilter) (LandResult, error) {
	fail := func(reason string) (LandResult, error) {
		return LandResult{Status: "error", Reason: reason}, nil
	}
	if real == realPath(resolved) {
		return LandResult{Status: "absent", Reason: fmt.Sprintf("%s is the checkout itself", real)}, nil
	}
	wts, err := gitx.WorktreeList(resolved)
	if err != nil {
		return fail(fmt.Sprintf("cannot list the worktrees: %v", err))
	}
	// The first entry is the main worktree, which is never a linked one.
	var wt gitx.Worktree
	found := false
	for _, w := range wts[min(1, len(wts)):] {
		if realPath(w.Path) == real {
			wt, found = w, true
			break
		}
	}
	if !found {
		return LandResult{Status: "absent", Reason: fmt.Sprintf("%s is not a linked worktree of this repository", real)}, nil
	}
	_, adopted := ReadAdopted(wt.Path)
	if strings.HasPrefix(wt.Branch, BranchPrefix) || adopted || readMarker(wt.Path, baselineFile) != "" {
		return LandResult{Status: "refused", Branch: branch,
			Reason: fmt.Sprintf("%s is orchestra's tree: land it with --task", real)}, nil
	}

	target, err := landRevParse(resolved, "HEAD")
	if err != nil {
		return fail(fmt.Sprintf("cannot read %s's tip: %v", branch, err))
	}
	head, err := landRevParse(wt.Path, "HEAD")
	if err != nil {
		return fail(fmt.Sprintf("cannot read %s's HEAD: %v", real, err))
	}
	base := ""
	if baseline != "" {
		if base, err = landRevParse(resolved, baseline+"^{commit}"); err != nil {
			return LandResult{Status: "refused", Branch: branch,
				Reason: fmt.Sprintf("--baseline %s is not a commit", baseline)}, nil
		}
	} else {
		out, err := gitx.Run(resolved, "merge-base", target, head)
		if err != nil {
			return LandResult{Status: "refused", Branch: branch,
				Reason: fmt.Sprintf("no common history between the checkout and %s", real)}, nil
		}
		base = strings.TrimSpace(out)
	}

	tip, err := landSnapshot(wt.Path, head, f)
	if err != nil {
		return fail(fmt.Sprintf("cannot snapshot %s: %v", real, err))
	}
	return landOnto(resolved, "", branch, base, tip, message, f)
}

// landSnapshot is the content of the worktree at path, committed and
// uncommitted minus the excluded paths, as a commit on head that no ref
// points to (or head itself when the content is head's tree). The worktree's
// own index is never touched: git works on a copy of it, which keeps git's
// stat cache.
func landSnapshot(path, head string, f ChangeFilter) (string, error) {
	gitIdx, err := gitx.Run(path, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return "", err
	}
	idx := strings.TrimSpace(gitIdx)
	snap := idx + ".orchestra-land"
	b, err := os.ReadFile(idx)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(snap, b, 0o600); err != nil {
		return "", err
	}
	defer os.Remove(snap)

	env := append(os.Environ(), "GIT_INDEX_FILE="+snap)
	if _, err := gitx.RunEnv(path, env, append([]string{"add", "-A"}, f.Pathspec()...)...); err != nil {
		return "", err
	}
	out, err := gitx.RunEnv(path, env, "write-tree")
	if err != nil {
		return "", err
	}
	tree := strings.TrimSpace(out)
	headTree, err := landRevParse(path, head+"^{tree}")
	if err != nil {
		return "", err
	}
	if tree == headTree {
		return head, nil
	}
	out, err = gitx.Run(path, "-c", "user.name=orchestra", "-c", "user.email=orchestra@local",
		"commit-tree", tree, "-p", head, "-m", "orchestra land snapshot")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// landOnto lands the change from base to tip as one commit on branch, the
// branch checked out at resolved: the squash of base..tip with the
// repository's own identity and message, fast-forwarded in. task is "" for a
// --path source.
func landOnto(resolved, task, branch, base, tip, message string, f ChangeFilter) (LandResult, error) {
	fail := func(reason string) (LandResult, error) {
		return LandResult{Status: "error", Task: task, Reason: reason}, nil
	}

	out, err := gitx.Run(resolved, "diff", "--name-status", "--no-renames", "-z", base, tip)
	if err != nil {
		return fail(fmt.Sprintf("cannot list the integrated changes: %v", err))
	}
	changed, added := landChanges(out)
	if len(changed) == 0 {
		return LandResult{Status: "empty", Task: task, Branch: branch}, nil
	}
	excluded := []string{}
	for _, p := range changed {
		if f.Excluded(p) {
			excluded = append(excluded, p)
		}
	}
	if len(excluded) > 0 {
		return LandResult{Status: "refused", Task: task, Branch: branch, Files: excluded,
			Reason: "the integration branch commits paths excluded by isolation.exclude_paths; " +
				"remove them from the integration branch before landing"}, nil
	}
	if f.MaxAdded > 0 && added > f.MaxAdded {
		return LandResult{Status: "refused", Task: task, Branch: branch,
			Reason: fmt.Sprintf("adds %d new files (isolation.max_added_files %d)", added, f.MaxAdded)}, nil
	}

	out, err = gitx.Run(resolved, "status", "--porcelain=v1", "-z", "--untracked-files=no", "--no-renames")
	if err != nil {
		return fail(fmt.Sprintf("cannot read the checkout's status: %v", err))
	}
	if dirty := landStatusPaths(out); len(dirty) > 0 {
		return LandResult{Status: "dirty", Task: task, Branch: branch, Files: dirty,
			Reason: "the checkout has uncommitted changes to tracked files; commit or set them aside first"}, nil
	}

	target, err := landRevParse(resolved, "HEAD")
	if err != nil {
		return fail(fmt.Sprintf("cannot read %s's tip: %v", branch, err))
	}
	if _, err := gitx.Run(resolved, "merge-base", "--is-ancestor", base, target); err != nil {
		return LandResult{Status: "refused", Task: task, Branch: branch,
			Reason: fmt.Sprintf("%s no longer contains the run's base %s", branch, base[:min(8, len(base))])}, nil
	}

	var tree string
	if target == base {
		if tree, err = landRevParse(resolved, tip+"^{tree}"); err != nil {
			return fail(fmt.Sprintf("cannot read the integrated tree: %v", err))
		}
	} else {
		out, err := gitx.Run(resolved, "merge-tree", "--write-tree", "--name-only", "--no-messages", "-z",
			"--merge-base="+base, target, tip)
		var ee *gitx.ExitError
		switch {
		case err == nil:
			tree, _, _ = strings.Cut(out, "\x00")
			tree = strings.TrimSpace(tree)
		case errors.As(err, &ee) && ee.Code == 1:
			_, rest, _ := strings.Cut(out, "\x00")
			files := landSplitZ(rest)
			slices.Sort(files)
			return LandResult{Status: "conflict", Task: task, Branch: branch, Files: slices.Compact(files),
				Reason: "the branch moved since the run started and the changes conflict; land by hand"}, nil
		default:
			return fail(fmt.Sprintf("git merge-tree failed (git 2.40 or later is needed): %v", err))
		}
	}

	// The repository's own identity: no -c user.* overrides, not CommitAll.
	out, err = gitx.RunInput(resolved, []byte(message), "commit-tree", tree, "-p", target, "-F", "-")
	if err != nil {
		return fail(fmt.Sprintf("git commit-tree failed: %v", err))
	}
	commit := strings.TrimSpace(out)
	if _, err := gitx.Run(resolved, "merge", "--ff-only", "-q", commit); err != nil {
		return fail(fmt.Sprintf("could not fast-forward %s to the landed commit %s: %v", branch, commit, err))
	}
	slices.Sort(changed)
	return LandResult{Status: "landed", Task: task, Branch: branch, Commit: commit, Base: target, Files: changed}, nil
}

// landOwnMessage reports whether message's subject is one orchestra writes.
func landOwnMessage(message string) bool {
	subject, _, _ := strings.Cut(message, "\n")
	return landOwnSubject.MatchString(strings.TrimSpace(subject))
}

// landBranch is the branch checked out at resolved, the one a land lands on;
// a non-empty reason refuses it.
func landBranch(resolved string) (string, string) {
	out, err := gitx.Run(resolved, "symbolic-ref", "--short", "-q", "HEAD")
	branch := strings.TrimSpace(out)
	if err != nil || branch == "" {
		return "", fmt.Sprintf("the checkout at %s has no branch checked out", resolved)
	}
	if strings.HasPrefix(branch, BranchPrefix) {
		return branch, fmt.Sprintf("run land from the checkout of the branch to land on, not from an orchestra worktree (%s)", branch)
	}
	return branch, ""
}

// realPath is p with its symlinks resolved, or p itself when that fails.
func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func landRevParse(dir, rev string) (string, error) {
	out, err := gitx.Run(dir, "rev-parse", "--verify", "-q", rev)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// landChanges is the paths of `git diff --name-status --no-renames -z`
// output, and how many of them were added.
func landChanges(out string) ([]string, int) {
	fields := landSplitZ(out)
	files := []string{}
	added := 0
	for i := 0; i+1 < len(fields); i += 2 {
		if fields[i] == "A" {
			added++
		}
		files = append(files, fields[i+1])
	}
	return files, added
}

// landSplitZ splits NUL-separated git output, dropping empty fields.
func landSplitZ(out string) []string {
	files := []string{}
	for _, f := range strings.Split(out, "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files
}

// landStatusPaths is the paths of `git status --porcelain=v1 -z
// --no-renames` output ("XY path" per entry).
func landStatusPaths(out string) []string {
	files := []string{}
	for _, e := range landSplitZ(out) {
		if len(e) > 3 {
			files = append(files, e[3:])
		}
	}
	return files
}
