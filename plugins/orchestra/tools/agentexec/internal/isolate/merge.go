package isolate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// mergeAssemblyLockTimeout bounds the wait for another merge into the same
// assembly: merges are short, so a longer wait means a stuck holder.
const mergeAssemblyLockTimeout = 900 * time.Second

// MergeResult is `isolate merge`'s output.
type MergeResult struct {
	Status   string // merged | empty | has-children | dirty | conflict | busy | refused | absent | error
	Task     string
	Assembly string   // the assembly's task id, once known
	Commit   string   // merged: the new assembly commit
	Files    []string // merged: changed paths; dirty/conflict/refused: the offending paths
	Children []Child  // has-children: the children still open
	Removed  bool     // merged/empty: the integration tree was removed afterwards
	Reason   string   // every other status; merged/empty: why the tree was kept (only when Removed is false)
}

// MarshalJSON always writes "status" and "task", and "assembly" once known;
// merged adds "commit", "files" and "removed"; empty adds "removed" (both
// add "reason" when the tree was kept); has-children adds "children" and
// "reason"; the others add "reason" and, when non-empty, "files".
func (r MergeResult) MarshalJSON() ([]byte, error) {
	o := map[string]any{"status": r.Status, "task": r.Task}
	if r.Assembly != "" {
		o["assembly"] = r.Assembly
	}
	switch r.Status {
	case "merged", "empty":
		if r.Status == "merged" {
			o["commit"] = r.Commit
			o["files"] = nonNil(r.Files)
		}
		o["removed"] = r.Removed
		if r.Reason != "" {
			o["reason"] = r.Reason
		}
	case "has-children":
		o["children"] = r.Children
		o["reason"] = r.Reason
	default:
		o["reason"] = r.Reason
		if len(r.Files) > 0 {
			o["files"] = r.Files
		}
	}
	return fsutil.MarshalJSON(o)
}

// Merge completes task's integration tree: its integrated children are
// reaped, merge-base(assembly HEAD, tip)..tip becomes one commit on the
// parent assembly with message as written, and the tree is marked merged and
// removed. The user's checkout and branch are never touched. A non-nil error
// is unusable input only (an unusable task id, an ambiguous task, an empty
// message), as everywhere in this package.
func Merge(root, task, message string) (MergeResult, error) {
	resolved, ok := gitx.RepoRoot(root)
	if !ok {
		return MergeResult{Status: "error", Reason: "not a git repository"}, nil
	}
	task, err := SanitizeTaskID(task)
	if err != nil {
		return MergeResult{}, err
	}
	if strings.TrimSpace(message) == "" {
		return MergeResult{}, errors.New("merge needs a commit message")
	}
	if landOwnMessage(message) {
		return MergeResult{Status: "refused", Task: task, Reason: landOwnReason}, nil
	}
	wt, found, err := resolveWorktree(resolved, task, "")
	if err != nil {
		return MergeResult{}, err
	}
	if !found {
		return MergeResult{Status: "absent", Task: task}, nil
	}
	tree := wt.Path
	if ReadRole(tree) != RoleIntegration {
		return MergeResult{Status: "refused", Task: task,
			Reason: fmt.Sprintf("%s is not an integration tree; only an integration tree merges into an assembly", task)}, nil
	}
	parent := ReadParent(tree)
	if parent == "" {
		return MergeResult{Status: "refused", Task: task,
			Reason: fmt.Sprintf("integration tree %s has no assembly to merge into; "+
				"discard it with 'agent-exec isolate remove --task %s --force'", task, task)}, nil
	}

	res := MergeResult{Task: task}
	fail := func(status, reason string) (MergeResult, error) {
		res.Status, res.Reason = status, reason
		return res, nil
	}
	var asm gitx.Worktree
	asmFound := false
	for _, w := range worktrees(resolved) {
		if paths.RealPath(w.Path) == paths.RealPath(parent) {
			asm, asmFound = w, true
			break
		}
	}
	if !asmFound || ReadRole(asm.Path) != RoleAssembly {
		return fail("error", fmt.Sprintf("the assembly %s of %s is gone", parent, task))
	}
	if asm.Branch == "" {
		return fail("error", fmt.Sprintf("the assembly %s has no branch checked out", parent))
	}
	res.Assembly = filepath.Base(parent)
	if strings.HasPrefix(asm.Branch, BranchPrefix) {
		res.Assembly = branchTask(asm.Branch)
	}

	// Before the locks: inUse probes the integrate lock of task and would
	// see our own hold as a running integrate.
	if reason := inUse(resolved, task, tree); reason != "" {
		return fail("busy", reason)
	}
	unlock, status, reason := mergeLock(resolved, task, 0,
		fmt.Sprintf("an isolate integrate into %s is running", task))
	if status != "" {
		return fail(status, reason)
	}
	unlockAsm, status, reason := mergeLock(resolved, res.Assembly, mergeAssemblyLockTimeout,
		fmt.Sprintf("another merge into %s is running", res.Assembly))
	if status != "" {
		_ = unlock()
		return fail(status, reason)
	}
	locked := true
	release := func() {
		if locked {
			locked = false
			_ = unlockAsm()
			_ = unlock()
		}
	}
	defer release()

	res, tip := mergeLocked(resolved, tree, parent, asm.Branch, res, message)
	if res.Status != "merged" && res.Status != "empty" {
		return res, nil
	}
	if err := writeMarker(tree, mergedFile, tip); err != nil {
		res.Reason = fmt.Sprintf("could not record the merge in the integration tree: %v", err)
		return res, nil
	}
	// Remove probes inUse, which must not find our own locks.
	release()
	rr, err := Remove(resolved, task, false, branchSession(wt.Branch))
	switch {
	case err != nil:
		res.Reason = fmt.Sprintf("the integration tree was kept: error: %v", err)
	case rr.Status == "removed":
		res.Removed = true
	default:
		res.Reason = fmt.Sprintf("the integration tree was kept: %s: %s", rr.Status, rr.Reason)
	}
	return res, nil
}

// mergeLock takes the integrate lock of into exclusively; a non-empty status
// (busy on timeout, error otherwise) means it was not taken.
func mergeLock(resolved, into string, timeout time.Duration, busy string) (func() error, string, string) {
	lock := IntegrateLockPath(resolved, into)
	if lock == "" {
		return nil, "error", fmt.Sprintf("cannot locate the integrate lock of %s", into)
	}
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		return nil, "error", fmt.Sprintf("cannot create the lock directory: %v", err)
	}
	unlock, err := fsutil.Lock(lock, fsutil.Exclusive, timeout)
	if errors.Is(err, fsutil.ErrLockTimeout) {
		return nil, "busy", busy
	}
	if err != nil {
		return nil, "error", fmt.Sprintf("cannot lock %s: %v", lock, err)
	}
	return unlock, "", ""
}

// mergeLocked is Merge's work under both locks: reap the children, then
// squash the integration tree into the assembly at parent (on branch). tip is
// the integration tree's HEAD, set once the status is merged or empty.
func mergeLocked(resolved, tree, parent, branch string, res MergeResult, message string) (MergeResult, string) {
	task := res.Task
	fail := func(status, reason string, files []string) (MergeResult, string) {
		res.Status, res.Reason, res.Files = status, reason, files
		return res, ""
	}

	children, err := mergeReapChildren(resolved, tree)
	if err != nil {
		return fail("error", fmt.Sprintf("cannot list the integration tree's children (%v); nothing was merged", err), nil)
	}
	if len(children) > 0 {
		res.Children = children
		return fail("has-children", fmt.Sprintf("%d child tree(s) still hold work that is not integrated, "+
			"or are in use; integrate or discard them first", len(children)), nil)
	}

	f := CurrentFilter()
	pending, err := uncommittedWith(tree, f)
	if err != nil {
		return fail("error", fmt.Sprintf("cannot list the integration tree's uncommitted changes: %v", err), nil)
	}
	if len(pending) > 0 {
		return fail("dirty", "the integration tree has uncommitted changes that would not be merged", pending)
	}

	tip, err := landRevParse(tree, "HEAD")
	if err != nil {
		return fail("error", fmt.Sprintf("cannot read the integration tree's HEAD: %v", err), nil)
	}
	target, err := landRevParse(parent, "HEAD")
	if err != nil {
		return fail("error", fmt.Sprintf("cannot read the assembly's HEAD: %v", err), nil)
	}
	// From the fork point, not the baseline: what the assembly already has
	// (an earlier merge, a refresh) must not be squashed in again.
	out, err := gitx.Run(parent, "merge-base", target, tip)
	base := strings.TrimSpace(out)
	if err != nil || base == "" {
		return fail("error", fmt.Sprintf("no common history between %s and its assembly", task), nil)
	}
	if base == tip {
		res.Status = "empty"
		return res, tip
	}

	lr, err := landOnto(parent, task, branch, base, tip, message, f)
	if err != nil {
		return fail("error", err.Error(), nil)
	}
	switch lr.Status {
	case "landed":
		res.Status, res.Commit, res.Files = "merged", lr.Commit, nonNil(lr.Files)
		return res, tip
	case "empty":
		res.Status = "empty"
		return res, tip
	case "dirty":
		return fail("dirty", "the assembly has uncommitted changes to tracked files; commit or discard them there first", lr.Files)
	case "conflict":
		return fail("conflict", fmt.Sprintf("the assembly moved since %s was cut and the changes conflict; "+
			"merge the assembly's branch %s into the integration tree, resolve, commit, and run isolate merge again",
			task, branch), lr.Files)
	}
	return fail(lr.Status, lr.Reason, lr.Files)
}

// mergeReapChildren removes (or, when adopted, unadopts) every child of tree
// whose work is already integrated and nobody is using, and lists the
// children that remain. A child the gates keep simply stays.
func mergeReapChildren(resolved, tree string) ([]Child, error) {
	children, err := Children(resolved, tree)
	if err != nil {
		return nil, err
	}
	for _, c := range children {
		if c.Task == "" {
			continue
		}
		if a, adopted := readAdoption(c.Path); adopted {
			// Remove would drop an adoption unconditionally; only a clean
			// one may stop counting as a child here.
			_, _, dirty, err := uncollected(resolved, c.Path, c.Task, c.Branch, a.Session)
			if err == nil && !dirty && inUse(resolved, c.Task, c.Path) == "" {
				_ = ClearAdopted(c.Path)
			}
			continue
		}
		_, _ = Remove(resolved, c.Task, false, branchSession(c.Branch))
	}
	return Children(resolved, tree)
}

// FormatMergeText is the one-line text form of r.
func FormatMergeText(r MergeResult) string {
	kept := ""
	if !r.Removed && r.Reason != "" {
		kept = "; " + r.Reason
	}
	switch r.Status {
	case "merged":
		return fmt.Sprintf("merged %s into %s: %d files (from %s)",
			r.Commit[:min(8, len(r.Commit))], r.Assembly, len(r.Files), r.Task) + kept
	case "empty":
		return fmt.Sprintf("nothing to merge: %s has no change past its assembly", r.Task) + kept
	case "has-children":
		tasks := make([]string, len(r.Children))
		for i, c := range r.Children {
			tasks[i] = c.Task
		}
		return "has-children: " + r.Reason + ": " + strings.Join(tasks, ", ")
	}
	line := r.Status + ": " + r.Reason
	if len(r.Files) > 0 {
		shown, more := r.Files, ""
		if len(shown) > 5 {
			shown, more = shown[:5], ", ..."
		}
		line += ": " + strings.Join(shown, ", ") + more
	}
	return line
}
