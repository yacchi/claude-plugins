// Package integrate replays each task worktree's diff, in order, onto one
// integration worktree, and answers with counts and enums only (paths, file
// and hunk counts, never patch text), so an orchestrator can parallelize
// tasks that share files without ingesting diffs. Conflicts are a normal,
// reported outcome; an optional verify command grades the result and an
// optional bisection finds and reverts the applied task that broke it.
package integrate

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// Conflict policies of Options.OnConflict.
const (
	// OnConflictKeep commits the conflict markers and carries on: a later
	// resolution task inspects them. Earlier clean applies are kept.
	OnConflictKeep = "keep"
	// OnConflictSkip rolls the conflicting apply back and carries on.
	OnConflictSkip = "skip"
	// OnConflictStop rolls it back and skips every remaining task.
	OnConflictStop = "stop"
)

// DefaultInto is the integration task id when Options.Into is empty.
const DefaultInto = "integrate"

// DefaultBisectMax is the bisect-and-revert round budget when
// Options.BisectMax is not positive.
const DefaultBisectMax = 3

// Options tunes Integrate. Into "" is DefaultInto; the integration tree is
// created by isolate.EnsureIntegration from the calling session's request
// assembly, and Uncommitted is the choice it needs when the user's checkout
// has uncommitted work (see isolate.UncommittedInclude). OnConflict "" is
// keep. Verify "" runs no verification; VerifyTimeout 0 means no limit. Lock
// "" takes no lock; LockWait waits up to LockTimeout, LockFail tries once.
// Progress, when set, receives one JSON line per stage
// ({"event","pkg","detail","at"}), one Write per line; its errors are
// ignored. Filter nil is isolate.CurrentFilter(): excluded paths never
// enter a change set or a commit, and a task adding more than MaxAdded new
// files is refused.
type Options struct {
	Into, OnConflict, Verify, Lock, Uncommitted string
	VerifyTimeout, LockTimeout                  time.Duration
	Bisect                                      bool
	BisectMax                                   int
	Progress                                    io.Writer
	Filter                                      *isolate.ChangeFilter
}

// Integration names the integration worktree.
type Integration struct {
	Task   string             `json:"task"`
	Branch string             `json:"branch"`
	Path   isolate.NullString `json:"path"`
}

// TaskResult is one task's outcome. Status is applied | conflicted |
// missing | empty | foreign (its parent marker names another integration
// tree; left for that tree, because the hierarchy only completes when every
// child merges into its OWN integration tree) | skipped | reverted | refused
// (adds more new files than the filter allows; Reason says how many, and the
// task was not applied) |
// commit-failed (its integrate commit failed; Reason is git's output, the
// apply was rolled back and the source is not collected).
// Excluded counts the files the filter kept out of an applied change set.
type TaskResult struct {
	Task          string             `json:"task"`
	Status        string             `json:"status"`
	FilesChanged  int                `json:"files_changed"`
	Conflicts     []isolate.Conflict `json:"conflicts"`
	Reason        string             `json:"reason,omitempty"`
	Excluded      int                `json:"excluded,omitempty"`
	RolledBack    bool               `json:"rolled_back,omitempty"`
	Revert        string             `json:"revert,omitempty"`
	VerifyExcerpt *string            `json:"verify_excerpt,omitempty"`
}

// Result is `isolate integrate`'s output. Status is ok | conflicted |
// verify-failed | commit-failed | reverted | refused (otherwise ok, but a
// task was refused) | uncommitted (the checkout has uncommitted work and the
// request's assembly was not created; every task is missing) | error, or
// locked (then only Into, Holder, LockFile and Note are meaningful).
type Result struct {
	Status      string
	Integration Integration
	Onto        string
	Tasks       []TaskResult
	Note        string
	Verify      *VerifyResult
	Into        string // locked only
	Holder      string // locked only
	LockFile    string // locked only
}

func (r Result) MarshalJSON() ([]byte, error) {
	if r.Status == "locked" {
		var holder any
		if r.Holder != "" {
			holder = r.Holder
		}
		return fsutil.MarshalJSON(map[string]any{
			"status": r.Status, "into": r.Into, "holder": holder, "lock": r.LockFile, "note": r.Note,
		})
	}
	o := map[string]any{"status": r.Status}
	var onto any
	if r.Onto != "" {
		onto = r.Onto
	}
	tasks := r.Tasks
	if tasks == nil {
		tasks = []TaskResult{}
	}
	o["integration"] = r.Integration
	o["onto"] = onto
	o["tasks"] = tasks
	o["note"] = r.Note
	if r.Verify != nil {
		o["verify"] = r.Verify
	}
	return fsutil.MarshalJSON(o)
}

// ExitCode is the CLI exit status of a result.
func ExitCode(r Result) int {
	switch r.Status {
	case "ok":
		return 0
	case "conflicted":
		return 1
	case "verify-failed":
		return 4
	case "reverted":
		return 5
	case "locked":
		return 6
	case "refused":
		return 7
	case "commit-failed":
		return 8
	case "uncommitted":
		return 9
	}
	return 3
}

// commitExcerptLines bounds the git output a commit-failed task carries.
const commitExcerptLines = 20

func commitAll(wt, message string) error {
	return commitAllWith(wt, message, isolate.CurrentFilter())
}

// commitAllWith commits everything but f's excluded paths (a cache dir
// created in the integration tree never lands in a commit), hooks off. A
// path under an excluded entry staged earlier is unstaged first, since git
// commit takes the whole index. With --allow-empty a commit always moves
// HEAD, so one that exits 0 and does not is a failure too.
func commitAllWith(wt, message string, f isolate.ChangeFilter) error {
	pre := headSHA(wt)
	// Nothing staged there exits 0; a failure must not block the commit, as
	// the add below is still filtered.
	if spec := f.ExcludedPathspec(); spec != nil {
		_, _ = gitx.Run(wt, append([]string{"reset", "-q"}, spec...)...)
	}
	if err := gitx.CommitAll(wt, message, f.Pathspec()...); err != nil {
		return err
	}
	if headSHA(wt) == pre {
		return errors.New("git commit exited 0 but HEAD did not move")
	}
	return nil
}

// commitExcerpt is the first lines of a failed commit's output.
func commitExcerpt(err error) string {
	lines := nonBlank(strings.Split(err.Error(), "\n"))
	if len(lines) > commitExcerptLines {
		lines = lines[:commitExcerptLines]
	}
	return strings.Join(lines, "\n")
}

func nonBlank(lines []string) []string {
	var out []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			out = append(out, strings.TrimRight(l, "\r"))
		}
	}
	return out
}

// rollback puts the integration tree back at pre with nothing staged or
// untracked left; false when that did not hold afterwards.
func rollback(wt, pre string) bool {
	_, _ = gitx.Run(wt, "reset", "-q", "--hard", pre)
	_, _ = gitx.Run(wt, "clean", "-fdq")
	out, err := gitx.Run(wt, "status", "--porcelain")
	return err == nil && strings.TrimSpace(out) == "" && headSHA(wt) == pre
}

func headSHA(wt string) string {
	out, err := gitx.Run(wt, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// emit writes one progress line; observability never fails a merge.
func emit(w io.Writer, event string, pkg string, detail map[string]any) {
	if w == nil {
		return
	}
	var p any
	if pkg != "" {
		p = pkg
	}
	line, err := fsutil.MarshalJSON(map[string]any{
		"event":  event,
		"pkg":    p,
		"detail": detail,
		"at":     float64(time.Now().UnixNano()) / 1e9,
	})
	if err == nil {
		_, _ = w.Write(append(line, '\n'))
	}
}

// changeSet is one source task's own changes.
type changeSet struct {
	task         string
	state        string // missing | empty | ready | foreign
	reason       string // why a foreign task is left alone
	files        []string
	patch        string
	added        int // new files among files
	excluded     int // files the filter kept out
	excludedDirs []string
}

func taskChangeSet(root, task string, f isolate.ChangeFilter) (changeSet, error) {
	blank := changeSet{task: task, state: "missing"}
	path, _, ok := isolate.ResolveWorktree(root, task, "")
	if !ok {
		// ResolveWorktree hides ambiguity; Diff below reports it.
		if _, err := isolate.DiffWith(root, task, "", false, f); err != nil {
			return blank, err
		}
		return blank, nil
	}
	if st, err := os.Stat(path); err != nil || !st.IsDir() {
		return blank, nil
	}
	// Stage in full first: `git apply --3way` needs the post-image blobs in
	// the object database, and an intent-to-add file has none. Excluded
	// paths are never staged.
	_, _ = gitx.Run(path, append([]string{"add", "-A"}, f.Pathspec()...)...)
	diff, err := isolate.DiffWith(root, task, "", true, f)
	if err != nil {
		return blank, err
	}
	if diff.Status != "ok" {
		return blank, nil
	}
	cs := changeSet{task: task, state: "empty", patch: diff.Patch,
		added: diff.Added, excluded: diff.Excluded, excludedDirs: diff.ExcludedDirs}
	for _, f := range diff.Files {
		if f != "" {
			cs.files = append(cs.files, f)
		}
	}
	if len(cs.files) > 0 && strings.TrimSpace(cs.patch) != "" {
		cs.state = "ready"
	}
	return cs, nil
}

// markSourceCollected records the source tree's changes as collected, from
// the files and patch this run already computed (never a fresh diff).
func markSourceCollected(root, task string, files []string, patch string) {
	if src, _, ok := isolate.ResolveWorktree(root, task, ""); ok {
		_ = isolate.WriteCollected(src, isolate.CollectedDigest(files, patch, isolate.IgnoredEntries(src)))
	}
}

// clearSourceCollected undoes markSourceCollected when a bisect revert takes
// the task's contribution back out: sweep must keep its tree again.
func clearSourceCollected(root, task string) {
	if src, _, ok := isolate.ResolveWorktree(root, task, ""); ok {
		_ = isolate.ClearCollected(src)
	}
}

// Integrate replays tasks' diffs onto the integration worktree o.Into of
// the request of the repository containing root. The user's tree is never
// touched. Each
// applied task is committed there, giving the next 3-way apply a real merge
// base. A non-nil error means unusable input (a task id with no usable
// characters, an ambiguous task); everything else is in Result.Status.
func Integrate(root string, tasks []string, o Options) (Result, error) {
	into := o.Into
	if into == "" {
		into = DefaultInto
	}
	if o.Lock != "" {
		if lockFile := LockPath(root, into); lockFile != "" {
			held, holder, err := acquireLock(lockFile, o.Lock, o.LockTimeout)
			if err != nil {
				return Result{Status: "error", Integration: Integration{Task: into},
					Note: fmt.Sprintf("could not open the integrate lock %s: %v", lockFile, err)}, nil
			}
			if held == nil {
				return Result{Status: "locked", Into: into, Holder: holder, LockFile: lockFile,
					Note: "another integrate holds the lock for this target"}, nil
			}
			defer held.release()
		}
	}
	return integrate(root, tasks, into, o)
}

func integrate(root string, tasks []string, into string, o Options) (Result, error) {
	resolved, isRepo := gitx.RepoRoot(root)
	intoTask, err := isolate.SanitizeTaskID(into)
	if err != nil {
		return Result{}, err
	}
	session, _ := paths.SessionID()
	branch := isolate.Branch(intoTask, session)
	ids := make([]string, len(tasks))
	for i, t := range tasks {
		if ids[i], err = isolate.SanitizeTaskID(t); err != nil {
			return Result{}, err
		}
	}
	failure := func(note string) (Result, error) {
		r := Result{Status: "error", Integration: Integration{Task: intoTask, Branch: branch}, Note: note}
		for _, t := range ids {
			r.Tasks = append(r.Tasks, TaskResult{Task: t, Status: "missing", Conflicts: []isolate.Conflict{}})
		}
		return r, nil
	}
	if !isRepo {
		return failure("not a git repository")
	}

	var f isolate.ChangeFilter
	if o.Filter != nil {
		f = *o.Filter
	} else {
		f = isolate.CurrentFilter()
	}
	changes := make([]changeSet, len(ids))
	for i, t := range ids {
		if changes[i], err = taskChangeSet(resolved, t, f); err != nil {
			return Result{}, err
		}
	}

	ens, err := isolate.EnsureIntegration(resolved, intoTask, o.Uncommitted)
	if err != nil {
		return Result{}, err
	}
	switch ens.Status {
	case "ready":
	case "uncommitted":
		res, _ := failure(ens.Reason)
		res.Status = "uncommitted"
		return res, nil
	default:
		return failure(ens.Reason)
	}
	wt := ens.Integration
	// A reused integration tree's own baseline is the truth about where it
	// started; a fresh one starts at the assembly's HEAD.
	ontoSHA := ens.Head
	if b, ok := isolate.ReadBaseline(wt); ok {
		ontoSHA = b
	}

	// A task whose parent marker names another integration tree belongs to
	// that tree: merging it here would strand it under the wrong parent.
	for i := range changes {
		ch := &changes[i]
		if ch.state != "ready" && ch.state != "empty" {
			continue
		}
		src, _, ok := isolate.ResolveWorktree(resolved, ch.task, "")
		if !ok {
			continue
		}
		if parent := isolate.ReadParent(src); parent != "" && parent != paths.RealPath(wt) {
			ch.state = "foreign"
			ch.reason = fmt.Sprintf("task %s belongs to integration tree %s", ch.task, parent)
		}
	}

	// Bisection's search space starts at the tip before THIS invocation: a
	// reused tree may already carry earlier rounds' commits.
	invocationBase := headSHA(wt)
	if invocationBase == "" {
		invocationBase = ontoSHA
	}

	emit(o.Progress, "integrate-start", "", map[string]any{"tasks": append([]string{}, ids...)})
	results := make([]TaskResult, 0, len(changes))
	var applied []AppliedCommit
	stop := false
	for _, ch := range changes {
		task := ch.task
		if stop {
			results = append(results, TaskResult{Task: task, Status: "skipped", Conflicts: []isolate.Conflict{}})
			emit(o.Progress, "integrate-task", task, map[string]any{"status": "skipped"})
			continue
		}
		if ch.state != "ready" {
			results = append(results, TaskResult{Task: task, Status: ch.state, Conflicts: []isolate.Conflict{},
				Reason: ch.reason})
			emit(o.Progress, "integrate-task", task, map[string]any{"status": ch.state})
			if ch.state == "empty" {
				markSourceCollected(resolved, task, ch.files, ch.patch)
			}
			continue
		}
		if f.MaxAdded > 0 && ch.added > f.MaxAdded {
			// The source tree keeps its work (not collected) for the fix.
			results = append(results, TaskResult{Task: task, Status: "refused", FilesChanged: len(ch.files),
				Conflicts: []isolate.Conflict{}, Reason: refusal(ch, f.MaxAdded)})
			emit(o.Progress, "integrate-task", task, map[string]any{"status": "refused"})
			continue
		}
		pre := headSHA(wt)
		if pre == "" {
			pre = invocationBase
		}
		applyErr := gitx.ApplyPatch3Way(wt, []byte(ch.patch))
		conflicts := []isolate.Conflict{}
		if applyErr != nil {
			conflicts = isolate.UnmergedConflicts(wt)
		}
		// Commit whatever resulted, markers included: staging resolves the
		// index, so the next task still has a real base to merge against.
		if err := commitAllWith(wt, "orchestra integrate "+task, f); err != nil {
			// Undone like a skipped conflict, so the next task starts from
			// a clean pre-task HEAD; the source keeps its work uncollected.
			// A tree that would not come clean stops the run instead of
			// letting later tasks pile onto it.
			clean := rollback(wt, pre)
			stop = !clean
			results = append(results, TaskResult{Task: task, Status: "commit-failed",
				FilesChanged: len(ch.files), Conflicts: []isolate.Conflict{}, Reason: commitExcerpt(err),
				Excluded: ch.excluded, RolledBack: clean})
			emit(o.Progress, "integrate-task", task, map[string]any{"status": "commit-failed"})
			continue
		}
		entry := TaskResult{Task: task, Status: "applied", FilesChanged: len(ch.files), Conflicts: conflicts,
			Excluded: ch.excluded}
		switch {
		case applyErr == nil:
			markSourceCollected(resolved, task, ch.files, ch.patch)
			applied = append(applied, AppliedCommit{Task: task, SHA: headSHA(wt)})
		case o.OnConflict == OnConflictSkip || o.OnConflict == OnConflictStop:
			// Undo the apply entirely: later tasks (and verify) must see no
			// conflict markers, and the source is NOT collected.
			entry.Status = "conflicted"
			entry.RolledBack = rollback(wt, pre)
			stop = o.OnConflict == OnConflictStop || !entry.RolledBack
		default:
			entry.Status = "conflicted"
		}
		results = append(results, entry)
		emit(o.Progress, "integrate-task", task, map[string]any{"status": entry.Status})
	}

	conflicted := false
	for _, r := range results {
		conflicted = conflicted || r.Status == "conflicted"
	}
	verify := runVerifyStage(o, resolved, wt, branch, invocationBase, applied, conflicted, results)

	status := "ok"
	switch {
	case verify != nil && verify.Status == "fail":
		status = "verify-failed"
	case conflicted:
		status = "conflicted"
	case hasStatus(results, "commit-failed"):
		status = "commit-failed"
	case hasStatus(results, "reverted"):
		status = "reverted"
	case hasStatus(results, "refused"):
		status = "refused"
	}
	res := Result{
		Status:      status,
		Integration: Integration{Task: intoTask, Branch: branch, Path: isolate.NullString(wt)},
		Onto:        ontoSHA,
		Tasks:       results,
		Note:        note(results, intoTask, wt),
		Verify:      verify,
	}
	emit(o.Progress, "integrate-end", "", map[string]any{"status": status})
	return res, nil
}

// refusal is a refused task's reason.
func refusal(ch changeSet, maxAdded int) string {
	why := fmt.Sprintf("adds %d new files (isolation.max_added_files %d)", ch.added, maxAdded)
	if ch.excluded > 0 {
		why += fmt.Sprintf("; excluded %d files under %s", ch.excluded, strings.Join(ch.excludedDirs, ", "))
	}
	return why
}

// runVerifyStage runs o.Verify (and the bisection) after every task was
// processed, updating results in place for reverted culprits. nil when no
// verify was asked for.
func runVerifyStage(o Options, root, wt, branch, base string, applied []AppliedCommit,
	conflicted bool, results []TaskResult) *VerifyResult {
	switch {
	case o.Verify == "":
		return nil
	case (o.OnConflict == "" || o.OnConflict == OnConflictKeep) && conflicted:
		return skippedVerify("conflict markers committed")
	case len(applied) == 0:
		return skippedVerify("nothing applied")
	}
	emit(o.Progress, "verify-start", "", map[string]any{"at_commit": headSHA(wt)})
	v := RunVerify(wt, o.Verify, o.VerifyTimeout)
	emit(o.Progress, "verify-end", "", map[string]any{"status": v.Status, "seconds": v.Seconds})
	if v.Status != "fail" || !o.Bisect {
		return &v
	}
	b, err := Bisect(BisectInput{
		Root: root, Worktree: wt, Branch: branch, BaseSHA: base, Applied: applied,
		Verify: o.Verify, Timeout: o.VerifyTimeout, Max: o.BisectMax,
		Progress: o.Progress, DirtiedInitial: v.Dirtied,
	})
	if err != nil {
		return &v
	}
	for i := range results {
		for _, rv := range b.Reverted {
			if results[i].Task == rv.Task {
				excerpt := rv.Excerpt
				results[i].Status, results[i].VerifyExcerpt = "reverted", &excerpt
			}
		}
		if results[i].Task == b.RevertConflicted {
			results[i].Revert = "conflicted"
		}
		if results[i].Task == b.RevertCommitFailed {
			results[i].Revert = "commit-failed"
		}
	}
	return &b.Verify
}

func hasStatus(results []TaskResult, status string) bool {
	for _, r := range results {
		if r.Status == status {
			return true
		}
	}
	return false
}

// note is one short human sentence summarizing the run (never patch text).
func note(results []TaskResult, intoTask, wt string) string {
	counts := map[string]int{}
	for _, r := range results {
		counts[r.Status]++
	}
	var tail []string
	if n := counts["missing"]; n > 0 {
		tail = append(tail, fmt.Sprintf("%d had no worktree", n))
	}
	if n := counts["foreign"]; n > 0 {
		tail = append(tail, fmt.Sprintf("%d belong to another integration tree", n))
	}
	if n := counts["empty"]; n > 0 {
		tail = append(tail, fmt.Sprintf("%d changed nothing", n))
	}
	if n := counts["skipped"]; n > 0 {
		tail = append(tail, fmt.Sprintf("%d skipped", n))
	}
	if n := counts["reverted"]; n > 0 {
		tail = append(tail, fmt.Sprintf("%d reverted", n))
	}
	if n := counts["refused"]; n > 0 {
		tail = append(tail, fmt.Sprintf("%d refused", n))
	}
	if n := counts["commit-failed"]; n > 0 {
		tail = append(tail, fmt.Sprintf("%d failed to commit", n))
	}
	suffix := ""
	if len(tail) > 0 {
		suffix = " (" + strings.Join(tail, ", ") + ")"
	}
	if counts["conflicted"] > 0 {
		return fmt.Sprintf("%d of %d task(s) applied, %d conflicted%s; resolve in worktree '%s' at %s",
			counts["applied"], len(results), counts["conflicted"], suffix, intoTask, wt)
	}
	return fmt.Sprintf("%d of %d task(s) applied cleanly%s into worktree '%s' at %s",
		counts["applied"], len(results), suffix, intoTask, wt)
}
