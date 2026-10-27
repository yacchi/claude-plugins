package isolate

import (
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// CurrentRequest is the calling session's newest open request.
func CurrentRequest() (Request, bool, error) {
	var (
		r  Request
		ok bool
	)
	err := withRequests(func(_ string, open []Request) error {
		r, ok = pickCurrent(open)
		return nil
	})
	return r, ok, err
}

// FindRequest is the open request with the given id.
func FindRequest(id string) (Request, bool, error) {
	open, err := lookupOpen(func(r Request) bool { return r.ID == id })
	if err != nil || len(open) == 0 {
		return Request{}, false, err
	}
	return open[0], true, nil
}

// ListRequests is every open request, oldest first.
func ListRequests() ([]Request, error) {
	return lookupOpen(func(Request) bool { return true })
}

// RequestsFor is the open requests with a live assembly of the repository
// that root belongs to, oldest first.
func RequestsFor(root string) ([]Request, error) {
	common, err := RepoCommon(root)
	if err != nil {
		return nil, err
	}
	reqs, err := lookupOpen(func(r Request) bool { return liveFor(r, common) })
	if err != nil {
		return nil, err
	}
	if reqs == nil {
		reqs = []Request{}
	}
	return reqs, nil
}

// RequestFor is the open request that owns assembly, alive or not. The
// assembly path is compared as a real path: a symlinked spelling of it names
// the same tree.
func RequestFor(assembly string) (Request, bool, error) {
	target := paths.RealPath(assembly)
	open, err := lookupOpen(func(r Request) bool {
		for _, rr := range r.Repos {
			if rr.Assembly == target {
				return true
			}
		}
		return false
	})
	if err != nil || len(open) == 0 {
		return Request{}, false, err
	}
	return open[0], true, nil
}

// EndRequestIfDone reports whether request id is no longer open. Opening the
// lookup ends a request whose assemblies are all gone, so this is how a caller
// learns that its request has finished.
func EndRequestIfDone(id string) (bool, error) {
	open, err := lookupOpen(func(r Request) bool { return r.ID == id })
	if err != nil {
		return false, err
	}
	return len(open) == 0, nil
}

// AssemblyFor is the task of the assembly that isolate land takes when none is
// named. The calling session's request wins; otherwise the only live request
// does. Several live requests with no session match leave the choice to the
// caller, who gets their ids as candidates.
func AssemblyFor(root string) (string, []string, error) {
	common, err := RepoCommon(root)
	if err != nil {
		return "", nil, err
	}
	reqs, err := lookupOpen(func(r Request) bool { return liveFor(r, common) })
	if err != nil {
		return "", nil, err
	}
	if mine, ok := pickCurrent(reqs); ok {
		return taskIn(mine, common), nil, nil
	}
	if len(reqs) == 1 {
		return taskIn(reqs[0], common), nil, nil
	}
	ids := []string{}
	for _, r := range reqs {
		ids = append(ids, r.ID)
	}
	return "", ids, nil
}

// lookupOpen is the open requests keep accepts, oldest first. It runs under
// withRequests, so requests that have ended are not returned.
func lookupOpen(keep func(Request) bool) ([]Request, error) {
	var out []Request
	err := withRequests(func(_ string, open []Request) error {
		for _, r := range open {
			if keep(r) {
				out = append(out, r)
			}
		}
		return nil
	})
	return out, err
}

// liveFor is whether r has an assembly of the repository common that is still
// on disk.
func liveFor(r Request, common string) bool {
	rr, ok := r.Repo(common)
	return ok && AssemblyAlive(rr)
}

// taskIn is the task of r's assembly for the repository common. Callers check
// liveFor first, so the entry exists.
func taskIn(r Request, common string) string {
	rr, _ := r.Repo(common)
	return rr.Task
}
