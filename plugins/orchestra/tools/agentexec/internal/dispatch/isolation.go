package dispatch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// isolation is the decision made before the hand-off: obj is the reported
// `isolation` object; path and wd are the worktree root and the effective
// workdir inside it when isolated.
type isolation struct {
	obj      *jsonx.Object
	isolated bool
	path, wd string
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
func shouldIsolate(path, mode string) (bool, *jsonx.Object) {
	o := jsonx.NewObject()
	if mode == IsolateNever {
		return false, o.Set("isolate", false).Set("mode", mode).Set("reason", "isolation disabled (--isolate never)")
	}
	root, ok := gitx.RepoRoot(path)
	if !ok {
		return false, o.Set("isolate", false).Set("mode", mode).Set("reason", "not a git repository, cannot isolate")
	}
	if mode == IsolateAlways {
		return true, o.Set("isolate", true).Set("mode", mode).Set("reason", "isolation forced (--isolate always)").Set("repo", root)
	}
	if gitx.IsDirty(root) {
		return true, o.Set("isolate", true).Set("mode", mode).
			Set("reason", "working tree is dirty; the user's uncommitted work must not be reachable by a worker").
			Set("repo", root)
	}
	return false, o.Set("isolate", false).Set("mode", mode).
		Set("reason", "working tree is clean; nothing for a worker to destroy").Set("repo", root)
}

// isolateFor resolves isolation for a dispatch into workdir. Failing to
// isolate degrades to the shared tree with the reason; it never aborts.
func isolateFor(cfg *config.Resolved, workdir, mode, task string) isolation {
	want, verdict := shouldIsolate(workdir, mode)
	if !want {
		return isolation{obj: verdict}
	}
	why, _ := verdict.Get("reason")
	shared := func(reason string) isolation {
		return isolation{obj: jsonx.NewObject().Set("isolate", false).Set("mode", mode).Set("reason", reason)}
	}
	if task == "" {
		return shared(fmt.Sprintf("isolation wanted (%s) but no --task id was given; "+
			"an id is what lets retry rounds share one worktree", why))
	}
	created, err := isolate.Create(workdir, task, isolate.CreateOptions{
		Carry: true, CarryExtra: cfg.StringList("isolation", "carry_extra"),
	})
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
	reason := fmt.Sprint(why)
	if fallback != "" {
		reason += "; " + fallback
	}
	carried := created.Carried
	if carried == nil {
		carried = []string{}
	}
	obj := jsonx.NewObject().
		Set("isolate", true).Set("mode", mode).Set("reason", reason).
		Set("path", wt).Set("workdir", effective).
		Set("branch", created.Branch).Set("backend", created.Backend).
		Set("carried", carried).Set("created", created.Status == "created")
	return isolation{obj: obj, isolated: true, path: wt, wd: effective}
}
