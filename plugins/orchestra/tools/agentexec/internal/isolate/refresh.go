package isolate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
)

// Refresh moves a task worktree's own changes onto a newer base: the
// task's diff is written to a patch file first (the safety net), then the
// worktree is recreated at the new base and the patch re-applied. An
// adopted tree is refreshed in place instead, since an external session
// lives in it. Conflicts are a normal outcome, reported like integrate's.

// patchesDirName is where refresh keeps its patch files, under Home(root).
const patchesDirName = ".patches"

// legacySession makes Create build a session-less branch name: it is not ""
// (which would mean "the calling session") and sessionComponent rejects it.
const legacySession = "-"

// Conflict is one conflicted file and how many conflict regions git left in
// it. Counted from the markers, never echoed.
type Conflict struct {
	File  string `json:"file"`
	Hunks int    `json:"hunks"`
}

// UnmergedConflicts lists the conflicted files of wt after a 3-way apply,
// from git's own reporting (the diff filter, else the raw unmerged index
// entries).
func UnmergedConflicts(wt string) []Conflict {
	var names []string
	if out, err := gitx.Run(wt, "diff", "--name-only", "--diff-filter=U"); err == nil {
		for _, n := range strings.Split(out, "\n") {
			if n = strings.TrimSpace(n); n != "" {
				names = append(names, n)
			}
		}
	}
	if len(names) == 0 {
		if out, err := gitx.Run(wt, "ls-files", "-u"); err == nil {
			seen := map[string]bool{}
			for _, line := range strings.Split(out, "\n") {
				_, name, ok := strings.Cut(line, "\t")
				name = strings.TrimSpace(name)
				if ok && name != "" && !seen[name] {
					seen[name] = true
					names = append(names, name)
				}
			}
		}
	}
	conflicts := []Conflict{}
	for _, name := range names {
		hunks := countConflictMarkers(filepath.Join(wt, name))
		if hunks == 0 {
			hunks = 1
		}
		conflicts = append(conflicts, Conflict{File: name, Hunks: hunks})
	}
	return conflicts
}

// countConflictMarkers counts lines starting with "<<<<<<<" (any of \n,
// \r\n, \r ends a line); 0 when unreadable.
func countConflictMarkers(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	text := strings.ReplaceAll(strings.ReplaceAll(string(b), "\r\n", "\n"), "\r", "\n")
	n := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "<<<<<<<") {
			n++
		}
	}
	return n
}

// RefreshResult is `isolate refresh`'s output. PatchFile is set whenever
// the task's work was saved, so it stays recoverable whatever failed later.
type RefreshResult struct {
	Status      string // ok | unchanged | conflicted | absent | error
	Task        string
	Path        string
	Branch      string
	OldBaseline string
	Baseline    string
	Files       []string
	Conflicts   []Conflict
	PatchFile   string
	Note        string
	InPlace     bool // adopted tree refreshed without recreation
}

func (r RefreshResult) MarshalJSON() ([]byte, error) {
	conflicts := r.Conflicts
	if conflicts == nil {
		conflicts = []Conflict{}
	}
	o := map[string]any{
		"status":       r.Status,
		"task":         r.Task,
		"path":         nullable(r.Path),
		"branch":       nullable(r.Branch),
		"old_baseline": nullable(r.OldBaseline),
		"baseline":     nullable(r.Baseline),
		"files":        nonNil(r.Files),
		"conflicts":    conflicts,
		"patch_file":   nullable(r.PatchFile),
		"note":         r.Note,
	}
	if r.InPlace {
		o["in_place"] = true
	}
	return fsutil.MarshalJSON(o)
}

// Refresh moves task's worktree onto onto ("" = HEAD of root). session ""
// is the calling session. Only an unusable task id or an ambiguous task is
// an error; everything else is in the result's Status.
func Refresh(root, task, onto, session string) (RefreshResult, error) {
	resolved, isRepo := gitx.RepoRoot(root)
	task, err := SanitizeTaskID(task)
	if err != nil {
		return RefreshResult{}, err
	}
	if !isRepo {
		return RefreshResult{Status: "error", Task: task, Note: "not a git repository"}, nil
	}
	wt, found, err := resolveWorktree(resolved, task, session)
	if err != nil {
		return RefreshResult{}, err
	}
	if !found {
		return RefreshResult{Status: "absent", Task: task, Note: "no worktree for " + task}, nil
	}
	res := RefreshResult{Task: task, Path: wt.Path, Branch: wt.Branch}
	fail := func(note string) (RefreshResult, error) {
		res.Status, res.Note = "error", note
		return res, nil
	}
	if ReadRole(wt.Path) == RoleIntegration {
		return fail("refusing to refresh an integration worktree")
	}
	ref := onto
	if ref == "" {
		ref = "HEAD"
	}
	ontoSHA := RevParseCommit(resolved, ref)
	if ontoSHA == "" {
		return fail(fmt.Sprintf("could not resolve --onto ref %q to a commit", ref))
	}
	res.OldBaseline, _ = ReadBaseline(wt.Path)
	if res.OldBaseline == ontoSHA {
		res.Status, res.Baseline = "unchanged", ontoSHA
		res.Note = "already on onto " + ontoSHA[:min(12, len(ontoSHA))]
		return res, nil
	}

	diff, err := Diff(resolved, task, session, true)
	if err != nil {
		return RefreshResult{}, err
	}
	if diff.Status != "ok" {
		note := diff.Reason
		if note == "" {
			note = "could not compute diff for " + task
		}
		return fail(note)
	}
	patch, files := diff.Patch, nonNil(diff.Files)
	hasWork := strings.TrimSpace(patch) != ""
	if hasWork {
		old := res.OldBaseline
		if old == "" {
			old = "none"
		}
		file, err := writePatchFile(Home(resolved), fmt.Sprintf("%s-%s-%s.patch", task, prefix12(old), prefix12(ontoSHA)), patch)
		if err != nil {
			return fail(fmt.Sprintf("could not write the patch file for %s; nothing touched", task))
		}
		res.PatchFile = file
	}

	if _, adopted := readAdoption(wt.Path); adopted {
		return refreshInPlace(res, ontoSHA, patch, files), nil
	}

	// The patch is the safety net now: bypass the collected gate and remove
	// the old worktree and branch outright.
	if _, err := gitx.Run(resolved, "worktree", "remove", wt.Path, "--force"); err != nil {
		return fail("could not remove the old worktree for " + task)
	}
	_, _ = gitx.Run(resolved, "branch", "-D", wt.Branch)

	// Recreate under the identical branch name (a legacy session-less one
	// stays session-less).
	sess := branchSession(wt.Branch)
	if sess == "" {
		sess = legacySession
	}
	carryDeps := true
	created, err := Create(resolved, task, CreateOptions{
		Backend: BackendGit, Onto: ontoSHA, Session: sess, Carry: false, CarryDeps: &carryDeps,
	})
	lost := RefreshResult{Status: "error", Task: task, OldBaseline: res.OldBaseline, PatchFile: res.PatchFile}
	if err != nil {
		lost.Note = fmt.Sprintf("isolate_create raised recreating %s: %s; recover from patch_file", task, err)
		return lost, nil
	}
	if created.Status == "error" || created.Path == "" || !isDir(created.Path) {
		lost.Note = fmt.Sprintf("could not recreate the worktree for %s; recover from patch_file", task)
		return lost, nil
	}
	res.Path, res.Baseline = created.Path, ontoSHA
	if !hasWork {
		res.Status, res.Note = "ok", "no work to carry for "+task
		return res, nil
	}
	res.Files = files
	if gitx.ApplyPatch3Way(created.Path, []byte(patch)) != nil {
		res.Status, res.Conflicts = "conflicted", UnmergedConflicts(created.Path)
		res.Note = fmt.Sprintf("%d file(s) conflicted; resolve in worktree at %s", len(res.Conflicts), created.Path)
		return res, nil
	}
	res.Status = "ok"
	res.Note = fmt.Sprintf("moved %d file(s) onto %s", len(files), prefix12(ontoSHA))
	return res, nil
}

// refreshInPlace refreshes an adopted tree without removing or recreating
// it: reset to onto, clear untracked (never ignored) files, re-apply the
// saved patch uncommitted.
func refreshInPlace(res RefreshResult, ontoSHA, patch string, files []string) RefreshResult {
	res.InPlace = true
	if _, err := gitx.Run(res.Path, "reset", "-q", "--hard", ontoSHA); err != nil {
		res.Status, res.Note = "error", fmt.Sprintf("git reset failed in %s; recover from patch_file", res.Path)
		return res
	}
	_, _ = gitx.Run(res.Path, "clean", "-fdq")
	_ = WriteBaseline(res.Path, ontoSHA)
	res.Baseline = ontoSHA
	if strings.TrimSpace(patch) == "" {
		res.Status, res.Note = "ok", "no work to carry for "+res.Task
		return res
	}
	res.Files = files
	if gitx.ApplyPatch3Way(res.Path, []byte(patch)) != nil {
		res.Status, res.Conflicts = "conflicted", UnmergedConflicts(res.Path)
		res.Note = fmt.Sprintf("%d file(s) conflicted; resolve in worktree at %s", len(res.Conflicts), res.Path)
		return res
	}
	res.Status = "ok"
	res.Note = fmt.Sprintf("moved %d file(s) onto %s in place", len(files), prefix12(ontoSHA))
	return res
}

// writePatchFile writes patch durably (fsync) as dir/name.
func writePatchFile(dir, name, patch string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dir = filepath.Join(dir, patchesDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return "", err
	}
	_, werr := f.WriteString(patch)
	serr := f.Sync()
	cerr := f.Close()
	for _, e := range []error{werr, serr, cerr} {
		if e != nil {
			return "", e
		}
	}
	return path, nil
}

// RevParseCommit resolves ref to a commit SHA in root, "" when it does not
// name one.
func RevParseCommit(root, ref string) string {
	out, err := gitx.Run(root, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func prefix12(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// FormatRefreshText is the compact `isolate refresh --text` rendering (no
// patch text).
func FormatRefreshText(r RefreshResult) string {
	at := r.Baseline
	if at == "" {
		at = r.OldBaseline
	}
	if at == "" {
		at = "-"
	}
	path := r.Path
	if path == "" {
		path = "-"
	}
	lines := []string{fmt.Sprintf("refresh %-10s task %-16s onto %s  worktree %s", r.Status, r.Task, prefix12(at), path)}
	for _, f := range r.Files {
		lines = append(lines, "  "+f)
	}
	if len(r.Conflicts) > 0 {
		lines = append(lines, "  conflicts: "+FormatConflicts(r.Conflicts))
	}
	if r.PatchFile != "" {
		lines = append(lines, "  patch: "+r.PatchFile)
	}
	lines = append(lines, "note: "+r.Note)
	return strings.Join(lines, "\n")
}

// FormatConflicts renders conflicts as "file (N hunk(s)), ...".
func FormatConflicts(cs []Conflict) string {
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = fmt.Sprintf("%s (%d hunk(s))", c.File, c.Hunks)
	}
	return strings.Join(parts, ", ")
}
