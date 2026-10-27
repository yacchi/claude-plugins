package isolate

import (
	"sort"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// Lineage markers. A tree's children are derived from these every time
// (never stored as a count, never read from Orca), so a child count is
// always what git and the markers say now.
const (
	// parentFile is in a child's git dir: the real path of the parent tree.
	parentFile = "orchestra-parent"
	// mergedFile is in an integration tree's git dir: the HEAD it had when
	// `isolate merge` squashed it into its assembly, which is what lets
	// remove treat the integration tree as complete.
	mergedFile = "orchestra-merged"
)

// maxLineageDepth bounds lineageDepth, so a cycle of parent markers
// terminates.
const maxLineageDepth = 16

// Child is one live worktree whose parent marker names a tree.
type Child struct {
	Task   string `json:"task"` // branchTask of an orchestra/* branch, else the adoption task, else ""
	Branch string `json:"-"`
	Path   string `json:"path"`
}

// WriteParent records parent as the tree wt was created under. The real
// path is stored so that a later lookup through a symlink still matches.
func WriteParent(wt, parent string) error {
	return writeMarker(wt, parentFile, paths.RealPath(parent))
}

// ReadParent is the real path of wt's parent tree, "" when it has none.
func ReadParent(wt string) string { return readMarker(wt, parentFile) }

// Children lists the live worktrees whose parent marker names parent. The
// main worktree is never a child, and a tree whose directory is gone has no
// readable marker, so it is not listed either. An error means the worktree
// list could not be read; callers must treat that as "cannot tell", never as
// "no children".
func Children(root, parent string) ([]Child, error) {
	list, err := gitx.WorktreeList(root)
	if err != nil {
		return nil, err
	}
	want := paths.RealPath(parent)
	children := []Child{}
	for i, w := range list {
		if i == 0 || paths.RealPath(w.Path) == want || ReadParent(w.Path) != want {
			continue
		}
		children = append(children, Child{Task: childTask(w), Branch: w.Branch, Path: w.Path})
	}
	sort.Slice(children, func(i, j int) bool { return children[i].Path < children[j].Path })
	return children, nil
}

// childTask names a child by its orchestra branch, else by its adoption.
func childTask(w gitx.Worktree) string {
	if strings.HasPrefix(w.Branch, BranchPrefix) {
		return branchTask(w.Branch)
	}
	if task, ok := ReadAdopted(w.Path); ok {
		return task
	}
	return ""
}

// IntegrationTreeFor is the path of the integration tree that has branch
// checked out, "" when there is none. A non-integration tree on that branch
// does not count: only an integration tree can take a merge into its
// assembly.
func IntegrationTreeFor(repo, branch string) string {
	branch = strings.TrimPrefix(branch, "refs/heads/")
	if branch == "" {
		return ""
	}
	list, err := gitx.WorktreeList(repo)
	if err != nil {
		return ""
	}
	for _, w := range list {
		if w.Branch == branch && ReadRole(w.Path) == RoleIntegration {
			return w.Path
		}
	}
	return ""
}

// RoleName is the role of wt as a word for humans and JSON: "work" for a
// tree with no role.
func RoleName(wt string) string {
	if role := ReadRole(wt); role != "" {
		return role
	}
	return "work"
}

// lineageDepth is how many ancestors wt has, following parent markers until
// one has none, cannot be read, or the bound is reached.
func lineageDepth(wt string) int {
	depth := 0
	for cur := wt; depth < maxLineageDepth; {
		parent := ReadParent(cur)
		if parent == "" {
			break
		}
		depth++
		cur = parent
	}
	return depth
}
