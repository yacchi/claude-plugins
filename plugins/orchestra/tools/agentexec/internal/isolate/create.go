package isolate

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
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
type CreateOptions struct {
	Backend, Onto, Session string
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
	Baseline       string // created only
	Carried        []string
	CarryMethod    string // none | clone | copy | failed
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
// root. Idempotent per task and session.
func Create(root, task string, o CreateOptions) (CreateResult, error) {
	resolved, ok := gitx.RepoRoot(root)
	if !ok {
		return CreateResult{Status: "error", Reason: "not a git repository"}, nil
	}
	task, err := SanitizeTaskID(task)
	if err != nil {
		return CreateResult{}, err
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
			Backend: backendExisting, Carried: []string{}, CarryMethod: carryNone,
			CarriedFiles: []string{},
		}, nil
	}

	backend := o.Backend
	if backend == "" {
		backend = BackendAuto
	}
	if o.Onto != "" {
		// gtr only branches from the current checkout.
		backend = BackendGit
	}
	if backend == BackendAuto {
		backend = BackendGit
		if gtrAvailable() {
			backend = BackendGtr
		}
	}
	path := ""
	if backend == BackendGtr {
		path = createViaGtr(resolved, branch, gtrConfigPairs(resolved))
		if path == "" {
			backend = BackendGit // a worktree still beats a shared tree
		}
	}
	if path == "" {
		start := o.Onto
		if start == "" {
			start = "HEAD"
		}
		path = createViaGit(resolved, branch, start)
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
		// the worker's changes and nothing else.
		_, _ = gitx.Run(path, "add", "-A")
		_, _ = gitx.Run(path, "-c", "user.email=orchestra@local", "-c", "user.name=orchestra",
			"commit", "-q", "--allow-empty", "-m", "orchestra baseline for "+task)
	}
	baseline := ""
	if out, err := gitx.Run(path, "rev-parse", "HEAD"); err == nil {
		baseline = strings.TrimSpace(out)
	}
	if baseline != "" {
		_ = WriteBaseline(path, baseline)
	}

	res := CreateResult{
		Status: "created", Task: task, Branch: branch, Path: path, Backend: backend,
		Baseline: baseline, Carried: []string{}, CarryMethod: carryNone, CarriedFiles: []string{},
	}
	if carryDeps {
		res.Carried, res.CarryMethod, res.AlreadyPresent = carryDependencies(resolved, path, o.CarryExtra)
		res.CarriedFiles = carryLocalFiles(resolved, path, carryFiles)
	}
	// The ignored surface exactly as the worker receives it.
	_ = writeCreatedIgnored(path, ignoredDigest(IgnoredEntries(path)))

	switch res.CarryMethod {
	case carryCopy:
		res.CarryNote = "CoW was unavailable; carrying dependencies cost real time and disk."
	case carryFailed:
		res.CarryNote = "Workers need a dependency install; run it ONCE with a cheap agent, not per worker."
	}
	return res, nil
}

// createViaGit adds the worktree under Home(root); "" when git refused.
func createViaGit(root, branch, start string) string {
	name := strings.ReplaceAll(strings.TrimPrefix(branch, BranchPrefix), "/", "-")
	path := filepath.Join(Home(root), name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return ""
	}
	if _, err := gitx.Run(root, "worktree", "add", "-q", "-b", branch, path, start); err != nil {
		return ""
	}
	if !isDir(path) {
		return ""
	}
	return path
}
