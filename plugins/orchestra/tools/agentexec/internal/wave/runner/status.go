package runner

import (
	"fmt"
	"math"
	"os"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/statusfile"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/watch"
)

// statusPoll is how often a running wave run checks its state file for a
// change (tests shorten it).
var statusPoll = 5 * time.Second

// statusWriter keeps this run's entry of the machine-wide status file
// current. Only the poller goroutine writes between startStatus and finish.
type statusWriter struct {
	r                   *runner
	store               statusfile.Store
	key, project, title string
	runID               *string
	startedAt           int64

	// mtime and size are the state file as the last write saw it; wrote is
	// when that write happened.
	mtime time.Time
	size  int64
	wrote time.Time

	warned bool
	stop   chan struct{}
	done   chan struct{}
}

// startStatus writes the run's entry at once and starts the poller.
func (r *runner) startStatus() *statusWriter {
	key := statusfile.RunKey(r.statePath)
	title := r.opts.Title
	if title == "" {
		title = r.opts.RunID
	}
	if title == "" {
		title = key
	}
	var runID *string
	if r.opts.RunID != "" {
		id := r.opts.RunID
		runID = &id
	}
	s := &statusWriter{
		r: r, store: statusfile.Default(), key: key, project: statusfile.Project(r.root),
		title: title, runID: runID, startedAt: time.Now().Unix(),
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	s.write("")
	go s.poll()
	return s
}

// poll rewrites the entry when the state file changed since the last write
// (the runner's own changes and those of other processes such as `wave
// mark`) or when a heartbeat is due.
func (s *statusWriter) poll() {
	defer close(s.done)
	ticker := time.NewTicker(statusPoll)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
		}
		mtime, size := s.stat()
		heartbeat := time.Duration(statusfile.HeartbeatSeconds) * time.Second
		if mtime.Equal(s.mtime) && size == s.size && time.Since(s.wrote) < heartbeat {
			continue
		}
		s.write("")
	}
}

// finish stops the poller, waits for it to exit and writes the final entry.
func (s *statusWriter) finish(rep Report) {
	close(s.stop)
	<-s.done
	status := "done"
	switch {
	case rep.Status == "stopped":
		status = "stopped"
	case len(rep.Needs) > 0:
		status = "needs"
	}
	s.write(status)
}

func (s *statusWriter) stat() (time.Time, int64) {
	info, err := os.Stat(s.r.statePath)
	if err != nil {
		return time.Time{}, -1
	}
	return info.ModTime(), info.Size()
}

// write rewrites the run's file and status.json. final is the ended status
// ("" while the run is live, which reads as running or needs).
func (s *statusWriter) write(final string) {
	s.mtime, s.size = s.stat()
	now := time.Now()
	s.wrote = now

	st := s.r.store.Snapshot()
	pr := watch.Compute(s.r.statePath, &st, wallClock(), nil)
	if usd, ok := s.r.runCost(); ok {
		pr.CostUSD = &usd
	}

	status := final
	if status == "" {
		status = "running"
		if len(pr.Needs) > 0 {
			status = "needs"
		}
	}
	var endedAt *int64
	if final != "" {
		ended := now.Unix()
		endedAt = &ended
	}

	failed := 0
	for _, ps := range st.Packages {
		if ps.Status == "failed" {
			failed++
		}
	}
	packages := []statusfile.RunPackage{}
	for _, p := range pr.Running {
		var est *int
		if p.EstSeconds != nil {
			v := int(math.Round(*p.EstSeconds))
			est = &v
		}
		packages = append(packages, statusfile.RunPackage{ID: p.ID, Phase: p.Phase, ElapsedS: p.ElapsedS, EstS: est, Model: p.Model})
	}
	needs := []statusfile.RunNeed{}
	for _, n := range pr.Needs {
		needs = append(needs, statusfile.RunNeed{ID: n.ID, Kind: n.Kind, Detail: n.Detail})
	}

	run := statusfile.Run{
		Key: s.key, RunID: s.runID, Project: s.project, Title: s.title,
		Status: status, Line: watch.Line(pr),
		Counts: statusfile.Counts{
			Total: pr.Total, Integrated: pr.Integrated, Running: len(pr.Running), Pending: pr.Pending,
			Blocked: pr.Blocked, Needs: len(pr.Needs), Failed: failed,
		},
		Packages: packages, Needs: needs, EtaS: pr.EtaS, CostUSD: pr.CostUSD,
		StartedAt: s.startedAt, UpdatedAt: now.Unix(), EndedAt: endedAt,
		PID: os.Getpid(), State: s.r.statePath,
	}
	err := s.store.WriteRun(run)
	if err == nil {
		_, err = s.store.Rebuild(now)
	}
	if err != nil && !s.warned {
		s.warned = true
		fmt.Fprintf(s.r.stderr, "agent-exec: wave: status file: %s\n", err)
	}
}
