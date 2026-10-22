// Package trace is the always-on, machine-local timing log: one JSON line
// per finished operation (a git call, a removal, a copy, a herdr call, a
// lock wait, a worker's Bash command), appended to a per-day file that
// every agent-exec process and the orchestra hooks share. Unlike telemetry
// it records paths and commands, so it never leaves the machine.
//
// Each line holds the operation's start and end, so a reader can count how
// many operations overlapped any one of them: that is how `agent-exec trace
// report` finds the commands that slow down as parallelism goes up.
package trace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// Kinds of operation. A record's Kind is one of these.
const (
	KindGit    = "git"         // a git subprocess
	KindFS     = "fs"          // a removal, copy or clone of a tree
	KindExec   = "exec"        // any other subprocess agent-exec waits on
	KindHerdr  = "herdr"       // a herdr CLI call
	KindLock   = "lock"        // time spent waiting to acquire a lock
	KindWorker = "worker-bash" // a Bash command a Claude worker ran
)

// RetentionDays is how long day files are kept; older ones are deleted the
// first time a process writes.
const RetentionDays = 14

const maxDetail = 300

// Record is one line of the log. At and End are Unix seconds.
type Record struct {
	At     float64 `json:"at"`
	End    float64 `json:"end"`
	MS     int64   `json:"ms"`
	Kind   string  `json:"kind"`
	Op     string  `json:"op"`
	PID    int     `json:"pid"`
	Pkg    string  `json:"pkg,omitempty"`
	Cwd    string  `json:"cwd,omitempty"`
	Exit   int     `json:"exit"`
	Load1  float64 `json:"load1"`
	Detail string  `json:"detail,omitempty"`
}

// Attrs are a span's optional fields.
type Attrs struct {
	Pkg    string
	Cwd    string
	Detail string
}

// Dir is where the day files live: $ORCHESTRA_TRACE_DIR, else
// <state dir>/trace (machine-local, never a synced directory).
func Dir() string {
	if d := os.Getenv("ORCHESTRA_TRACE_DIR"); d != "" {
		return d
	}
	return filepath.Join(paths.StateDir(), "trace")
}

// Enabled is false only when ORCHESTRA_TRACE is "off", "0" or "false".
func Enabled() bool {
	switch strings.ToLower(os.Getenv("ORCHESTRA_TRACE")) {
	case "off", "0", "false":
		return false
	}
	return true
}

// FileFor is the day file holding records that start at t (local date).
func FileFor(t time.Time) string {
	return filepath.Join(Dir(), t.Format("2006-01-02")+".jsonl")
}

// Begin starts timing an operation; the returned func ends it with the
// operation's exit code (0 success, -1 could not start) and writes the
// record. Ending twice writes once.
func Begin(kind, op string, a Attrs) func(exit int) {
	if !Enabled() {
		return func(int) {}
	}
	start := time.Now()
	var once sync.Once
	return func(exit int) {
		once.Do(func() { Write(build(kind, op, a, start, time.Now(), exit)) })
	}
}

// Lock times the wait for a mutex and returns once it is held.
func Lock(mu sync.Locker, name string, a Attrs) {
	end := Begin(KindLock, name, a)
	mu.Lock()
	end(0)
}

func build(kind, op string, a Attrs, start, end time.Time, exit int) Record {
	detail := a.Detail
	if len(detail) > maxDetail {
		detail = detail[:maxDetail]
	}
	return Record{
		At:     epoch(start),
		End:    epoch(end),
		MS:     end.Sub(start).Milliseconds(),
		Kind:   kind,
		Op:     op,
		PID:    os.Getpid(),
		Pkg:    a.Pkg,
		Cwd:    a.Cwd,
		Exit:   exit,
		Load1:  load1(),
		Detail: detail,
	}
}

func epoch(t time.Time) float64 {
	return float64(t.UnixNano()) / 1e9
}

var pruneOnce sync.Once

// Write appends r to its day file. Failures are dropped: timing must never
// break the operation it times.
func Write(r Record) {
	if !Enabled() {
		return
	}
	line, err := json.Marshal(r)
	if err != nil {
		return
	}
	at := time.Unix(0, int64(r.At*1e9))
	path := FileFor(at)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	pruneOnce.Do(func() { prune(time.Now()) })
	// One write(2) per line on an O_APPEND descriptor: lines from
	// concurrent processes do not interleave.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
	_ = f.Close()
}

// prune deletes day files older than RetentionDays.
func prune(now time.Time) {
	entries, err := os.ReadDir(Dir())
	if err != nil {
		return
	}
	cutoff := now.AddDate(0, 0, -RetentionDays).Format("2006-01-02")
	for _, e := range entries {
		name := e.Name()
		day, ok := strings.CutSuffix(name, ".jsonl")
		if !ok || len(day) != len("2006-01-02") {
			continue
		}
		if day < cutoff {
			_ = os.Remove(filepath.Join(Dir(), name))
		}
	}
}
