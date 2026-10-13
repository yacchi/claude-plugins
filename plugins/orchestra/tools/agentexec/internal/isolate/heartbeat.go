package isolate

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// Session heartbeats. Sweep reclaims worktrees repo-wide, so it meets trees
// of other Claude Code sessions that may still be running. A session id is
// not a process, so liveness is recorded rather than probed: every command
// implying "a session is working here" touches a file named after the
// session's 8-character id, and sweep treats a session touched inside the
// live window as in use. Advisory only: a missing or stale heartbeat
// degrades to "not live", which leaves the removal gate as the protection.

// DefaultLiveWindow is how recently another session must have been seen for
// its worktrees to count as in use.
const DefaultLiveWindow = 120 * time.Minute

// heartbeatRetention: heartbeats older than this are garbage.
const heartbeatRetention = 30 * 24 * time.Hour

var aliveDir struct {
	sync.Mutex
	dir string
}

// SetAliveDir points the heartbeat store at dir (see AliveDirFor). Unset,
// it is paths.AliveDir().
func SetAliveDir(dir string) {
	aliveDir.Lock()
	defer aliveDir.Unlock()
	aliveDir.dir = dir
}

func heartbeatDir() string {
	aliveDir.Lock()
	defer aliveDir.Unlock()
	if aliveDir.dir == "" {
		return paths.AliveDir()
	}
	return aliveDir.dir
}

// AliveDirFor is the heartbeat directory for cfg: $ORCHESTRA_ALIVE_DIR, else
// "alive" beside the run ledger directory (so relocating the ledger moves
// the heartbeats with it), else ~/.claude/orchestra/alive.
func AliveDirFor(cfg *config.Resolved) string {
	if os.Getenv("ORCHESTRA_ALIVE_DIR") != "" || cfg == nil {
		return paths.AliveDir()
	}
	ledger, ok := cfg.String("ledger", "dir")
	if ok && ledger != "" {
		ledger = fsutil.ExpandUser(ledger)
	} else {
		telemetry, _ := cfg.String("telemetry", "dir")
		if telemetry == "" {
			telemetry = "~/.claude/orchestra/telemetry"
		}
		ledger = filepath.Join(filepath.Dir(fsutil.ExpandUser(telemetry)), "runs")
	}
	abs, err := filepath.Abs(ledger)
	if err != nil {
		return paths.AliveDir()
	}
	return filepath.Join(filepath.Dir(abs), "alive")
}

// HeartbeatTouch records that session ("" = the calling session) is alive.
// Sessions without a usable id are ignored; failures are silent.
func HeartbeatTouch(session string) {
	c := sessionComponent(orCurrent(session))
	if c == "" {
		return
	}
	dir := heartbeatDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	p := filepath.Join(dir, c)
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return
	}
	_ = f.Close()
	now := time.Now()
	_ = os.Chtimes(p, now, now)
}

// sessionLastSeen is when session last touched its heartbeat.
func sessionLastSeen(session string) (time.Time, bool) {
	if session == "" {
		return time.Time{}, false
	}
	st, err := os.Stat(filepath.Join(heartbeatDir(), session))
	if err != nil {
		return time.Time{}, false
	}
	return st.ModTime(), true
}

func sessionIsLive(session string, window time.Duration, now time.Time) bool {
	seen, ok := sessionLastSeen(session)
	if !ok || window <= 0 {
		return false
	}
	return now.Sub(seen) < window
}

// pruneHeartbeats drops heartbeats far past any plausible live window.
func pruneHeartbeats(now time.Time) {
	dir := heartbeatDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := now.Add(-heartbeatRetention)
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if st, err := os.Stat(p); err == nil && st.ModTime().Before(cutoff) {
			_ = os.Remove(p)
		}
	}
}
