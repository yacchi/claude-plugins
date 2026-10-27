package isolate

import (
	"fmt"
	"path/filepath"
)

// EnsureIntegration is the integration tree task of the calling session's
// request. It is cut from the assembly's HEAD, not the user's checkout, so a
// feature starts from what the request has landed so far. Callers hold the
// request lock through runAssembly, so the existence check and the creation
// cannot race with another caller for the same tree.
func EnsureIntegration(root, task, uncommitted string) (EnsureResult, error) {
	task, err := SanitizeTaskID(task)
	if err != nil {
		return EnsureResult{}, err
	}
	if err := checkUncommitted(uncommitted); err != nil {
		return EnsureResult{}, err
	}
	return runAssembly(root, func(rootDir string, open []Request, resolved, common string) (EnsureResult, error) {
		if existing := worktreeForBranch(resolved, Branch(task, currentSession())); existing != "" {
			return adoptIntegration(existing, task, open), nil
		}
		asm, err := ensureAssembly(rootDir, open, resolved, common, uncommitted)
		if err != nil || asm.Status != "ready" {
			return asm, err
		}
		// The assembly's directory name is the entry's Name: the assembly is
		// always planted at <workspace>/<Name>.
		res, err := Create(asm.Assembly, task, CreateOptions{
			Backend: BackendGit,
			Onto:    RevParseCommit(asm.Assembly, "HEAD"),
			Role:    RoleIntegration,
			Parent:  asm.Assembly,
			Dir:     filepath.Join(asm.Dir, ".integration", filepath.Base(asm.Assembly), task),
		})
		switch {
		case err != nil:
			return ensureError("could not create the integration tree: " + err.Error()), nil
		case res.Status == "error":
			return ensureError("could not create the integration tree: " + res.Reason), nil
		case res.Status == "exists":
			adopted := adoptIntegration(res.Path, task, open)
			adopted.Created = asm.Created
			return adopted, nil
		}
		asm.Into = task
		asm.Integration = res.Path
		asm.Head = RevParseCommit(res.Path, "HEAD")
		asm.Created = append(asm.Created, "integration")
		return asm, nil
	})
}

// adoptIntegration makes the existing tree at path the integration tree task.
// A tree with no role takes the integration role; an assembly is refused, and
// its markers stay as they are.
func adoptIntegration(path, task string, open []Request) EnsureResult {
	switch ReadRole(path) {
	case RoleAssembly:
		return ensureError(fmt.Sprintf("%s is an assembly worktree, not an integration target", task))
	case "":
		if err := WriteRole(path, RoleIntegration); err != nil {
			return ensureError("could not record the integration role: " + err.Error())
		}
	}
	res := EnsureResult{
		Status:      "ready",
		Into:        task,
		Integration: path,
		Head:        RevParseCommit(path, "HEAD"),
		Assembly:    ReadParent(path),
	}
	if r, rr, ok := requestOfAssembly(open, res.Assembly); ok {
		res.Request, res.Dir, res.Task = r.ID, r.Dir, rr.Task
		res.Uncommitted, res.Files = rr.Uncommitted, nonNil(rr.Files)
	}
	return res
}

// requestOfAssembly is the open request whose entry names assembly. It reads
// the slice it is given: the caller already holds the request lock.
func requestOfAssembly(open []Request, assembly string) (Request, RequestRepo, bool) {
	if assembly == "" {
		return Request{}, RequestRepo{}, false
	}
	for _, r := range open {
		for _, rr := range r.Repos {
			if rr.Assembly == assembly {
				return r, rr, true
			}
		}
	}
	return Request{}, RequestRepo{}, false
}
