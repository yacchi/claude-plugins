package isolate

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// Backends of Create.
const (
	BackendAuto     = "auto"
	BackendGtr      = "gtr"
	BackendGit      = "git"
	backendExisting = "existing"
)

// CreateOptions tunes Create.
//
// Onto names the commit the worktree starts from. Empty means "HEAD plus the
// user's uncommitted work", which is what a worker needs; a ref (integration
// worktrees pass one) gives exactly that commit, with no uncommitted work
// carried and no baseline commit layered on top.
//
// Carry copies gitignored dependency dirs and the CarryFiles into the tree.
// CarryDeps overrides Carry for that copy when non-nil (refresh passes
// Carry=false, CarryDeps=true). Uncommitted work is always reproduced when
// Onto is empty; without carry the CarryFiles are excluded from it.
// CarryFiles nil means DefaultCarryFiles. Session "" is the calling session.
//
// Dir places the tree at that path instead of the default place (treeHome).
// It forces the git backend, since gtr names the tree's directory itself, and
// refuses a path that exists and is not an empty directory, so a user's files
// are never adopted as a tree.
type CreateOptions struct {
	Backend, Onto, Session string
	Role                   string // "" work tree | RoleIntegration | RoleAssembly
	Parent                 string // path of the parent tree; "" = none
	Dir                    string // the new tree's path; "" = the default place (treeHome). Forces the git backend
	Carry                  bool
	CarryExtra             []string
	CarryDeps              *bool
	CarryFiles             []string
}

// CreateResult is `isolate create`'s output.
type CreateResult struct {
	Status         string // created | exists | error
	Reason         string // error only
	Task           string
	Branch         string
	Path           string
	Backend        string // gtr | git | existing
	Role           string // work | integration | assembly; created and exists only
	Parent         string // real path of the parent tree, "" = none
	Baseline       string // created only
	Carried        []string
	CarryMethod    string // none | clone | copy | failed | pnpm-gvs
	CarriedFiles   []string
	AlreadyPresent []string
	CarryNote      string
}

func (r CreateResult) MarshalJSON() ([]byte, error) {
	if r.Status == "error" {
		return fsutil.MarshalJSON(map[string]any{"status": r.Status, "reason": r.Reason})
	}
	o := map[string]any{"status": r.Status}
	o["task"] = r.Task
	o["branch"] = r.Branch
	o["path"] = r.Path
	o["backend"] = r.Backend
	o["role"] = r.Role
	o["parent"] = nullable(r.Parent)
	if r.Status == "created" {
		o["baseline"] = nullable(r.Baseline)
	}
	o["carried"] = nonNil(r.Carried)
	o["carry_method"] = r.CarryMethod
	o["carried_files"] = nonNil(r.CarriedFiles)
	if len(r.AlreadyPresent) > 0 {
		o["already_present"] = r.AlreadyPresent
	}
	if r.CarryNote != "" {
		o["carry_note"] = r.CarryNote
	}
	return fsutil.MarshalJSON(o)
}

// joinNote joins notes a and b with "; ", either of which may be empty.
func joinNote(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "; " + b
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Create makes (or returns) task's worktree in the repository containing
// root. Idempotent per task and session. A created tree records its role and
// parent in its git dir, so child counts are derived from git and never from
// Orca; a reused tree keeps the markers it has.
func Create(root, task string, o CreateOptions) (CreateResult, error) {
	resolved, ok := gitx.RepoRoot(root)
	if !ok {
		return CreateResult{Status: "error", Reason: "not a git repository"}, nil
	}
	task, err := SanitizeTaskID(task)
	if err != nil {
		return CreateResult{}, err
	}
	// Before the existing-branch check and any git write: an invalid parent
	// fails even when the task's tree is already there, and leaves nothing.
	if reason := lineageError(resolved, o); reason != "" {
		return CreateResult{Status: "error", Reason: reason}, nil
	}
	branch := branchName(task, sessionComponent(orCurrent(o.Session)))
	carryFiles := o.CarryFiles
	if carryFiles == nil {
		carryFiles = DefaultCarryFiles
	}

	if existing := worktreeForBranch(resolved, branch); existing != "" {
		// A tree reused across retry rounds may predate the marker.
		if readCreatedIgnored(existing) == "" {
			_ = writeCreatedIgnored(existing, ignoredDigest(IgnoredEntries(existing)))
		}
		return CreateResult{
			Status: "exists", Task: task, Branch: branch, Path: existing,
			Backend: backendExisting, Role: RoleName(existing), Parent: ReadParent(existing),
			Carried: []string{}, CarryMethod: carryNone, CarriedFiles: []string{},
		}, nil
	}

	// Refused before any git write, so a bad path leaves no branch behind.
	dir := o.Dir
	if dir != "" {
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
		if occupiedDir(dir) {
			return CreateResult{Status: "error", Reason: dir + " exists and is not an empty directory"}, nil
		}
	}

	backend := o.Backend
	if backend == "" {
		backend = BackendAuto
	}
	if o.Onto != "" || dir != "" {
		// gtr only branches from the current checkout and names the tree's directory itself.
		backend = BackendGit
	}
	if backend == BackendAuto {
		backend = BackendGit
		if gtrAvailable() {
			backend = BackendGtr
		}
	}
	target := dir
	if target == "" {
		target = filepath.Join(treeHome(resolved, o.Parent), treeLeaf(branch))
	}
	path := ""
	if backend == BackendGtr {
		path = createViaGtr(resolved, branch, filepath.Dir(target), gtrConfigPairs(resolved))
		if path == "" {
			backend = BackendGit // a worktree still beats a shared tree
		}
	}
	if path == "" {
		start := o.Onto
		if start == "" {
			start = "HEAD"
		}
		path = createViaGit(resolved, branch, start, target)
	}
	if path == "" {
		return CreateResult{Status: "error", Reason: "could not create a worktree for " + branch}, nil
	}

	carryDeps := o.Carry
	if o.CarryDeps != nil {
		carryDeps = *o.CarryDeps
	}
	if o.Onto == "" {
		var excluded []string
		if !carryDeps {
			excluded = carryFiles
		}
		carryUncommitted(resolved, path, excluded)
		// The user's state becomes the baseline, so every later diff shows
		// the worker's changes and nothing else. A store a gtr hook or copy
		// put into the tree must not enter it either.
		if err := gitx.CommitAll(path, "orchestra baseline for "+task, CurrentFilter().Pathspec()...); err != nil {
			// A tree without its baseline would show the carried state as
			// the worker's own changes; it holds only copies, so drop it.
			reason := "could not commit the baseline for " + branch + ": " + err.Error()
			if _, why := discard(resolved, path, branch); why != "" {
				reason += "; " + why
			}
			return CreateResult{Status: "error", Reason: reason}, nil
		}
	}
	baseline := ""
	if out, err := gitx.Run(path, "rev-parse", "HEAD"); err == nil {
		baseline = strings.TrimSpace(out)
	}
	if baseline != "" {
		_ = WriteBaseline(path, baseline)
	}
	if err := writeLineage(path, o); err != nil {
		// Without its markers the tree would sit in the wrong place in the
		// hierarchy, so drop it as a failed baseline does.
		reason := "could not record the lineage of " + branch + ": " + err.Error()
		if _, why := discard(resolved, path, branch); why != "" {
			reason += "; " + why
		}
		return CreateResult{Status: "error", Reason: reason}, nil
	}

	res := CreateResult{
		Status: "created", Task: task, Branch: branch, Path: path, Backend: backend,
		Role: RoleName(path), Parent: ReadParent(path),
		Baseline: baseline, Carried: []string{}, CarryMethod: carryNone, CarriedFiles: []string{},
	}
	if carryDeps {
		res.Carried, res.CarryMethod, res.AlreadyPresent, res.CarryNote = CarryDependencies(resolved, path, o.CarryExtra)
		res.CarriedFiles = carryLocalFiles(resolved, path, carryFiles)
	}
	// The ignored surface exactly as the worker receives it.
	_ = writeCreatedIgnored(path, ignoredDigest(IgnoredEntries(path)))

	switch res.CarryMethod {
	case carryCopy:
		res.CarryNote = joinNote(res.CarryNote, "CoW was unavailable; carrying dependencies cost real time and disk.")
	case carryFailed:
		res.CarryNote = joinNote(res.CarryNote, "Workers need a dependency install; run it ONCE with a cheap agent, not per worker.")
	}
	return res, nil
}

// lineageError is why o's role and parent are refused, "" when they are
// accepted. The checks run in the order the messages are listed in the spec.
func lineageError(resolved string, o CreateOptions) string {
	switch o.Role {
	case "", RoleIntegration, RoleAssembly:
	default:
		return "unknown role " + o.Role
	}
	if o.Role == RoleAssembly && o.Parent != "" {
		return "an assembly worktree has no parent"
	}
	if o.Parent == "" {
		return ""
	}
	if !isWorktreeOf(resolved, o.Parent) {
		return "parent " + o.Parent + " is not a worktree of this repository"
	}
	if o.Role == "" && ReadRole(o.Parent) != RoleIntegration {
		return "parent " + o.Parent + " is not an integration tree"
	}
	if o.Role == RoleIntegration && ReadRole(o.Parent) != RoleAssembly {
		return "parent " + o.Parent + " is not an assembly worktree"
	}
	return ""
}

// isWorktreeOf reports whether path is one of the repository's worktrees.
func isWorktreeOf(resolved, path string) bool {
	list, err := gitx.WorktreeList(resolved)
	if err != nil {
		return false
	}
	for _, w := range list {
		if paths.RealPath(w.Path) == paths.RealPath(path) {
			return true
		}
	}
	return false
}

// writeLineage records o's role and parent in the new tree's git dir.
func writeLineage(wt string, o CreateOptions) error {
	if o.Role != "" {
		if err := WriteRole(wt, o.Role); err != nil {
			return err
		}
	}
	if o.Parent != "" {
		return WriteParent(wt, o.Parent)
	}
	return nil
}

// occupiedDir reports whether dir exists and is not an empty directory. A dir
// that cannot be inspected counts as occupied: git must not write into it.
func occupiedDir(dir string) bool {
	st, err := os.Stat(dir)
	if os.IsNotExist(err) {
		return false
	}
	if err != nil || !st.IsDir() {
		return true
	}
	entries, err := os.ReadDir(dir)
	return err != nil || len(entries) > 0
}

// workDirName is the request workspace's directory of the work trees cut
// from its integration trees: <workspace>/.work/<assembly name>/<leaf>.
const workDirName = ".work"

// treeHome is the directory Create puts a tree in when CreateOptions.Dir is
// "": <workspace>/.work/<assembly name> when parent descends from the
// assembly of a request, else Home(resolved).
func treeHome(resolved, parent string) string {
	if ws, name, ok := requestOfParent(parent); ok {
		return filepath.Join(ws, workDirName, name)
	}
	return Home(resolved)
}

// requestOfParent follows parent markers from parent (parent included) to
// the first assembly. ok is true when that assembly's directory holds a
// request manifest: workspace is that directory and name the assembly's
// base name.
func requestOfParent(parent string) (workspace, name string, ok bool) {
	for cur, depth := parent, 0; cur != "" && depth < maxLineageDepth; depth++ {
		if ReadRole(cur) == RoleAssembly {
			ws := filepath.Dir(cur)
			if st, err := os.Stat(filepath.Join(ws, requestFile)); err == nil && st.Mode().IsRegular() {
				return ws, filepath.Base(cur), true
			}
			return "", "", false
		}
		cur = ReadParent(cur)
	}
	return "", "", false
}

// treeLeaf is the directory name of branch's tree: the branch without
// "orchestra/", with "/" replaced by "-".
func treeLeaf(branch string) string {
	return strings.ReplaceAll(strings.TrimPrefix(branch, BranchPrefix), "/", "-")
}

// createViaGit adds the worktree at path, creating missing parent
// directories; "" when git refused.
func createViaGit(root, branch, start, path string) string {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return ""
	}
	release := HoldHeavy("git worktree add")
	_, err := gitx.Run(root, "worktree", "add", "-q", "-b", branch, path, start)
	release()
	if err != nil {
		return ""
	}
	if !isDir(path) {
		return ""
	}
	return path
}
