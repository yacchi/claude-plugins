package isolate

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
)

// A tree somebody is still working in is never removed, not even with
// --force (which discards content, not a live writer's tree). Two signals:
// the writer lease the worktree-lease hook keeps in the worktree's git dir,
// and the flock an `isolate integrate` into the tree holds while it runs.

// leaseFile is the hook's on-disk contract (hooks/guards/worktree-lease.ts):
// {agent_id, agent_type, last_seen, since}, last_seen in epoch seconds.
const leaseFile = "orchestra-lease.json"

const defaultLeaseWindow = 120 * time.Second

// leaseWindow is how recently a lease must have been refreshed to count as
// held: ORCHESTRA_LEASE_WINDOW seconds when it parses as a finite number
// (as the hook reads it), else 120s.
func leaseWindow() time.Duration {
	if v := strings.TrimSpace(os.Getenv("ORCHESTRA_LEASE_WINDOW")); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil && !math.IsInf(n, 0) && !math.IsNaN(n) {
			return time.Duration(n * float64(time.Second))
		}
	}
	return defaultLeaseWindow
}

// freshLease describes the lease on the tree at path when it was refreshed
// within the window; "" when there is none, it is stale, or unreadable.
func freshLease(path string, now time.Time) string {
	p, err := markerPath(path, leaseFile)
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	var lease map[string]any
	if json.Unmarshal(b, &lease) != nil {
		return ""
	}
	// Like the hook: a missing or non-numeric last_seen is ancient.
	seen, _ := lease["last_seen"].(float64)
	age := float64(now.UnixNano())/1e9 - seen
	if age >= leaseWindow().Seconds() {
		return ""
	}
	holder, _ := lease["agent_type"].(string)
	if holder == "" {
		holder = "another agent"
	}
	return fmt.Sprintf("in use: lease held by %s %ds ago", holder, int(math.Max(0, age)))
}

var reLockUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// IntegrateLockPath is the lock file of one integration target, shared by
// every worktree of the repository at repo; "" outside a repository.
func IntegrateLockPath(repo, into string) string {
	out, err := gitx.Run(repo, "rev-parse", "--git-common-dir")
	common := strings.TrimSpace(out)
	if err != nil || common == "" {
		return ""
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(repo, common)
	}
	if abs, err := filepath.Abs(common); err == nil {
		common = abs
	}
	safe := reLockUnsafe.ReplaceAllString(into, "_")
	if safe == "" {
		safe = "_"
	}
	return filepath.Join(common, "orchestra", "integrate-"+safe+".lock")
}

// integrateRunning reports whether an `isolate integrate` into task holds
// its lock right now. A probe only: the file is never created or written,
// and anything unexpected reads as "not running".
func integrateRunning(repo, task string) bool {
	lock := IntegrateLockPath(repo, task)
	if lock == "" {
		return false
	}
	f, err := os.Open(lock)
	if err != nil {
		return false
	}
	defer f.Close()
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
}

// inUse says why task's tree at path must not be removed now, "" when
// nobody is working in it.
func inUse(repo, task, path string) string {
	if reason := freshLease(path, time.Now()); reason != "" {
		return reason
	}
	if integrateRunning(repo, task) {
		return "in use: isolate integrate is running"
	}
	return ""
}
