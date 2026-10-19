package isolate

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
)

// Sweep is the after-the-fact reclaim over a whole repository (the
// SessionEnd hook only covers its own session, and only when it runs). It
// applies exactly remove's review gate, so uncollected work is reported and
// kept; the running session's trees and live sessions' trees are left alone
// unless asked for; orphaned branches are only deleted on Branches.

// SweepOptions tunes Sweep. OlderThanDays skips anything whose branch tip
// is newer. LiveWindow is how recently another session must have been seen
// for its trees to count as in use (0 disables liveness; the CLI default is
// DefaultLiveWindow).
type SweepOptions struct {
	OlderThanDays                                        *float64
	IncludeCurrent, IncludeLive, Branches, DryRun, Force bool
	LiveWindow                                           time.Duration
}

// SweepWorktree is one worktree's outcome.
type SweepWorktree struct {
	Task               string
	Branch             string
	Path               string
	Session            string
	Current            bool
	AgeDays            *float64
	SessionIdleMinutes *float64
	External           bool // adopted tree: no session_idle_minutes key
	Status             string
	Reason             string
	Files              []string
	HasFiles           bool // files key present (even when empty)
}

func (w SweepWorktree) MarshalJSON() ([]byte, error) {
	o := map[string]any{
		"task":     w.Task,
		"branch":   nullable(w.Branch),
		"path":     nullable(w.Path),
		"session":  nullable(w.Session),
		"current":  w.Current,
		"age_days": floatOrNil(w.AgeDays),
	}
	if !w.External {
		o["session_idle_minutes"] = floatOrNil(w.SessionIdleMinutes)
	}
	o["status"] = w.Status
	if w.Reason != "" {
		o["reason"] = w.Reason
	}
	if w.HasFiles {
		o["files"] = nonNil(w.Files)
	}
	return fsutil.MarshalJSON(o)
}

// SweepBranch is one orchestra branch without a worktree.
type SweepBranch struct {
	Branch  string
	AgeDays *float64
	Session string
	Status  string // orphan | would-delete | deleted | skipped | error
	Reason  string
}

func (b SweepBranch) MarshalJSON() ([]byte, error) {
	o := map[string]any{
		"branch":   b.Branch,
		"age_days": floatOrNil(b.AgeDays),
		"session":  nullable(b.Session),
		"status":   b.Status,
	}
	if b.Reason != "" {
		o["reason"] = b.Reason
	}
	return fsutil.MarshalJSON(o)
}

func floatOrNil(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

// SweepSummary counts the outcomes.
type SweepSummary struct {
	Removed             int `json:"removed"`
	WouldRemove         int `json:"would_remove"`
	Dirty               int `json:"dirty"`
	Skipped             int `json:"skipped"`
	External            int `json:"external"`
	Absent              int `json:"absent"`
	Errors              int `json:"errors"`
	BranchesOrphan      int `json:"branches_orphan"`
	BranchesDeleted     int `json:"branches_deleted"`
	BranchesWouldDelete int `json:"branches_would_delete"`
	TrashRemoved        int `json:"trash_removed"`
}

// SweepResult is `isolate sweep`'s output.
type SweepResult struct {
	Status    string // ok | error
	Reason    string // error only
	Repo      string
	DryRun    bool
	Pruned    bool
	Worktrees []SweepWorktree
	Branches  []SweepBranch
	Summary   SweepSummary
}

func (r SweepResult) MarshalJSON() ([]byte, error) {
	if r.Status == "error" {
		return fsutil.MarshalJSON(map[string]any{"status": r.Status, "reason": r.Reason})
	}
	o := map[string]any{"status": r.Status}
	wts, brs := r.Worktrees, r.Branches
	if wts == nil {
		wts = []SweepWorktree{}
	}
	if brs == nil {
		brs = []SweepBranch{}
	}
	o["repo"] = r.Repo
	o["dry_run"] = r.DryRun
	o["pruned"] = r.Pruned
	o["worktrees"] = wts
	o["branches"] = brs
	o["summary"] = r.Summary
	return fsutil.MarshalJSON(o)
}

// round returns x rounded to n decimal places.
func round(x float64, n int) float64 {
	p := math.Pow10(n)
	return math.Round(x*p) / p
}

func branchAgeDays(root, branch string, now time.Time) *float64 {
	if branch == "" {
		return nil
	}
	out, err := gitx.Run(root, "log", "-1", "--format=%ct", branch, "--")
	if err != nil {
		return nil
	}
	first, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	committed, err := strconv.ParseInt(strings.TrimSpace(first), 10, 64)
	if err != nil {
		return nil
	}
	nowSec := float64(now.UnixNano()) / 1e9
	age := round(math.Max(0, (nowSec-float64(committed))/86400.0), 2)
	return &age
}

// orphanBranches are orchestra/* branches with no worktree on them.
func orphanBranches(root string) []string {
	out, err := gitx.Run(root, "for-each-ref", "--format=%(refname:short)", "refs/heads/"+BranchPrefix)
	if err != nil {
		return nil
	}
	held := map[string]bool{}
	for _, wt := range worktrees(root) {
		if wt.Branch != "" {
			held[wt.Branch] = true
		}
	}
	var orphans []string
	for _, line := range strings.Split(out, "\n") {
		if b := strings.TrimSpace(line); b != "" && !held[b] {
			orphans = append(orphans, b)
		}
	}
	return orphans
}

// Sweep reclaims leftover orchestra worktrees across the repository
// containing root.
func Sweep(root string, o SweepOptions) (SweepResult, error) {
	resolved, ok := gitx.RepoRoot(root)
	if !ok {
		return SweepResult{Status: "error", Reason: "not a git repository"}, nil
	}
	// A hand-deleted worktree directory keeps its admin record; pruning
	// first shows the real surface. It mutates, so a dry run skips it.
	pruned := false
	if !o.DryRun {
		_, err := gitx.Run(resolved, "worktree", "prune")
		pruned = err == nil
	}
	current := sessionComponent(currentSession())
	now := time.Now()
	if !o.DryRun {
		pruneHeartbeats(now)
	}
	newer := func(age *float64) bool {
		return o.OlderThanDays != nil && (age == nil || *age < *o.OlderThanDays)
	}
	newerReason := func() string {
		return fmt.Sprintf("newer than --older-than %s day(s)", strconv.FormatFloat(*o.OlderThanDays, 'g', -1, 64))
	}

	res := SweepResult{Status: "ok", Repo: resolved, DryRun: o.DryRun, Pruned: pruned,
		Worktrees: []SweepWorktree{}, Branches: []SweepBranch{}}
	entries, _ := List(resolved, "")
	for _, e := range entries {
		session := string(e.Session)
		rec := SweepWorktree{
			Task: e.Task, Branch: e.Branch, Path: e.Path, Session: session,
			Current: session != "" && session == current,
			AgeDays: branchAgeDays(resolved, e.Branch, now),
		}
		if seen, ok := sessionLastSeen(session); ok {
			idle := round(math.Max(0, now.Sub(seen).Minutes()), 1)
			rec.SessionIdleMinutes = &idle
		}
		switch {
		case rec.Current && !o.IncludeCurrent:
			rec.Status = "skipped"
			rec.Reason = "belongs to the running session; pass --include-current to sweep it"
		case !rec.Current && !o.IncludeLive && sessionIsLive(session, o.LiveWindow, now):
			// Another session active moments ago is very likely mid-run.
			idle := 0.0
			if rec.SessionIdleMinutes != nil {
				idle = *rec.SessionIdleMinutes
			}
			rec.Status = "skipped"
			rec.Reason = fmt.Sprintf("session %s was active %.0f min ago; pass --include-live to sweep it", session, idle)
		case newer(rec.AgeDays):
			rec.Status = "skipped"
			rec.Reason = newerReason()
		case o.DryRun:
			rec.Status = "would-remove"
			if reason := inUse(resolved, e.Task, e.Path); reason != "" {
				rec.Status, rec.Reason = "busy", reason
			} else if !o.Force {
				files, reason, dirty, err := uncollected(resolved, e.Path, e.Task, e.Branch, session)
				if err != nil {
					return SweepResult{}, err
				}
				if dirty {
					rec.Status, rec.Reason, rec.Files, rec.HasFiles = "dirty", reason, files, true
				}
			}
		default:
			out, err := Remove(resolved, e.Task, o.Force, session)
			if err != nil {
				return SweepResult{}, err
			}
			rec.Status, rec.Reason = out.Status, out.Reason
			if len(out.Files) > 0 {
				rec.Files, rec.HasFiles = out.Files, true
			}
		}
		res.Worktrees = append(res.Worktrees, rec)
	}

	// Adopted trees are not orchestra branches: reported, never removed.
	for _, wt := range worktrees(resolved) {
		a, ok := readAdoption(wt.Path)
		if !ok {
			continue
		}
		res.Worktrees = append(res.Worktrees, SweepWorktree{
			Task: a.Task, Branch: wt.Branch, Path: wt.Path, Session: a.Session,
			AgeDays: branchAgeDays(resolved, wt.Branch, now), External: true,
			Status: "external", Reason: "adopted external worktree; orchestra never removes it",
		})
	}

	for _, branch := range orphanBranches(resolved) {
		rec := SweepBranch{Branch: branch, AgeDays: branchAgeDays(resolved, branch, now), Session: branchSession(branch)}
		switch {
		case newer(rec.AgeDays):
			rec.Status, rec.Reason = "skipped", newerReason()
		case !o.Branches:
			rec.Status, rec.Reason = "orphan", "no worktree; pass --branches to delete it"
		case o.DryRun:
			rec.Status = "would-delete"
		default:
			if _, err := gitx.Run(resolved, "branch", "-D", branch); err != nil {
				rec.Status, rec.Reason = "error", "git branch -D failed"
			} else {
				rec.Status = "deleted"
			}
		}
		res.Branches = append(res.Branches, rec)
	}
	res.Summary = summarize(res.Worktrees, res.Branches)
	if !o.DryRun {
		res.Summary.TrashRemoved = removeStaleTrash(entries)
	}
	return res, nil
}

// removeStaleTrash deletes trees a removal renamed aside and never finished
// deleting (.<name>.orchestra-removing-<pid>-<nanos>, next to the listed
// worktrees) once the process that renamed them is gone. Git already
// forgot them; only disk space is reclaimed. Returns how many went.
func removeStaleTrash(entries []Entry) int {
	parents := map[string]bool{}
	for _, e := range entries {
		if e.Path != "" {
			parents[filepath.Dir(e.Path)] = true
		}
	}
	n := 0
	for parent := range parents {
		matches, _ := filepath.Glob(filepath.Join(parent, ".*"+trashMarker+"*"))
		for _, m := range matches {
			_, tail, _ := strings.Cut(filepath.Base(m), trashMarker)
			pidText, _, _ := strings.Cut(tail, "-")
			pid, err := strconv.Atoi(pidText)
			if err != nil || pid == os.Getpid() || fsutil.PIDAlive(pid) {
				continue
			}
			if os.RemoveAll(m) == nil {
				n++
			}
		}
	}
	return n
}

func summarize(wts []SweepWorktree, brs []SweepBranch) SweepSummary {
	var s SweepSummary
	for _, w := range wts {
		switch w.Status {
		case "removed":
			s.Removed++
		case "would-remove":
			s.WouldRemove++
		case "dirty":
			s.Dirty++
		case "skipped", "busy":
			s.Skipped++
		case "external":
			s.External++
		case "absent":
			s.Absent++
		case "error":
			s.Errors++
		}
	}
	for _, b := range brs {
		switch b.Status {
		case "orphan":
			s.BranchesOrphan++
		case "deleted":
			s.BranchesDeleted++
		case "would-delete":
			s.BranchesWouldDelete++
		case "error":
			s.Errors++
		}
	}
	return s
}

// FormatSweepText is the compact `isolate sweep --text` rendering.
func FormatSweepText(r SweepResult) string {
	if r.Status == "error" {
		return "sweep error  " + r.Reason
	}
	head := "sweep " + r.Repo
	if r.DryRun {
		head += "  (dry-run)"
	}
	lines := []string{head}
	age := func(a *float64) string {
		if a == nil {
			return "-"
		}
		return fmt.Sprintf("%.1fd", *a)
	}
	for _, w := range r.Worktrees {
		path := w.Path
		if path == "" {
			path = "-"
		}
		reason := ""
		if w.Reason != "" {
			reason = "  (" + w.Reason + ")"
		}
		lines = append(lines, fmt.Sprintf("  %-12s %-16s %-9s %s%s", w.Status, w.Task, age(w.AgeDays), path, reason))
	}
	for _, b := range r.Branches {
		lines = append(lines, fmt.Sprintf("  %-12s %-16s %-9s %s", b.Status, "(branch)", age(b.AgeDays), b.Branch))
	}
	s := r.Summary
	lines = append(lines, fmt.Sprintf(
		"summary: removed %d, would-remove %d, dirty %d, skipped %d, external %d, errors %d, branches %d orphan / %d deleted",
		s.Removed, s.WouldRemove, s.Dirty, s.Skipped, s.External, s.Errors, s.BranchesOrphan, s.BranchesDeleted))
	if s.TrashRemoved > 0 {
		lines = append(lines, fmt.Sprintf("  trash removed %d (trees an interrupted remove left aside)", s.TrashRemoved))
	}
	if s.Dirty > 0 {
		lines = append(lines, "  inspect: agent-exec isolate diff --task <id>   |   "+
			"discard: agent-exec isolate sweep --force")
	}
	return strings.Join(lines, "\n")
}
