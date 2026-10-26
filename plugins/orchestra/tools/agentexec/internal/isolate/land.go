package isolate

import (
	"errors"
	"fmt"
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

// LandResult is `isolate land`'s output.
type LandResult struct {
	Status string // landed | empty | dirty | conflict | refused | absent | error
	Task   string
	Branch string   // the target branch, short name
	Commit string   // landed: the new commit
	Base   string   // landed: the target branch tip it was made on
	Files  []string // landed: changed paths; dirty: the dirty paths; conflict: conflicted paths; refused: offending paths (may be empty)
	Reason string   // every status except landed and empty
}

// MarshalJSON always writes "status" and "task"; landed adds "branch",
// "commit", "base" and "files"; empty adds "branch"; the others add
// "reason" and, when non-empty, "files".
func (r LandResult) MarshalJSON() ([]byte, error) {
	o := map[string]any{"status": r.Status, "task": r.Task}
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
	subject, _, _ := strings.Cut(message, "\n")
	if landOwnSubject.MatchString(strings.TrimSpace(subject)) {
		return LandResult{Status: "refused", Task: task, Reason: "write a commit message in the repository's convention; " +
			"orchestra's own subjects stay on the integration branch"}, nil
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

	out, err := gitx.Run(resolved, "symbolic-ref", "--short", "-q", "HEAD")
	branch := strings.TrimSpace(out)
	if err != nil || branch == "" {
		return LandResult{Status: "refused", Task: task,
			Reason: fmt.Sprintf("the checkout at %s has no branch checked out", resolved)}, nil
	}
	if strings.HasPrefix(branch, BranchPrefix) {
		return LandResult{Status: "refused", Task: task, Branch: branch,
			Reason: fmt.Sprintf("run land from the checkout of the branch to land on, not from an orchestra worktree (%s)", branch)}, nil
	}

	filter := CurrentFilter()
	pending, err := uncommittedWith(wt.Path, filter)
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
	if tip == baseline {
		return LandResult{Status: "empty", Task: task, Branch: branch}, nil
	}

	out, err = gitx.Run(wt.Path, "diff", "--name-only", "--no-renames", "-z", baseline, tip)
	if err != nil {
		return fail(fmt.Sprintf("cannot list the integrated changes: %v", err))
	}
	changed := landSplitZ(out)
	excluded := []string{}
	for _, p := range changed {
		if filter.Excluded(p) {
			excluded = append(excluded, p)
		}
	}
	if len(excluded) > 0 {
		return LandResult{Status: "refused", Task: task, Branch: branch, Files: excluded,
			Reason: "the integration branch commits paths excluded by isolation.exclude_paths; " +
				"remove them from the integration branch before landing"}, nil
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
	if _, err := gitx.Run(resolved, "merge-base", "--is-ancestor", baseline, target); err != nil {
		return LandResult{Status: "refused", Task: task, Branch: branch,
			Reason: fmt.Sprintf("%s no longer contains the run's base %s", branch, baseline[:min(8, len(baseline))])}, nil
	}

	var tree string
	if target == baseline {
		if tree, err = landRevParse(resolved, tip+"^{tree}"); err != nil {
			return fail(fmt.Sprintf("cannot read the integrated tree: %v", err))
		}
	} else {
		out, err := gitx.Run(resolved, "merge-tree", "--write-tree", "--name-only", "--no-messages", "-z",
			"--merge-base="+baseline, target, tip)
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
	if err := writeMarker(wt.Path, landedFile, tip); err != nil {
		return fail(fmt.Sprintf("landed %s, but could not record it in the integration tree: %v", commit, err))
	}
	slices.Sort(changed)
	return LandResult{Status: "landed", Task: task, Branch: branch, Commit: commit, Base: target, Files: changed}, nil
}

func landRevParse(dir, rev string) (string, error) {
	out, err := gitx.Run(dir, "rev-parse", "--verify", "-q", rev)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
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
