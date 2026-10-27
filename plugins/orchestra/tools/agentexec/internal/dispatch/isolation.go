package dispatch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// isolation is the decision made before the hand-off: obj is the reported
// `isolation` object; path and wd are the worktree root and the effective
// workdir inside it when isolated.
type isolation struct {
	obj      map[string]any
	isolated bool
	path, wd string
	// refused is non-empty when the dispatch must not run at all (see
	// isolateFor): the start commit tracks dependency stores.
	refused string
}

// workdir is the directory the executor runs in.
func (i isolation) workdir(requested string) string {
	if i.isolated {
		return i.wd
	}
	return requested
}

// tree is what the sandbox confines to: the worktree root when isolated.
func (i isolation) tree(workdir string) string {
	if i.isolated {
		return i.path
	}
	return workdir
}

// shouldIsolate is the mode's verdict on the tree at path.
func shouldIsolate(path, mode string) (bool, map[string]any) {
	verdict := func(isolate bool, reason, repo string) (bool, map[string]any) {
		o := map[string]any{"isolate": isolate, "mode": mode, "reason": reason}
		if repo != "" {
			o["repo"] = repo
		}
		return isolate, o
	}
	if mode == IsolateNever {
		return verdict(false, "isolation disabled (--isolate never)", "")
	}
	root, ok := gitx.RepoRoot(path)
	if !ok {
		return verdict(false, "not a git repository, cannot isolate", "")
	}
	if mode == IsolateAlways || mode == IsolateCli {
		return verdict(true, "isolation forced (--isolate "+mode+")", root)
	}
	if gitx.IsDirty(root) {
		return verdict(true, "working tree is dirty; the user's uncommitted work must not be reachable by a worker", root)
	}
	return verdict(false, "working tree is clean; nothing for a worker to destroy", root)
}

// callerIsolates is a `cli` dispatch's decision for a delegate route: no
// tree, the caller (a wave runner's interactive host) makes its own.
func callerIsolates() isolation {
	return isolation{obj: map[string]any{"isolate": false, "mode": IsolateCli,
		"reason": "delegate route under --isolate cli; the caller isolates delegate routes"}}
}

// isolateFor resolves isolation for a dispatch into workdir. Failing to
// isolate degrades to the shared tree with the reason, except a refused start
// commit (HEAD tracks dependency stores) and, for a forced isolation with a run
// id in the user's checkout, a request whose integration tree cannot be made:
// those abort the dispatch before Create, so no worktree is made and the worker
// never lands in the user's checkout.
//
// With a run id the task tree starts from the request's integration tree for
// that run and is recorded as its child, so the run's work flows checkout ->
// assembly -> integration tree -> task trees.
func isolateFor(cfg *config.Resolved, workdir, mode, task, runID string) isolation {
	want, verdict := shouldIsolate(workdir, mode)
	if !want {
		return isolation{obj: verdict}
	}
	why := verdict["reason"]
	shared := func(reason string) isolation {
		return isolation{obj: map[string]any{"isolate": false, "mode": mode, "reason": reason}}
	}
	refuse := func(reason string) isolation {
		return isolation{
			obj:     map[string]any{"isolate": false, "mode": mode, "reason": reason},
			refused: reason,
		}
	}
	if task == "" {
		return shared(fmt.Sprintf("isolation wanted (%s) but no --task id was given; "+
			"an id is what lets retry rounds share one worktree", why))
	}
	if dirs, err := isolate.TrackedExcluded(workdir, "HEAD", isolate.CurrentFilter()); err == nil && len(dirs) > 0 {
		return refuse(isolate.TrackedRefusal("HEAD", dirs))
	}
	opts := isolate.CreateOptions{
		Carry: true, CarryExtra: cfg.StringList("isolation", "carry_extra"),
	}
	// A workdir inside an integration tree (wave run's per-package dispatch)
	// makes the task tree that tree's child, so the integration tree counts it.
	if root, ok := gitx.RepoRoot(workdir); ok && isolate.ReadRole(root) == isolate.RoleIntegration {
		opts.Parent = root
	}
	cut := ""
	if (mode == IsolateAlways || mode == IsolateCli) && runID != "" {
		if _, user := isolate.UserCheckout(workdir); user {
			res, err := isolate.EnsureIntegration(workdir, runID, "")
			switch {
			case err != nil:
				return refuse(err.Error())
			case res.Status == "uncommitted":
				return refuse(fmt.Sprintf("%s; run 'agent-exec isolate request start --into %s --uncommitted include|ignore' once before preparing or dispatching", res.Reason, runID))
			case res.Status != "ready" || res.Head == "":
				return refuse(fmt.Sprintf("could not prepare integration tree %s: %s", runID, res.Reason))
			}
			// The task tree is cut from the integration tree's HEAD, but its
			// dependencies still come from the checkout (Create's root).
			opts.Onto, opts.Parent = res.Head, res.Integration
			cut = fmt.Sprintf("; cut from integration tree %s", res.Into)
			if res.Request != "" {
				cut += fmt.Sprintf(" of request %s", res.Request)
			}
		}
	}
	created, err := isolate.Create(workdir, task, opts)
	if err != nil {
		created = isolate.CreateResult{Status: "error", Reason: err.Error()}
	}
	if (created.Status != "created" && created.Status != "exists") || created.Path == "" {
		reason := created.Reason
		if reason == "" {
			reason = "unknown error"
		}
		return shared(fmt.Sprintf("could not isolate (%s); running in the shared tree", reason))
	}
	// An existing tree under another integration tree is not this wave's to
	// reuse; integrate would reject it as foreign later, so stop here.
	if created.Status == "exists" && opts.Parent != "" && created.Parent != "" &&
		paths.RealPath(created.Parent) != paths.RealPath(opts.Parent) {
		return refuse(fmt.Sprintf("task %s already has a tree at %s under %s, not under %s; "+
			"finish or discard it ('agent-exec isolate remove --task %s', add --force to discard its work), "+
			"or use another task id", task, created.Path, created.Parent, opts.Parent, task))
	}
	wt := created.Path
	effective, fallback := wt, ""
	if root, ok := gitx.RepoRoot(workdir); ok {
		// rev-parse resolves symlinks: compare like with like.
		rel, err := filepath.Rel(root, paths.RealPath(workdir))
		switch {
		case err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)):
			fallback = "--workdir resolved outside its own repository root; using the worktree root instead"
		case rel == ".":
		default:
			candidate := filepath.Join(wt, rel)
			if st, err := os.Stat(candidate); err == nil && st.IsDir() {
				effective = candidate
			} else {
				fallback = fmt.Sprintf("--workdir's subdirectory (%s) does not exist in the "+
					"fresh worktree; using the worktree root instead", rel)
			}
		}
	}
	reason := fmt.Sprint(why) + cut
	if fallback != "" {
		reason += "; " + fallback
	}
	carried := created.Carried
	if carried == nil {
		carried = []string{}
	}
	obj := map[string]any{
		"isolate": true,
		"mode":    mode,
		"reason":  reason,
		"path":    wt,
		"workdir": effective,
		"branch":  created.Branch,
		"backend": created.Backend,
		"carried": carried,
		"created": created.Status == "created",
	}
	return isolation{obj: obj, isolated: true, path: wt, wd: effective}
}
