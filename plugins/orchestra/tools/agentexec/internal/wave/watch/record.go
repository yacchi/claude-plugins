// Package watch is how a wave is followed from outside its runner: `wave
// run --detach` (the runner as a detached child, recorded in runner.pid),
// `wave wait` (block until the runner ends, a new need appears or a timeout)
// and the progress object `wave status` and `wave wait` print. It only reads
// the files the runner writes (state.json, events.jsonl, runner.pid,
// runner.log) and the plan.
package watch

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
)

// startSlack is how far a live process's start time may sit from the
// runner.pid's started_at and still be that runner (a reused pid is not).
const startSlack = 5.0

// Record is runner.pid: the detached runner's pid, when it was started
// (epoch seconds) and its full argv.
type Record struct {
	PID       int      `json:"pid"`
	StartedAt int64    `json:"started_at"`
	Argv      []string `json:"argv"`
}

func abs(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// PIDPath is <state dir>/runner.pid.
func PIDPath(statePath string) string {
	return filepath.Join(filepath.Dir(abs(statePath)), "runner.pid")
}

// LogPath is <state dir>/runner.log, the detached runner's stdout and stderr.
func LogPath(statePath string) string {
	return filepath.Join(filepath.Dir(abs(statePath)), "runner.log")
}

// ReadRecord reads runner.pid; false when it is missing or unreadable.
func ReadRecord(statePath string) (Record, bool) {
	data, err := os.ReadFile(PIDPath(statePath))
	if err != nil {
		return Record{}, false
	}
	var r Record
	if json.Unmarshal(data, &r) != nil || r.PID <= 0 {
		return Record{}, false
	}
	return r, true
}

// Flag is the value of --name in the recorded argv (`--name=v` or
// `--name v`).
func (r Record) Flag(name string) (string, bool) {
	for i, a := range r.Argv {
		if v, ok := strings.CutPrefix(a, "--"+name+"="); ok {
			return v, true
		}
		if a == "--"+name && i+1 < len(r.Argv) {
			return r.Argv[i+1], true
		}
	}
	return "", false
}

// Alive reports whether the recorded runner still runs this state: the
// pid is a live, non-zombie process that started when the record says
// (not a reused pid) and whose argv names this state file.
func (r Record) Alive(statePath string) bool {
	st, ok := r.Flag("state")
	if !ok || abs(st) != abs(statePath) || !fsutil.PIDAlive(r.PID) {
		return false
	}
	age, gone, known := processAge(r.PID)
	if !known {
		return true // no ps to tell a reused pid apart: trust the pid
	}
	if gone {
		return false
	}
	started := float64(time.Now().Unix()) - age
	return math.Abs(started-float64(r.StartedAt)) <= startSlack
}

// processAge is a process's age in seconds from ps, gone for a zombie or a
// pid ps does not list; known is false when ps could not tell.
var processAge = func(pid int) (age float64, gone, known bool) {
	out, err := exec.Command("ps", "-o", "stat=,etime=", "-p", strconv.Itoa(pid)).Output()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return 0, false, false
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return 0, true, true
	}
	secs, ok := parseEtime(fields[1])
	if !ok {
		return 0, false, false
	}
	return secs, strings.HasPrefix(fields[0], "Z"), true
}

// parseEtime reads ps's etime, [[dd-]hh:]mm:ss.
func parseEtime(s string) (float64, bool) {
	days := 0
	if d, rest, ok := strings.Cut(s, "-"); ok {
		n, err := strconv.Atoi(d)
		if err != nil {
			return 0, false
		}
		days, s = n, rest
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	total := 0
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0, false
		}
		total = total*60 + n
	}
	return float64(days*86400 + total), true
}

// Detach starts exe args (a `wave run` without --detach whose --state is
// statePath) as a detached child in its own session, stdin /dev/null,
// stdout and stderr in runner.log (truncated), and records it in
// runner.pid. While a recorded runner of the same state lives it starts
// nothing and answers status "running" instead of "detached".
func Detach(statePath, exe string, args []string) (map[string]any, error) {
	statePath = abs(statePath)
	logPath := LogPath(statePath)
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		return nil, err
	}
	unlock, err := fsutil.Lock(PIDPath(statePath)+".lock", fsutil.Exclusive, 30*time.Second)
	if err != nil {
		return nil, err
	}
	defer unlock()
	answer := func(status string, pid int) map[string]any {
		return map[string]any{"status": status, "pid": pid, "state": statePath, "log": logPath}
	}
	if rec, ok := ReadRecord(statePath); ok && rec.Alive(statePath) {
		return answer("running", rec.PID), nil
	}
	out, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = out, out, nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("could not start the runner: %w", err)
	}
	rec := Record{PID: cmd.Process.Pid, StartedAt: time.Now().Unix(), Argv: append([]string{exe}, args...)}
	_ = cmd.Process.Release()
	data, err := fsutil.MarshalJSON(rec)
	if err == nil {
		err = fsutil.AtomicWriteFile(PIDPath(statePath), data, 0o644)
	}
	if err != nil {
		return nil, fmt.Errorf("runner started (pid %d) but runner.pid was not written: %w", rec.PID, err)
	}
	return answer("detached", rec.PID), nil
}
