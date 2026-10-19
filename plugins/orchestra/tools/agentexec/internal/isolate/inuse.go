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

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
)

// A tree somebody is still working in is never removed, not even with
// --force (which discards content, not a live tree). Three signals: the
// writer lease and the activity stamp the worktree-lease hook keeps in the
// worktree's git dir, and the flock an `isolate integrate` into the tree
// holds while it runs.

// The hook's on-disk contract (hooks/guards/worktree-lease.ts), last_seen in
// epoch seconds. leaseFile: {agent_id, agent_type, last_seen, since}, taken
// by a writer. activityFile: {agent_id, agent_type, last_seen, command},
// stamped by any Bash call run inside the tree (tests, builds, reads).
const (
	leaseFile    = "orchestra-lease.json"
	activityFile = "orchestra-activity.json"
)

const (
	defaultLeaseWindow = 120 * time.Second
	// A foreground Bash call can run for up to 10 minutes, and its stamp
	// is taken when it starts.
	defaultActivityWindow = 600 * time.Second
)

// envSeconds is the env var as seconds when it parses as a finite number
// (as the hook reads it).
func envSeconds(name string) (time.Duration, bool) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsInf(n, 0) || math.IsNaN(n) {
		return 0, false
	}
	return time.Duration(n * float64(time.Second)), true
}

// leaseWindow is how recently a lease must have been refreshed to count as
// held, in the hook's order: ORCHESTRA_LEASE_WINDOW, then a finite non-zero
// enforcement.worktree_lease_window, then 120s. Only asked when a lease
// file exists, so the common path never resolves the config.
func leaseWindow() time.Duration {
	if d, ok := envSeconds("ORCHESTRA_LEASE_WINDOW"); ok {
		return d
	}
	if cfg, err := config.Resolve(); err == nil {
		v, _ := cfg.Lookup("enforcement", "worktree_lease_window")
		var n float64
		switch x := v.(type) {
		case int64:
			n = float64(x)
		case int:
			n = float64(x)
		case float64:
			n = x
		case string:
			n, _ = strconv.ParseFloat(strings.TrimSpace(x), 64)
		}
		if n != 0 && !math.IsInf(n, 0) && !math.IsNaN(n) {
			return time.Duration(n * float64(time.Second))
		}
	}
	return defaultLeaseWindow
}

func activityWindow() time.Duration {
	if d, ok := envSeconds("ORCHESTRA_ACTIVITY_WINDOW"); ok {
		return d
	}
	return defaultActivityWindow
}

// readStamp is a lease or activity file of the tree at path and its age in
// seconds; ok=false when absent or unreadable. A missing or non-numeric
// last_seen is ancient, as in the hook.
func readStamp(path, name string, now time.Time) (stamp map[string]any, age float64, ok bool) {
	p, err := markerPath(path, name)
	if err != nil {
		return nil, 0, false
	}
	b, err := os.ReadFile(p)
	if err != nil || json.Unmarshal(b, &stamp) != nil || stamp == nil {
		return nil, 0, false
	}
	seen, _ := stamp["last_seen"].(float64)
	return stamp, float64(now.UnixNano())/1e9 - seen, true
}

func holderOf(stamp map[string]any) string {
	if holder, _ := stamp["agent_type"].(string); holder != "" {
		return holder
	}
	return "another agent"
}

// freshLease describes the lease on the tree at path when it was refreshed
// within the window; "" when there is none, it is stale, or unreadable.
func freshLease(path string, now time.Time) string {
	lease, age, ok := readStamp(path, leaseFile, now)
	if !ok || age >= leaseWindow().Seconds() {
		return ""
	}
	return fmt.Sprintf("in use: lease held by %s %ds ago", holderOf(lease), int(math.Max(0, age)))
}

// freshActivity describes the last Bash call run inside the tree at path
// when it started within the activity window; "" otherwise.
func freshActivity(path string, now time.Time) string {
	act, age, ok := readStamp(path, activityFile, now)
	if !ok || age >= activityWindow().Seconds() {
		return ""
	}
	command, _ := act["command"].(string)
	return fmt.Sprintf("in use: %s ran %q %ds ago", holderOf(act), command, int(math.Max(0, age)))
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
	now := time.Now()
	if reason := freshLease(path, now); reason != "" {
		return reason
	}
	if reason := freshActivity(path, now); reason != "" {
		return reason
	}
	if integrateRunning(repo, task) {
		return "in use: isolate integrate is running"
	}
	return ""
}
