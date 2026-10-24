// Package orcanest nests orchestra's worktrees in Orca's sidebar. Orca lists
// every git worktree of a repository, flat unless told a parent, so each tree
// orchestra creates is set under the tree it was cut from: the worktree that
// has its base branch checked out, else the checkout the caller launched
// from. A package tree cut from the integration branch thus sits under the
// integration tree, which sits under the launching checkout.
//
// Nesting is cosmetic: without `orca` on PATH it is skipped silently, and a
// failure is at most one stderr line per process, never an error.
package orcanest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
)

// DefaultTimeout bounds one `orca worktree set`.
const DefaultTimeout = 10 * time.Second

// Nester runs `orca worktree set`. Binary is the orca binary (a bare name is
// looked up on PATH), Stderr gets the warning, Timeout bounds a call
// (0 = DefaultTimeout).
type Nester struct {
	Binary  string
	Stderr  io.Writer
	Timeout time.Duration
}

// warned is set by the process's first failed nesting, whichever Nester
// (isolate's or the Orca host's) hit it: a broken Orca would otherwise warn
// once per tree.
var warned atomic.Bool

// ResetWarning lets a test see the next warning again, and again after it.
func ResetWarning(t testing.TB) {
	warned.Store(false)
	t.Cleanup(func() { warned.Store(false) })
}

// Default is the process's Nester: `orca` from PATH, warnings to stderr.
var Default = &Nester{Binary: "orca", Stderr: os.Stderr}

// Nest nests child under its parent (see Parent) through Default.
func Nest(repo, base, child string) { Default.Nest(repo, base, child) }

// Nest nests child, a worktree of repo cut from base, under Parent(repo,
// base, child). Inside a test binary a bare binary name is never looked up:
// the developer's real Orca must not see the suite's throwaway trees.
func (n *Nester) Nest(repo, base, child string) {
	if testing.Testing() && !filepath.IsAbs(n.Binary) {
		return
	}
	bin, err := exec.LookPath(n.Binary)
	if err != nil {
		return
	}
	parent := Parent(repo, base, child)
	if err := n.set(bin, child, parent); err != nil && warned.CompareAndSwap(false, true) {
		fmt.Fprintf(n.Stderr, "agent-exec: orca: could not nest %s under %s: %v\n", child, parent, err)
	}
}

func (n *Nester) set(bin, child, parent string) error {
	timeout := n.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "worktree", "set", "--worktree", "path:"+child,
		"--parent-worktree", "path:"+parent, "--json")
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("timed out after %s", timeout)
	}
	var payload struct {
		OK    bool `json:"ok"`
		Error any  `json:"error"`
	}
	_ = json.Unmarshal(out, &payload)
	switch {
	case payload.OK && err == nil:
		return nil
	case payload.Error != nil:
		return fmt.Errorf("%v", payload.Error)
	case err != nil:
		return err
	}
	return fmt.Errorf("unexpected reply %q", strings.TrimSpace(string(out)))
}

// Parent is the tree child nests under: the worktree of repo that has base
// (a branch, with or without refs/heads/) checked out, else repo itself.
// Never child: a base checked out only in child falls back to repo.
func Parent(repo, base, child string) string {
	base = strings.TrimPrefix(base, "refs/heads/")
	if base == "" {
		return repo
	}
	list, err := gitx.WorktreeList(repo)
	if err != nil {
		return repo
	}
	self := canonical(child)
	for _, wt := range list {
		if wt.Branch == base && canonical(wt.Path) != self {
			return wt.Path
		}
	}
	return repo
}

func canonical(path string) string {
	if p, err := filepath.EvalSymlinks(path); err == nil {
		return p
	}
	return filepath.Clean(path)
}
