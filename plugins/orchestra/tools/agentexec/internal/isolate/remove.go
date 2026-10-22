package isolate

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	Status    string // removed | dirty | busy | absent | unadopted | error
	Task      string
	Path      string
	Branch    string
	Session   string
	Reason    string   // dirty, busy, error; removed: a leftover trash dir
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
	}
	return fsutil.MarshalJSON(o)
}

// landingRefs are the local branches an integration tree can land on (full
// ref names), filtered by the extra for-each-ref args: never own, never an
// orchestra/* branch -- another task or integration branch holding the same
// commits is not a landing, and is itself removable.
func landingRefs(path, own string, args ...string) ([]string, error) {
	out, err := gitx.Run(path, append(append([]string{"for-each-ref", "--format=%(refname)"}, args...), "refs/heads/")...)
	if err != nil {
		return nil, err
	}
	var refs []string
	for _, ref := range strings.Split(out, "\n") {
		ref = strings.TrimSpace(ref)
		if ref != "" && ref != own && !strings.HasPrefix(ref, "refs/heads/"+BranchPrefix) {
			refs = append(refs, ref)
		}
	}
	return refs, nil
}

// unmergedIntegration says why an integration tree's commits would be lost
// with it; "" once they landed on a non-orchestra local branch: its tip
// (HEAD, so a detached tree works too) is reachable from one, or, for the
// usual squash landing, one holds exactly the tip's content in every file
// the tree changed since its baseline.
func unmergedIntegration(path, branch string) string {
	own := ""
	if branch != "" {
		own = "refs/heads/" + branch
	}
	reaching, err := landingRefs(path, own, "--contains", "HEAD")
	if err != nil {
		return "cannot tell whether the integration tree's commits are merged (git for-each-ref failed); pass --force"
	}
	if len(reaching) > 0 || squashLanded(path, own) {
		return ""
	}
	name := branch
	if name == "" {
		name = "(detached HEAD)"
	}
	return fmt.Sprintf("integration branch %s holds commits not landed on any non-orchestra local branch; "+
		"merge or squash it there first, or pass --force", name)
}

// pathspecChunk bounds the files per `git diff` call, keeping the argument
// list well under the OS limit however many files a round integrated.
const pathspecChunk = 500

// squashLanded reports whether the integrated changes of the tree at path
// (baseline..HEAD, by file) have identical content on some local branch
// other than own and the orchestra/* ones. A deletion must be a deletion
// there too. Anything git cannot answer is "not landed".
func squashLanded(path, own string) bool {
	baseline := readMarker(path, baselineFile)
	if baseline == "" {
		return false
	}
	// --no-renames: a rename must show its old path too, whose deletion has
	// to be on the other branch as well.
	out, err := gitx.Run(path, "diff", "--name-only", "--no-renames", "-z", baseline, "HEAD")
	if err != nil {
		return false
	}
	var files []string
	for _, f := range strings.Split(out, "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		return true
	}
	refs, err := landingRefs(path, own)
	if err != nil {
		return false
	}
	for _, ref := range refs {
		if sameContent(path, ref, files) {
			return true
		}
	}
	return false
}

// sameContent reports whether files read the same at HEAD and at ref. The
// pathspecs are literal, so odd names never act as globs.
func sameContent(path, ref string, files []string) bool {
	for start := 0; start < len(files); start += pathspecChunk {
		chunk := files[start:min(start+pathspecChunk, len(files))]
		args := append([]string{"--literal-pathspecs", "diff", "--quiet", "--no-renames", "HEAD", ref, "--"}, chunk...)
		if _, err := gitx.Run(path, args...); err != nil {
			return false
		}
	}
	return true
}

// uncommitted is what the tree at path holds beyond HEAD: index and working
// tree changes, untracked non-ignored files included (made visible to `git
// diff` as intent-to-add, as Diff does).
func uncommitted(path string) ([]string, error) {
	_, _ = gitx.Run(path, "add", "-A", "-N")
	out, err := gitx.Run(path, "diff", "--name-only", "--no-renames", "-z", "HEAD")
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
// gate cannot establish reads as dirty, never as clean.
func uncollected(root, path, task, branch, session string) (files []string, reason string, dirty bool, err error) {
	// An integration tree carries commits; collected markers say nothing
	// about whether they landed anywhere else.
	role := ReadRole(path)
	if role == RoleIntegration {
		if why := unmergedIntegration(path, branch); why != "" {
			return nil, why, true, nil
		}
	}
	// ReadBaseline would fall back to HEAD, hiding every commit on the branch.
	if readMarker(path, baselineFile) == "" {
		return nil, "no baseline recorded; cannot tell what is uncollected; pass --force", true, nil
	}
	if role == RoleIntegration {
		// Landed: its commits are safe elsewhere, so only what was never
		// committed can be lost. Its ignored surface changes routinely.
		files, err := uncommitted(path)
		if err != nil {
			return nil, fmt.Sprintf("cannot list the integration tree's uncommitted changes (%v); pass --force", err), true, nil
		}
		if len(files) > 0 {
			return files, fmt.Sprintf("integration tree holds %d uncommitted change(s) on top of its landed commits; "+
				"commit and land them, or pass --force", len(files)), true, nil
		}
		return nil, "", false, nil
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
// reclaims.
func deleteDetached(path string) error {
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
	if err := deleteDetached(trash); err != nil {
		// Nothing to hand it to: delete it here rather than leave it.
		end := trace.Begin(trace.KindFS, "remove-all", trace.Attrs{Detail: trash})
		err := os.RemoveAll(trash)
		end(exitOf(err))
		if err != nil {
			return true, fmt.Sprintf("git state removed, but the old tree is left at %s (%v); delete it by hand", trash, err)
		}
	}
	return true, ""
}

// Remove deletes task's worktree and branch, refusing (status "dirty") to
// discard changes nobody collected unless force, and (status "busy", even
// with force) a tree somebody is still working in. An adopted tree is never
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
	removed, reason := discard(resolved, wt.Path, wt.Branch)
	if !removed {
		return RemoveResult{Status: "error", Task: task, Reason: reason}, nil
	}
	return RemoveResult{
		Status: "removed", Task: task, Path: wt.Path, Branch: wt.Branch, Session: branchSession(wt.Branch),
		Reason: reason,
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
