package dispatch

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/ledger"
)

const defaultRetentionDays = 30

var retention struct {
	sync.Mutex
	ran bool
}

// ResetRetention re-arms the once-per-process sweep (tests).
func ResetRetention() {
	retention.Lock()
	retention.ran = false
	retention.Unlock()
}

// SweepRetention deletes, at most once per process, everything older than
// ledger.retention_days (default 30): run-ledger files, token specs,
// session-store records, and the detach files of children that are gone.
// Best effort: nothing here can fail a dispatch.
func SweepRetention(cfg *config.Resolved) {
	retention.Lock()
	if retention.ran {
		retention.Unlock()
		return
	}
	retention.ran = true
	retention.Unlock()

	days, ok := cfg.Int("ledger", "retention_days")
	if !ok || days <= 0 {
		days = defaultRetentionDays
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	ledger.SweepOlder(ledger.Dir(cfg), cutoff)
	for _, dir := range []string{TokenDir(cfg), SessionDir(cfg)} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".json") {
				removeIfOlder(filepath.Join(dir, e.Name()), cutoff)
			}
		}
	}
	sweepDetach(cfg, cutoff)
}

func removeIfOlder(path string, cutoff time.Time) {
	if fi, err := os.Stat(path); err == nil && fi.Mode().IsRegular() && fi.ModTime().Before(cutoff) {
		_ = os.Remove(path)
	}
}

// sweepDetach removes old detach/<token>.* files whose child is not alive.
func sweepDetach(cfg *config.Resolved, cutoff time.Time) {
	dir := DetachDir(cfg)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	alive := map[string]bool{}
	for _, e := range entries {
		token, _, _ := strings.Cut(e.Name(), ".")
		live, seen := alive[token]
		if !seen {
			live = fsutil.PIDAlive(statePID(filepath.Join(dir, token+".json")))
			alive[token] = live
		}
		if !live {
			removeIfOlder(filepath.Join(dir, e.Name()), cutoff)
		}
	}
}

// statePID is the pid a detach state file records (0 when none).
func statePID(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	v, err := jsonx.Decode(raw)
	if err != nil {
		return 0
	}
	o, _ := v.(*jsonx.Object)
	if o == nil {
		return 0
	}
	p, _ := o.Get("pid")
	return pidOf(p)
}

// pidOf is a JSON pid: a positive integer (never a bool or a float).
func pidOf(v any) int {
	if n, ok := v.(int64); ok && n > 0 && n < 1<<31 {
		return int(n)
	}
	return 0
}
