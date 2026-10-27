package isolate

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/trace"
)

// RemoveResult is `isolate remove`'s output, for one task or (IsSession,
// Removed set) for every tree of a session.
type RemoveResult struct {
	Status    string // removed | dirty | busy | has-children | absent | unadopted | error
	Task      string
	Path      string
	Branch    string
	Session   string
	Reason    string   // dirty, busy, has-children, error; removed: a leftover trash dir
	Warning   string   // removed: why the hosted session in the tree was left open
	Files     []string // dirty only
	Children  []Child  // has-children only
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
	case r.Status == "has-children":
		children := r.Children
		if children == nil {
			children = []Child{}
		}
		o["status"] = r.Status
		o["task"] = r.Task
		o["path"] = r.Path
		o["reason"] = r.Reason
		o["children"] = children
	case r.Status == "busy":
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
		if r.Reason != "" {
			o["reason"] = r.Reason
		}
		if r.Warning != "" {
			o["warning"] = r.Warning
		}
	}
	return fsutil.MarshalJSON(o)
}

// uncommitted is what the tree at path holds beyond HEAD: index and working
// tree changes, untracked non-ignored files included (made visible to `git
// diff` as intent-to-add, as Diff does), excluded directories left out as
// Diff leaves them out.
func uncommitted(path string) ([]string, error) {
	return uncommittedWith(path, CurrentFilter())
}

func uncommittedWith(path string, f ChangeFilter) ([]string, error) {
	spec := f.Pathspec()
	_, _ = gitx.Run(path, append([]string{"add", "-A", "-N"}, spec...)...)
	out, err := gitx.Run(path, append([]string{"diff", "--name-only", "--no-renames", "-z", "HEAD"}, spec...)...)
	if err != nil {
		return nil, err
	}
	files := []string{}
	for _, f := range strings.Split(out, "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

// uncollected says why the tree at path still holds unreviewed work, or
// returns ok=false when it is safe to discard. Shared by remove's refusal
// and sweep's dry-run preview so the two can never disagree. Anything the
// gate cannot establish reads as dirty, never as clean. A role tree is gated
// by its commits, not by collection: an integration tree is complete once
// `isolate merge` recorded its HEAD as merged, an assembly once `isolate
// land` recorded its HEAD as landed (either also when nothing was ever
// committed past its baseline); then only uncommitted changes can be lost.
func uncollected(root, path, task, branch, session string) (files []string, reason string, dirty bool, err error) {
	role := ReadRole(path)
	if role == RoleIntegration || role == RoleAssembly {
		if why := unfinishedRoleTree(path, task, role); why != "" {
			return nil, why, true, nil
		}
	}
	// ReadBaseline would fall back to HEAD, hiding every commit on the branch.
	if readMarker(path, baselineFile) == "" {
		return nil, "no baseline recorded; cannot tell what is uncollected; pass --force", true, nil
	}
	if role == RoleIntegration || role == RoleAssembly {
		// Its commits are safe in its parent (or on the user's branch), so
		// only what was never committed can be lost. Its ignored surface
		// changes routinely.
		files, err := uncommitted(path)
		if err != nil {
			return nil, fmt.Sprintf("cannot list the %s tree's uncommitted changes (%v); pass --force", role, err), true, nil
		}
		if len(files) == 0 {
			return nil, "", false, nil
		}
		if role == RoleIntegration {
			return files, fmt.Sprintf("integration tree holds %d uncommitted change(s) on top of its merged commits; "+
				"commit and merge them, or pass --force", len(files)), true, nil
		}
		return files, fmt.Sprintf("assembly holds %d uncommitted change(s) on top of its landed commits; "+
			"commit and land them, or pass --force", len(files)), true, nil
	}
	// The patch is only needed to recompute a collected digest, which can
	// only match when a marker exists: the never-collected common case costs
	// one `git diff --name-only` instead of a full binary diff.
	stored := readCollected(path)
	diff, err := Diff(root, task, session, stored != "")
	if err != nil {
		return nil, "", false, err
	}
	if diff.Status != "ok" {
		why := diff.Reason
		if why == "" {
			why = "worktree " + diff.Status
		}
		return nil, fmt.Sprintf("cannot diff the worktree (%s: %s); pass --force", diff.Status, why), true, nil
	}
	files = nonNil(diff.Files)
	ignored := IgnoredEntries(path)
	matches := stored != "" && stored == CollectedDigest(files, diff.Patch, ignoredForDigest(path))
	trackedOK := len(files) == 0 || matches
	// Nearly every tree carries some ignored surface by design (the carried
	// node_modules); only a surface that differs from what create left
	// counts.
	created := readCreatedIgnored(path)
	ignoredOK := (created != "" && created == ignoredDigest(ignored)) || matches
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

// unfinishedRoleTree says why the commits of an integration tree or
// assembly would be lost with it, "" once its HEAD is the one merged
// (integration) or landed (assembly), or is still its baseline. The marker
// is compared with this tree's own HEAD, so a commit made after the merge or
// landing makes it unfinished again.
func unfinishedRoleTree(path, task, role string) string {
	out, err := gitx.Run(path, "rev-parse", "HEAD")
	head := strings.TrimSpace(out)
	if err != nil || head == "" {
		return fmt.Sprintf("cannot read the %s tree's HEAD; pass --force", role)
	}
	done := mergedFile
	if role == RoleAssembly {
		done = landedFile
	}
	if readMarker(path, done) == head || readMarker(path, baselineFile) == head {
		return ""
	}
	switch {
	case role == RoleAssembly:
		return fmt.Sprintf("assembly %s holds work not landed on your branch; land it from your checkout with "+
			"'agent-exec isolate land --task %s -m <message>', or pass --force to discard it", task, task)
	case ReadParent(path) != "":
		return fmt.Sprintf("integration tree %s holds commits not merged into its assembly; merge it with "+
			"'agent-exec isolate merge --task %s -m <message>', or pass --force to discard them", task, task)
	default:
		return fmt.Sprintf("integration tree %s has no assembly to merge into; pass --force to discard it", task)
	}
}

// exitOf is the timing-log exit code for a filesystem call: 0 or 1.
func exitOf(err error) int {
	if err != nil {
		return 1
	}
	return 0
}

// trashMarker names the sibling a tree is renamed to while it is removed:
// .<base>.orchestra-removing-<pid>-<unixnano>.
const trashMarker = ".orchestra-removing-"

func trashPath(path string) string {
	return filepath.Join(filepath.Dir(path), fmt.Sprintf(".%s%s%d-%d",
		filepath.Base(path), trashMarker, os.Getpid(), time.Now().UnixNano()))
}

// deleteDetached hands a renamed-aside tree to an `rm -rf` in its own
// session and returns at once: git already forgot the tree, and a carried
// node_modules is hundreds of thousands of files nobody should wait on. The
// rm outlives Ctrl-C and the caller; whatever it never finishes, sweep
// reclaims. done runs once the rm exits (while the caller lives); never when
// it could not start.
func deleteDetached(path string, done func()) error {
	cmd := exec.Command("/bin/rm", "-rf", "--", path)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	end := trace.Begin(trace.KindFS, "rm -rf (detached)", trace.Attrs{Detail: path})
	if err := cmd.Start(); err != nil {
		end(-1)
		return err
	}
	go func() { // reaped while the caller lives
		_ = cmd.Wait()
		end(cmd.ProcessState.ExitCode())
		done()
	}()
	return nil
}

// discard removes the worktree at path and its branch in two phases, so a
// failure never leaves a half-deleted tree where the worktree was: the tree
// is renamed aside (atomic, same directory), git forgets it by pruning, and
// only then is the branch dropped and the renamed tree handed to a detached
// delete. reason is "" on a clean removal, or names a leftover trash dir
// (ok stays true).
func discard(repo, path, branch string) (ok bool, reason string) {
	admin, err := gitx.GitDir(path)
	if err != nil {
		return false, fmt.Sprintf("cannot locate the worktree's git metadata (%v); nothing was changed", err)
	}
	trash := trashPath(path)
	end := trace.Begin(trace.KindFS, "rename-aside", trace.Attrs{Detail: path})
	err = os.Rename(path, trash)
	end(exitOf(err))
	if err != nil {
		return false, fmt.Sprintf("could not move the worktree aside (%v); nothing was changed: "+
			"tree and git metadata intact at %s", err, path)
	}
	_, pruneErr := gitx.Run(repo, "worktree", "prune")
	if _, err := os.Lstat(admin); err == nil {
		// Prune failed, or kept the entry (a locked worktree): put the tree
		// back so the metadata and the directory agree again.
		why := "git worktree prune kept its metadata (locked?)"
		if pruneErr != nil {
			why = fmt.Sprintf("git worktree prune failed (%v)", pruneErr)
		}
		end := trace.Begin(trace.KindFS, "rename-back", trace.Attrs{Detail: trash})
		err := os.Rename(trash, path)
		end(exitOf(err))
		if err != nil {
			return false, fmt.Sprintf("%s, and moving the tree back failed (%v): git metadata kept at %s, "+
				"tree at %s; recover with `mv %s %s`", why, err, admin, trash, trash, path)
		}
		return false, fmt.Sprintf("%s; metadata kept; tree restored at %s", why, path)
	}
	if branch != "" {
		_, _ = gitx.Run(repo, "branch", "-D", branch)
	}
	// The delete holds a heavy slot until it is done (or this process
	// exits, which frees the slot while a detached rm carries on).
	release := HoldHeavy("rm -rf worktree")
	if err := deleteDetached(trash, release); err != nil {
		// Nothing to hand it to: delete it here rather than leave it.
		end := trace.Begin(trace.KindFS, "remove-all", trace.Attrs{Detail: trash})
		err := os.RemoveAll(trash)
		end(exitOf(err))
		release()
		if err != nil {
			return true, fmt.Sprintf("git state removed, but the old tree is left at %s (%v); delete it by hand", trash, err)
		}
	}
	return true, ""
}

// Remove deletes task's worktree and branch. It refuses, even with force, a
// tree that still has children (status "has-children": a tree is complete
// only once every child was merged into it or discarded) and a tree
// somebody is still working in (status "busy"); without force it refuses
// (status "dirty") to discard work the role's gate cannot account for: a
// work tree's uncollected changes, an integration tree's unmerged commits,
// an assembly's unlanded ones. An adopted tree is never deleted: only its
// adoption is dropped. A hosted session recorded in a tree
// that goes is closed first (Warning when it could not be). session "" is
// the calling session.
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
	// Before force is looked at: a parent removed under its children would
	// orphan them and everything they still hold.
	children, err := Children(resolved, wt.Path)
	if err != nil {
		return RemoveResult{Status: "error", Task: task,
			Reason: fmt.Sprintf("cannot list the tree's children (%v); nothing was removed", err)}, nil
	}
	if len(children) > 0 {
		return RemoveResult{Status: "has-children", Task: task, Path: wt.Path, Children: children,
			Reason: fmt.Sprintf("%d child tree(s) still open; merge or discard them first "+
				"(--force never removes a tree with children)", len(children))}, nil
	}
	// Before the gate: its diff stages intent-to-add in the tree, which a
	// live writer must not see either.
	if reason := inUse(resolved, task, wt.Path); reason != "" {
		return RemoveResult{Status: "busy", Task: task, Path: wt.Path, Reason: reason}, nil
	}
	if !force {
		files, reason, dirty, err := uncollected(resolved, wt.Path, task, wt.Branch, session)
		if err != nil {
			return RemoveResult{}, err
		}
		if dirty {
			return RemoveResult{Status: "dirty", Task: task, Path: wt.Path, Files: files, Reason: reason}, nil
		}
	}
	warning := closeHostSession(wt.Path)
	removed, reason := discard(resolved, wt.Path, wt.Branch)
	if !removed {
		return RemoveResult{Status: "error", Task: task, Reason: reason}, nil
	}
	return RemoveResult{
		Status: "removed", Task: task, Path: wt.Path, Branch: wt.Branch, Session: branchSession(wt.Branch),
		Reason: reason, Warning: warning,
	}, nil
}

// RemoveSession removes every worktree of session (the SessionEnd hook's
// `isolate remove --session S --repo R`), deepest first, so a child goes
// before its parent and the parent can then go in the same call; dirty
// ones and those whose children stay are reported and kept.
func RemoveSession(root, session string, force bool) (RemoveResult, error) {
	wanted := sessionComponent(session)
	if wanted == "" {
		return RemoveResult{}, errInvalidSession
	}
	entries, _ := List(root, "")
	var mine []Entry
	for _, e := range entries {
		if string(e.Session) == wanted {
			mine = append(mine, e)
		}
	}
	depth := make(map[string]int, len(mine))
	for _, e := range mine {
		depth[e.Path] = lineageDepth(e.Path)
	}
	sort.SliceStable(mine, func(i, j int) bool { return depth[mine[i].Path] > depth[mine[j].Path] })
	res := RemoveResult{Status: "removed", Session: wanted, IsSession: true, Removed: []RemoveResult{}}
	for _, e := range mine {
		one, err := Remove(root, e.Task, force, wanted)
		if err != nil {
			return RemoveResult{}, err
		}
		res.Removed = append(res.Removed, one)
	}
	return res, nil
}
