package isolate

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// What to do with the uncommitted work of a checkout that an assembly starts
// from. Without a choice, creation refuses: starting from HEAD silently would
// drop work the user may still need.
const (
	UncommittedInclude = "include"
	UncommittedIgnore  = "ignore"
)

// includedFile: in an assembly whose baseline carries the user's uncommitted
// work, the commit that work sat on; land checks ancestry against it.
const includedFile = "orchestra-included"

// EnsureResult is the outcome of EnsureAssembly and CheckAssembly, and the
// shape of `isolate assembly`'s JSON.
type EnsureResult struct {
	Status      string   // ready | uncommitted | error
	Request     string   // ready: the request id
	Dir         string   // ready: the request workspace
	Assembly    string   // ready: the assembly's path
	Task        string   // ready: the assembly's task id (= the request id)
	Uncommitted string   // ready: clean | include | ignore
	Files       []string // ready: the paths included/ignored at creation; uncommitted: the checkout's changes
	Created     []string // ready: what this call made, "assembly" then "integration"
	Into        string   // EnsureIntegration only
	Integration string   // EnsureIntegration only: the integration tree's path
	Head        string   // EnsureIntegration only: its HEAD
	Checkout    string   // uncommitted: the checkout looked at
	Reason      string   // uncommitted | error
}

func (r EnsureResult) MarshalJSON() ([]byte, error) {
	switch r.Status {
	case "error":
		return fsutil.MarshalJSON(map[string]any{"status": r.Status, "reason": r.Reason})
	case "uncommitted":
		return fsutil.MarshalJSON(map[string]any{
			"status": r.Status, "checkout": r.Checkout, "files": nonNil(r.Files), "reason": r.Reason,
		})
	}
	o := map[string]any{
		"status": r.Status, "request": r.Request, "dir": r.Dir, "assembly": r.Assembly, "task": r.Task,
		"uncommitted": r.Uncommitted, "files": nonNil(r.Files), "created": nonNil(r.Created),
	}
	if r.Into != "" {
		o["into"] = r.Into
		o["integration"] = r.Integration
		o["head"] = r.Head
	}
	return fsutil.MarshalJSON(o)
}

// EnsureAssembly is the calling session's request assembly of the repository
// containing root, created from the user's checkout when the request has none
// yet. uncommitted is the choice for a checkout with uncommitted work: "",
// UncommittedInclude or UncommittedIgnore. Only creation looks at the
// checkout; an existing assembly is returned as it is.
func EnsureAssembly(root, uncommitted string) (EnsureResult, error) {
	if err := checkUncommitted(uncommitted); err != nil {
		return EnsureResult{}, err
	}
	return runAssembly(root, func(rootDir string, open []Request, resolved, common string) (EnsureResult, error) {
		return ensureAssembly(rootDir, open, resolved, common, uncommitted)
	})
}

// CheckAssembly reports what EnsureAssembly would do, without creating
// anything: the existing assembly, or the uncommitted refusal, or the
// readiness of a creation.
func CheckAssembly(root, uncommitted string) (EnsureResult, error) {
	if err := checkUncommitted(uncommitted); err != nil {
		return EnsureResult{}, err
	}
	return runAssembly(root, func(_ string, open []Request, resolved, common string) (EnsureResult, error) {
		return checkAssembly(open, resolved, common, uncommitted)
	})
}

// UserCheckout is the repository root of dir and whether it is the user's own
// checkout: not an orchestra tree (role, baseline or adoption marker, or an
// orchestra branch checked out). resolved is "" outside a repository.
func UserCheckout(dir string) (string, bool) {
	resolved, ok := gitx.RepoRoot(dir)
	if !ok {
		return "", false
	}
	if ReadRole(resolved) != "" || readMarker(resolved, baselineFile) != "" {
		return resolved, false
	}
	if _, adopted := ReadAdopted(resolved); adopted {
		return resolved, false
	}
	// Detached HEAD gives no branch, which is not an orchestra branch.
	out, _ := gitx.Run(resolved, "symbolic-ref", "--short", "-q", "HEAD")
	return resolved, !strings.HasPrefix(strings.TrimSpace(out), BranchPrefix)
}

func checkUncommitted(v string) error {
	switch v {
	case "", UncommittedInclude, UncommittedIgnore:
		return nil
	}
	return fmt.Errorf("invalid --uncommitted value %q: include or ignore", v)
}

// runAssembly resolves the repository of root and runs fn with the request
// root locked. Every failure is an error status, not an error return: the
// caller has already validated its input.
func runAssembly(root string, fn func(rootDir string, open []Request, resolved, common string) (EnsureResult, error)) (EnsureResult, error) {
	resolved, ok := gitx.RepoRoot(root)
	if !ok {
		return ensureError("not a git repository"), nil
	}
	common, err := RepoCommon(resolved)
	if err != nil {
		return ensureError(err.Error()), nil
	}
	var res EnsureResult
	err = withRequests(func(rootDir string, open []Request) error {
		var err error
		res, err = fn(rootDir, open, resolved, common)
		return err
	})
	if err != nil {
		return ensureError(err.Error()), nil
	}
	return res, nil
}

// liveAssembly is the calling session's request entry for common, when that
// entry's assembly still exists.
func liveAssembly(open []Request, common string) (Request, RequestRepo, bool) {
	cur, ok := pickCurrent(open)
	if !ok {
		return Request{}, RequestRepo{}, false
	}
	rr, ok := cur.Repo(common)
	if !ok || !AssemblyAlive(rr) {
		return Request{}, RequestRepo{}, false
	}
	return cur, rr, true
}

// ensureAssembly is EnsureAssembly under the request lock. Creation runs in
// this order, and each refusal leaves nothing behind.
func ensureAssembly(rootDir string, open []Request, resolved, common, uncommitted string) (EnsureResult, error) {
	if cur, rr, ok := liveAssembly(open, common); ok {
		return readyEnsure(cur, rr, nil), nil
	}
	if _, ok := UserCheckout(resolved); !ok {
		return ensureError(fmt.Sprintf("a request's assembly starts from your own checkout, and %s is an orchestra tree", resolved)), nil
	}
	head := RevParseCommit(resolved, "HEAD")
	if head == "" {
		return ensureError(fmt.Sprintf("could not resolve HEAD in %s", resolved)), nil
	}
	// A failed scan is not a refusal: the assembly is still created from HEAD.
	if dirs, err := TrackedExcluded(resolved, head, CurrentFilter()); err == nil && len(dirs) > 0 {
		return ensureError(TrackedRefusal(head, dirs)), nil
	}
	files, err := checkoutChanges(resolved)
	if err != nil {
		return ensureError(fmt.Sprintf("cannot read the checkout's status: %v", err)), nil
	}
	if len(files) > 0 && uncommitted == "" {
		return uncommittedRefusal(resolved, files), nil
	}

	req, found := pickCurrent(open)
	if !found {
		req = Request{
			ID:      newRequestID(rootDir),
			Session: currentSession(),
			Created: requestNow().Format(time.RFC3339),
		}
		req.Dir = filepath.Join(rootDir, req.ID)
		if err := saveRequest(req); err != nil {
			removeWorkspace(req.Dir)
			return ensureError(err.Error()), nil
		}
	}
	// fail ends a request this call created, so a refusal leaves no workspace.
	fail := func(reason string) (EnsureResult, error) {
		if !found {
			removeWorkspace(req.Dir)
		}
		return ensureError(reason), nil
	}

	name, err := assemblyName(resolved, req, common)
	if err != nil {
		return fail(err.Error())
	}
	mode := uncommittedMode(uncommitted, files)
	o := CreateOptions{Backend: BackendGit, Onto: head, Role: RoleAssembly, Dir: filepath.Join(req.Dir, name)}
	if mode == UncommittedInclude {
		// An empty Onto copies the checkout's uncommitted work into a baseline commit.
		o.Onto = ""
	}
	res, err := Create(resolved, req.ID, o)
	if err != nil {
		return fail("could not create the assembly: " + err.Error())
	}
	if res.Status == "error" {
		return fail("could not create the assembly: " + res.Reason)
	}
	if res.Status == "exists" && res.Role != RoleAssembly {
		return fail(fmt.Sprintf("branch %s is checked out in %s, which is not an assembly", res.Branch, res.Path))
	}
	path := res.Path
	created := res.Status == "created"
	// dropTree removes a tree this call made before the refusal it reports.
	dropTree := func(reason string) (EnsureResult, error) {
		if !created {
			return fail(reason)
		}
		if _, why := discard(resolved, path, res.Branch); why != "" {
			reason += "; " + why
		}
		return fail(reason)
	}
	// The marker is written only for a tree this call made: an existing one
	// keeps the baseline it was created with.
	if mode == UncommittedInclude && created {
		if err := writeMarker(path, includedFile, head); err != nil {
			return dropTree("could not record the included work: " + err.Error())
		}
	}

	rr := RequestRepo{
		Common:      common,
		Checkout:    resolved,
		Name:        name,
		Assembly:    paths.RealPath(path),
		Task:        req.ID,
		Uncommitted: mode,
		Files:       nonNil(files),
	}
	replaced := false
	for i := range req.Repos {
		if req.Repos[i].Common == common {
			req.Repos[i] = rr
			replaced = true
		}
	}
	if !replaced {
		req.Repos = append(req.Repos, rr)
	}
	if err := saveRequest(req); err != nil {
		return dropTree("could not record the request: " + err.Error())
	}
	var made []string
	if created {
		made = []string{"assembly"}
	}
	return readyEnsure(req, rr, made), nil
}

// checkAssembly is CheckAssembly under the request lock: the existing
// assembly, or what a creation would decide, with nothing created.
func checkAssembly(open []Request, resolved, common, uncommitted string) (EnsureResult, error) {
	if cur, rr, ok := liveAssembly(open, common); ok {
		return readyEnsure(cur, rr, nil), nil
	}
	files, err := checkoutChanges(resolved)
	if err != nil {
		return ensureError(fmt.Sprintf("cannot read the checkout's status: %v", err)), nil
	}
	if len(files) > 0 && uncommitted == "" {
		return uncommittedRefusal(resolved, files), nil
	}
	return EnsureResult{
		Status:      "ready",
		Uncommitted: uncommittedMode(uncommitted, files),
		Files:       nonNil(files),
	}, nil
}

// assemblyName is the directory name of common's assembly under req: the one
// its entry already has, else the checkout's own name made unique in req.
func assemblyName(resolved string, req Request, common string) (string, error) {
	if rr, ok := req.Repo(common); ok && rr.Name != "" {
		return rr.Name, nil
	}
	list, err := gitx.WorktreeList(resolved)
	if err != nil || len(list) == 0 {
		return "", fmt.Errorf("could not list the worktrees of %s", resolved)
	}
	base := filepath.Base(list[0].Path)
	if strings.HasPrefix(base, ".") {
		base = "_" + base
	}
	name := base
	for n := 2; nameTaken(req, common, name); n++ {
		name = fmt.Sprintf("%s-%d", base, n)
	}
	return name, nil
}

// nameTaken is whether name is used by another repository's assembly in req
// or is occupied on disk under the workspace.
func nameTaken(req Request, common, name string) bool {
	for _, rr := range req.Repos {
		if rr.Common != common && rr.Name == name {
			return true
		}
	}
	return occupiedDir(filepath.Join(req.Dir, name))
}

// checkoutChanges are the checkout's uncommitted paths that an assembly would
// not start with, sorted. It only reads: the user's index is never touched, so
// no path is marked intent-to-add. Carry files are left out.
func checkoutChanges(resolved string) ([]string, error) {
	args := append([]string{"--no-optional-locks", "status", "--porcelain=v1", "-z", "--no-renames", "--untracked-files=all"}, CurrentFilter().Pathspec()...)
	out, err := gitx.Run(resolved, args...)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	files := []string{}
	for _, p := range landStatusPaths(out) {
		if seen[p] || isDefaultCarryFile(p) {
			continue
		}
		seen[p] = true
		files = append(files, p)
	}
	sort.Strings(files)
	return files, nil
}

func isDefaultCarryFile(p string) bool {
	for _, c := range DefaultCarryFiles {
		if p == c {
			return true
		}
	}
	return false
}

// uncommittedMode is how an assembly records its checkout's uncommitted work:
// clean when there is none, else the caller's choice.
func uncommittedMode(uncommitted string, files []string) string {
	if len(files) == 0 {
		return "clean"
	}
	return uncommitted
}

// uncommittedRefusal is the status for a checkout whose uncommitted work the
// caller has not decided about. It names the first ten paths.
func uncommittedRefusal(checkout string, files []string) EnsureResult {
	shown := strings.Join(files[:min(len(files), 10)], ", ")
	if len(files) > 10 {
		shown += ", ..."
	}
	return EnsureResult{
		Status:   "uncommitted",
		Checkout: checkout,
		Files:    files,
		Reason: fmt.Sprintf("%s has uncommitted changes (%s): the request's assembly starts from this checkout, so decide whether the work needs them: --uncommitted include builds on them (land then takes only the assembly's own changes), --uncommitted ignore starts from HEAD",
			checkout, shown),
	}
}

func readyEnsure(req Request, rr RequestRepo, created []string) EnsureResult {
	return EnsureResult{
		Status:      "ready",
		Request:     req.ID,
		Dir:         req.Dir,
		Assembly:    rr.Assembly,
		Task:        rr.Task,
		Uncommitted: rr.Uncommitted,
		Files:       nonNil(rr.Files),
		Created:     nonNil(created),
	}
}

func ensureError(reason string) EnsureResult {
	return EnsureResult{Status: "error", Reason: reason}
}
