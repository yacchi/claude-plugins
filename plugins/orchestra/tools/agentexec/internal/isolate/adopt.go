package isolate

import (
	"fmt"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// Adopt registers a worktree orchestra did not create (an external app's,
// such as Orca's) as a task's worktree, so diff, check, collect, integrate
// and refresh reach it by task id. The mapping is a marker in the tree's git
// dir; the tree itself is never deleted by orchestra.

// AdoptOptions tunes Adopt. Baseline "" means the tree's current HEAD.
// Session "" is the calling session. Carry copies dependency dirs and
// CarryFiles (nil = DefaultCarryFiles) into the tree.
type AdoptOptions struct {
	Baseline, Session string
	Carry             bool
	CarryExtra        []string
	CarryFiles        []string
	Parent            string // path of the parent integration tree; "" = none
}

// AdoptResult is the output of `isolate adopt` and `isolate unadopt`
// (Unadopt sets IsUnadopt: its shape is only status, task and path).
type AdoptResult struct {
	Status         string // adopted | exists | error; unadopted | absent
	Task           string
	Path           string
	Branch         string
	Baseline       string
	Parent         string
	Note           string
	Carried        []string
	CarryMethod    string
	CarriedFiles   []string
	AlreadyPresent []string
	IsUnadopt      bool
}

func (r AdoptResult) MarshalJSON() ([]byte, error) {
	o := map[string]any{"status": r.Status, "task": r.Task, "path": nullable(r.Path)}
	if r.IsUnadopt {
		return fsutil.MarshalJSON(o)
	}
	method := r.CarryMethod
	if method == "" {
		method = carryNone
	}
	o["branch"] = nullable(r.Branch)
	o["baseline"] = nullable(r.Baseline)
	if r.Status == "adopted" || r.Status == "exists" {
		o["parent"] = nullable(r.Parent)
	}
	o["note"] = r.Note
	o["carried"] = nonNil(r.Carried)
	o["carry_method"] = method
	o["carried_files"] = nonNil(r.CarriedFiles)
	if len(r.AlreadyPresent) > 0 {
		o["already_present"] = r.AlreadyPresent
	}
	return fsutil.MarshalJSON(o)
}

// Adopt registers path, a linked worktree of root that orchestra did not
// create, as task's worktree. Re-adopting the same path under the same task
// is "exists" and keeps the baseline unless o.Baseline is given; likewise the
// parent marker is kept unless o.Parent is given. Only an
// unusable task id is an error; refusals are in the result's Status.
func Adopt(root, task, path string, o AdoptOptions) (AdoptResult, error) {
	resolved, isRepo := gitx.RepoRoot(root)
	task, err := SanitizeTaskID(task)
	if err != nil {
		return AdoptResult{}, err
	}
	res := AdoptResult{Task: task}
	fail := func(note string) (AdoptResult, error) {
		res.Status, res.Note = "error", note
		return res, nil
	}
	if !isRepo {
		return fail("not a git repository")
	}

	target := ""
	if path != "" {
		target = paths.RealPath(path)
	}
	all := worktrees(resolved)
	index := -1
	for i, wt := range all {
		if paths.RealPath(wt.Path) == target {
			index = i
			break
		}
	}
	res.Path = path
	if index < 0 {
		return fail(path + " is not a worktree of this repository")
	}
	wt := all[index]
	res.Path, res.Branch = wt.Path, wt.Branch
	if index == 0 {
		return fail("refusing to adopt the main worktree")
	}
	if strings.HasPrefix(wt.Branch, BranchPrefix) {
		return fail(wt.Path + " is an orchestra-created worktree; it already has a task")
	}
	existing, isAdopted := readAdoption(wt.Path)
	if isAdopted && existing.Task != task {
		return fail(fmt.Sprintf("%s is already adopted as task %s", wt.Path, existing.Task))
	}
	// The task id must not already name some other tree.
	for _, other := range all {
		if paths.RealPath(other.Path) == target {
			continue
		}
		a, ok := readAdoption(other.Path)
		if (ok && a.Task == task) || other.Branch == branchName(task, "") ||
			(strings.HasPrefix(other.Branch, BranchPrefix) && strings.HasSuffix(other.Branch, "/"+task)) {
			return fail(fmt.Sprintf("task %s already resolves to %s", task, other.Path))
		}
	}

	if o.Parent != "" && (!isWorktreeOf(resolved, o.Parent) || ReadRole(o.Parent) != RoleIntegration) {
		return fail("parent " + o.Parent + " is not an integration tree")
	}

	baseline := ""
	if o.Baseline != "" {
		if baseline = RevParseCommit(resolved, o.Baseline); baseline == "" {
			return fail(fmt.Sprintf("could not resolve --baseline ref %q to a commit", o.Baseline))
		}
	}
	if isAdopted {
		if baseline != "" {
			_ = WriteBaseline(wt.Path, baseline)
		}
		if o.Parent != "" && WriteParent(wt.Path, o.Parent) != nil {
			return fail("could not write the parent marker for " + wt.Path)
		}
		res.Status, res.Note = "exists", "already adopted as "+task
		res.Baseline, _ = ReadBaseline(wt.Path)
		res.Parent = ReadParent(wt.Path)
		return res, nil
	}
	if baseline == "" {
		out, err := gitx.Run(wt.Path, "rev-parse", "HEAD")
		if baseline = strings.TrimSpace(out); err != nil || baseline == "" {
			return fail("could not read HEAD of " + wt.Path)
		}
	}
	if _, err := gitx.GitDir(wt.Path); err != nil {
		return fail("could not locate the git dir of " + wt.Path)
	}
	if writeAdoption(wt.Path, task, o.Session) != nil {
		return fail("could not write the adoption marker for " + wt.Path)
	}
	if o.Parent != "" && WriteParent(wt.Path, o.Parent) != nil {
		_ = ClearAdopted(wt.Path)
		return fail("could not write the parent marker for " + wt.Path)
	}
	_ = WriteBaseline(wt.Path, baseline)
	res.CarryMethod = carryNone
	carryNote := ""
	if o.Carry {
		res.Carried, res.CarryMethod, res.AlreadyPresent, carryNote = CarryDependencies(resolved, wt.Path, o.CarryExtra)
		files := o.CarryFiles
		if files == nil {
			files = DefaultCarryFiles
		}
		res.CarriedFiles = carryLocalFiles(resolved, wt.Path, files)
	}
	_ = writeCreatedIgnored(wt.Path, ignoredDigest(IgnoredEntries(wt.Path)))
	res.Status, res.Baseline = "adopted", baseline
	res.Parent = ReadParent(wt.Path)
	res.Note = joinNote(fmt.Sprintf("adopted %s as task %s", wt.Path, task), carryNote)
	return res, nil
}

// Unadopt drops task's adoption (markers only: files and the worktree are
// never touched). session "" is the calling session, whose marker wins when
// several trees carry the task id; more than one otherwise is an error.
func Unadopt(root, task, session string) (AdoptResult, error) {
	resolved, isRepo := gitx.RepoRoot(root)
	task, err := SanitizeTaskID(task)
	if err != nil {
		return AdoptResult{}, err
	}
	res := AdoptResult{Status: "absent", Task: task, IsUnadopt: true}
	if !isRepo {
		return res, nil
	}
	wt, found, err := resolveAdopted(worktrees(resolved), task, sessionComponent(orCurrent(session)))
	if err != nil {
		return AdoptResult{}, err
	}
	if !found {
		return res, nil
	}
	_ = ClearAdopted(wt.Path)
	res.Status, res.Path = "unadopted", wt.Path
	return res, nil
}

// FormatAdoptText is the compact --text rendering of Adopt and Unadopt.
func FormatAdoptText(r AdoptResult) string {
	path := r.Path
	if path == "" {
		path = "-"
	}
	lines := []string{fmt.Sprintf("%s task %-16s worktree %s", r.Status, r.Task, path)}
	if r.Baseline != "" {
		lines = append(lines, "  baseline: "+prefix12(r.Baseline))
	}
	if r.Note != "" {
		lines = append(lines, "note: "+r.Note)
	}
	return strings.Join(lines, "\n")
}
