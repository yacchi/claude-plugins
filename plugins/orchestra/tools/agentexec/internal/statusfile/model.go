// Package statusfile is orchestra's machine-wide status file,
// ~/.claude/orchestra/status.json: what every wave runner and planner on this
// machine is doing, rebuilt from one small file per run and per plan.
package statusfile

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

const (
	Version           = 1             // status.json schema version
	HeartbeatSeconds  = 60            // a live runner rewrites its entry at least this often
	StaleAfterSeconds = 180           // a running/needs entry with a live pid not rewritten for this long is "stale"
	PlanStaleSeconds  = 3600          // a "planning" entry not rewritten for this long is "stale"
	EndedKeepSeconds  = 6 * 3600      // an entry no live runner keeps fresh leaves status.json this long after updated_at
	FileKeepSeconds   = 7 * 24 * 3600 // run and plan files older than this are deleted (store.go)
)

// PlanStatuses are the statuses a planner writes; PlanPhases its phases.
var (
	PlanStatuses = []string{"planning", "done", "needs-user", "failed"}
	PlanPhases   = []string{"explore", "verify", "critique", "design", "synthesize", "reality-check", "lint", "done"}
)

// Doc is the whole of status.json.
type Doc struct {
	Version     int     `json:"version"`
	GeneratedAt int64   `json:"generated_at"`
	HeartbeatS  int     `json:"heartbeat_s"`
	StaleAfterS int     `json:"stale_after_s"`
	Summary     Summary `json:"summary"`
	Runs        []Run   `json:"runs"`
	Plans       []Plan  `json:"plans"`
}

// Summary names no repository, path, run id or package id: it may be published alone.
type Summary struct {
	Busy      bool   `json:"busy"`
	Running   int    `json:"running"`
	Planning  int    `json:"planning"`
	Needs     int    `json:"needs"`
	Crashed   int    `json:"crashed"`
	UpdatedAt *int64 `json:"updated_at"`
	Line      string `json:"line"`
}

// Run is one wave run, as its runner last wrote it.
type Run struct {
	Key       string       `json:"key"`
	RunID     *string      `json:"run_id"`
	Project   string       `json:"project"`
	Title     string       `json:"title"`
	Status    string       `json:"status"` // written: running|needs|done|stopped; Build adds crashed|stale
	Line      string       `json:"line"`
	Counts    Counts       `json:"counts"`
	Packages  []RunPackage `json:"packages"`
	Needs     []RunNeed    `json:"needs"`
	EtaS      *int         `json:"eta_s"`
	CostUSD   *float64     `json:"cost_usd"`
	StartedAt int64        `json:"started_at"`
	UpdatedAt int64        `json:"updated_at"`
	EndedAt   *int64       `json:"ended_at"`
	PID       int          `json:"pid"`
	State     string       `json:"state"`
}

// Counts tallies a run's packages by phase.
type Counts struct {
	Total      int `json:"total"`
	Integrated int `json:"integrated"`
	Running    int `json:"running"`
	Pending    int `json:"pending"`
	Blocked    int `json:"blocked"`
	Needs      int `json:"needs"`
	Failed     int `json:"failed"`
}

// RunPackage is one package of a run in flight.
type RunPackage struct {
	ID       string  `json:"id"`
	Phase    string  `json:"phase"`
	ElapsedS int     `json:"elapsed_s"`
	EstS     *int    `json:"est_s"`
	Model    *string `json:"model"`
}

// RunNeed is something a run waits on the user for.
type RunNeed struct {
	ID     *string `json:"id"`
	Kind   string  `json:"kind"`
	Detail string  `json:"detail"`
}

// Plan is one planner's plan.
type Plan struct {
	PlanID       string `json:"plan_id"`
	Project      string `json:"project"`
	Title        string `json:"title"`
	Status       string `json:"status"` // written: PlanStatuses; Build adds stale
	Phase        string `json:"phase"`
	Questions    int    `json:"questions"`
	Packages     *int   `json:"packages"`
	EstTotalS    *int   `json:"est_total_s"`
	EstCriticalS *int   `json:"est_critical_s"`
	StartedAt    int64  `json:"started_at"`
	UpdatedAt    int64  `json:"updated_at"`
}

// Build is status.json from the run and plan files at now (unix seconds);
// alive reports whether a pid is a live process. It never mutates its inputs.
func Build(runs []Run, plans []Plan, now int64, alive func(pid int) bool) Doc {
	doc := Doc{
		Version:     Version,
		GeneratedAt: now,
		HeartbeatS:  HeartbeatSeconds,
		StaleAfterS: StaleAfterSeconds,
	}

	var s Summary
	var updated *int64
	note := func(at int64) {
		if updated == nil || at > *updated {
			updated = &at
		}
	}

	doc.Runs = []Run{}
	for _, r := range runs {
		if r.Packages == nil {
			r.Packages = []RunPackage{}
		}
		if r.Needs == nil {
			r.Needs = []RunNeed{}
		}
		ended := r.EndedAt != nil || r.Status == "done" || r.Status == "stopped"
		if !ended {
			switch {
			case !alive(r.PID):
				r.Status = "crashed"
			case now-r.UpdatedAt > StaleAfterSeconds:
				r.Status = "stale"
			}
		}
		live := !ended && (r.Status == "running" || r.Status == "needs")
		if !live && now-r.UpdatedAt > EndedKeepSeconds {
			continue
		}

		doc.Runs = append(doc.Runs, r)
		note(r.UpdatedAt)
		if live {
			s.Running++
		}
		switch r.Status {
		case "needs":
			s.Needs++
		case "crashed", "stale":
			s.Crashed++
		}
	}
	slices.SortFunc(doc.Runs, func(a, b Run) int {
		if c := cmp.Compare(b.UpdatedAt, a.UpdatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.Key, b.Key)
	})

	doc.Plans = []Plan{}
	for _, p := range plans {
		if p.Status == "planning" && now-p.UpdatedAt > PlanStaleSeconds {
			p.Status = "stale"
		}
		if now-p.UpdatedAt > EndedKeepSeconds {
			continue
		}

		doc.Plans = append(doc.Plans, p)
		note(p.UpdatedAt)
		switch p.Status {
		case "planning":
			s.Planning++
		case "needs-user":
			s.Needs++
		}
	}
	slices.SortFunc(doc.Plans, func(a, b Plan) int {
		if c := cmp.Compare(b.UpdatedAt, a.UpdatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.PlanID, b.PlanID)
	})

	s.Busy = s.Running > 0 || s.Planning > 0
	s.UpdatedAt = updated
	s.Line = summaryLine(s)
	doc.Summary = s
	return doc
}

// summaryLine joins the non-zero counts, or says idle when all are zero.
func summaryLine(s Summary) string {
	var parts []string
	if s.Running > 0 {
		parts = append(parts, fmt.Sprintf("%d running", s.Running))
	}
	if s.Planning > 0 {
		parts = append(parts, fmt.Sprintf("%d planning", s.Planning))
	}
	if s.Needs > 0 {
		parts = append(parts, fmt.Sprintf("%d needs", s.Needs))
	}
	if s.Crashed > 0 {
		parts = append(parts, fmt.Sprintf("%d crashed", s.Crashed))
	}
	if len(parts) == 0 {
		return "idle"
	}
	return strings.Join(parts, " · ")
}
