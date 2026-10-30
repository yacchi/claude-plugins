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
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// landedFile: the assembly's tip as last landed, so remove can reclaim an
// assembly whose squash no longer matches any branch's content once the
// target moved on. Kept in the tree's git dir like the other markers.
const landedFile = "orchestra-landed"

// landOwnSubject matches the subjects orchestra writes on integration
// branches; they must never reach the user's history.
var landOwnSubject = regexp.MustCompile(`(?i)^orchestra (integrate|revert|baseline)\b`)

const landOwnReason = "write a commit message in the repository's convention; " +
	"orchestra's own subjects stay on the integration branch"

// LandResult is `isolate land`'s output; the landed tree is always an
// assembly. An assembly is landed only once it has no children, and landing
// (or finding nothing to land) removes it.
type LandResult struct {
	Status       string // landed | empty | dirty | conflict | refused | absent | error | has-children
	Task         string
	Path         string   // the --path source; "" for --task
	Branch       string   // the target branch, short name
	Commit       string   // landed: the new commit
	Base         string   // landed: the target branch tip it was made on
	Files        []string // landed: changed paths; dirty: the dirty paths; conflict: conflicted paths; refused: offending paths (may be empty)
	Reason       string   // every status except landed and empty
	Children     []Child  // has-children only
	Request      string   // the request the assembly belonged to; "" when none
	Removed      bool     // landed/empty (--task): the assembly was removed afterwards
	RequestEnded bool     // landed/empty (--task): that removal ended the request
	Created      bool     // set when the checkout was left on a branch this land created (feature/<slug>); normally only with landed
}

// MarshalJSON always writes "status" and "task", and "path" for a --path
// source; landed adds "branch", "commit", "base" and "files", and "created"
// when it created the branch it landed on; empty adds "branch"; a --task
// landed or empty adds "removed", "request", "request_ended" and, when
// non-empty, "reason"; has-children adds "children"; the others add "reason"
// and, when non-empty, "files".
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
		if r.Created {
			o["created"] = true
		}
	case "empty":
		o["branch"] = r.Branch
	default:
		o["reason"] = r.Reason
		if len(r.Files) > 0 {
			o["files"] = r.Files
		}
	}
	if r.Path == "" && (r.Status == "landed" || r.Status == "empty") {
		var request any
		if r.Request != "" {
			request = r.Request
		}
		o["removed"] = r.Removed
		o["request"] = request
		o["request_ended"] = r.RequestEnded
		if r.Reason != "" {
			o["reason"] = r.Reason
		}
	}
	if r.Status == "has-children" {
		children := r.Children
		if children == nil {
			children = []Child{}
		}
		o["children"] = children
	}
	return fsutil.MarshalJSON(o)
}

// Land squashes task's assembly (baseline..HEAD of its worktree) into one
// commit on the branch checked out at root, authored by the repository's own
// identity with message as written. An assembly holds the organized product;
// integration trees reach it only through `isolate merge`, so any other tree
// is refused and changes nothing. An assembly with children is refused too,
// before anything is landed. Landing (or finding nothing to land) removes the
// assembly, and its request ends with its last assembly. message is the full
// commit message (subject, optional blank line + body). A non-nil error is
// unusable input only (an unusable task id, an ambiguous task, an empty
// message), as everywhere in this package.
func Land(root, task, message string) (LandResult, error) {
	return landWith(root, task, message, CurrentFilter())
}

// landWith is Land with the change filter f. The assembly is removed through
// Remove's own gate, never forced: a refusal there leaves it in place.
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

	if reason := landRoleReason(resolved, wt.Path, task); reason != "" {
		return LandResult{Status: "refused", Task: task, Reason: reason}, nil
	}
	// Before landBranch and the dirty checks: a refusal for children must not
	// depend on the checkout.
	kids, err := Children(resolved, wt.Path)
	if err != nil {
		return fail(fmt.Sprintf("cannot list the assembly's children (%v); nothing was landed", err))
	}
	if len(kids) > 0 {
		return LandResult{Status: "has-children", Task: task, Children: kids,
			Reason: fmt.Sprintf("assembly %s still has %d open integration tree(s); merge them with 'agent-exec isolate merge --task <into> -m <message>' or discard them, then land", task, len(kids))}, nil
	}

	branch, reason := landBranch(resolved)
	if reason != "" {
		return LandResult{Status: "refused", Task: task, Branch: branch, Reason: reason}, nil
	}

	pending, err := uncommittedWith(wt.Path, f)
	if err != nil {
		return fail(fmt.Sprintf("cannot list the assembly's uncommitted changes: %v", err))
	}
	if len(pending) > 0 {
		return LandResult{Status: "dirty", Task: task, Branch: branch, Files: pending,
			Reason: "the assembly has uncommitted changes that would not be landed"}, nil
	}

	baseline := readMarker(wt.Path, baselineFile)
	if baseline == "" {
		return fail("the assembly has no baseline marker")
	}
	tip, err := landRevParse(wt.Path, "HEAD")
	if err != nil {
		return fail(fmt.Sprintf("cannot read the assembly's HEAD: %v", err))
	}

	// An included assembly's baseline carries the user's uncommitted work; the
	// marker names the HEAD that work was on, which is what the target must
	// still contain.
	anchor := readMarker(wt.Path, includedFile)
	if anchor == "" {
		anchor = baseline
	}
	res, err := landOntoFrom(resolved, task, branch, baseline, anchor, tip, message, f)
	if err != nil {
		return res, err
	}
	switch res.Status {
	case "landed":
		if err := writeMarker(wt.Path, landedFile, tip); err != nil {
			return fail(fmt.Sprintf("landed %s, but could not record it in the assembly: %v", res.Commit, err))
		}
	case "empty":
	default:
		return res, nil
	}
	return finishAssembly(resolved, task, wt.Path, wt.Branch, res), nil
}

// finishAssembly removes the assembly once its work is landed (or nothing was
// left to land), and ends its request when that was the request's last live
// assembly. The request is found first: once the assembly is gone, nothing
// names it any more.
func finishAssembly(resolved, task, path, branch string, res LandResult) LandResult {
	req, _, _ := RequestFor(path)
	res.Request = req.ID
	rr, err := Remove(resolved, task, false, branchSession(branch))
	switch {
	case err != nil:
		res.Reason = fmt.Sprintf("the assembly was kept: %v", err)
	case rr.Status != "removed":
		res.Reason = fmt.Sprintf("the assembly was kept: %s: %s", rr.Status, rr.Reason)
	default:
		res.Removed = true
		if req.ID != "" {
			ended, err := EndRequestIfDone(req.ID)
			res.RequestEnded = ended
			if err != nil {
				res.Reason = fmt.Sprintf("could not end request %s: %v", req.ID, err)
			}
		}
	}
	return res
}

// LandPath lands the Agent-tool worktree at path (any linked worktree of
// the repository other than the checkout): its whole content, committed and
// uncommitted, measured from its fork point with the checkout or from
// baseline, and squashed the way Land squashes an assembly.
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
	return landOntoFrom(resolved, task, branch, base, base, tip, message, f)
}

// landOntoFrom is landOnto for a tip whose history starts at base but whose
// target must still contain anchor. An assembly built on the user's
// uncommitted work has base = its baseline commit, which holds that work on
// top of the HEAD it was cut from: the target is the user's branch, which
// need not carry the baseline itself, only the HEAD (anchor) it was made on.
// Only base..tip is squashed, so the uncommitted work stays out of the
// landed commit, and only the checkout's dirty paths the land changes block
// it: the fast-forward keeps the rest.
func landOntoFrom(resolved, task, branch, base, anchor, tip, message string, f ChangeFilter) (LandResult, error) {
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
	dirty := landStatusPaths(out)
	reason := "the checkout has uncommitted changes to tracked files; commit or set them aside first"
	if anchor != base {
		dirty = landOverlap(dirty, changed)
		reason = "the checkout has uncommitted changes to files this land changes; the assembly was built on your uncommitted work, so commit it (or set it aside) and land again"
	}
	if len(dirty) > 0 {
		return LandResult{Status: "dirty", Task: task, Branch: branch, Files: dirty, Reason: reason}, nil
	}

	target, err := landRevParse(resolved, "HEAD")
	if err != nil {
		return fail(fmt.Sprintf("cannot read %s's tip: %v", branch, err))
	}
	if _, err := gitx.Run(resolved, "merge-base", "--is-ancestor", anchor, target); err != nil {
		return LandResult{Status: "refused", Task: task, Branch: branch,
			Reason: fmt.Sprintf("%s no longer contains the run's base %s", branch, anchor[:min(8, len(anchor))])}, nil
	}

	var tree string
	// Compared with base, never anchor: with target at the anchor, the shortcut
	// would commit tip's tree, uncommitted work included.
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

// landRoleReason is why the tree at path cannot land, "" for an assembly.
func landRoleReason(resolved, path, task string) string {
	switch ReadRole(path) {
	case RoleAssembly:
		return ""
	case RoleIntegration:
		if ReadParent(path) != "" {
			return fmt.Sprintf("%s is an integration tree: merge it into its assembly with 'agent-exec isolate merge --task %s -m <message>', then land the assembly with 'agent-exec isolate land --task %s -m <message>'",
				task, task, landAssemblyTask(resolved, ReadParent(path)))
		}
		return fmt.Sprintf("%s is an integration tree with no assembly; it cannot be landed: discard it with 'agent-exec isolate remove --task %s --force'", task, task)
	default:
		return fmt.Sprintf("%s is a work tree; only an assembly lands: integrate it with 'agent-exec isolate integrate --tasks %s --into <integration>'", task, task)
	}
}

// landAssemblyTask is the task of the worktree at path, the assembly an
// integration tree names as its parent; "<assembly>" when it is not found.
func landAssemblyTask(resolved, path string) string {
	for _, w := range worktrees(resolved) {
		if paths.RealPath(w.Path) == paths.RealPath(path) && strings.HasPrefix(w.Branch, BranchPrefix) {
			return branchTask(w.Branch)
		}
	}
	return "<assembly>"
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

// landOverlap is the sorted paths both lists name.
func landOverlap(dirty, changed []string) []string {
	var both []string
	for _, p := range dirty {
		if slices.Contains(changed, p) {
			both = append(both, p)
		}
	}
	slices.Sort(both)
	return both
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
