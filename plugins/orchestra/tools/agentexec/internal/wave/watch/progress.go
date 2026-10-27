package watch

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/plan"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/state"
)

const (
	listMax      = 5
	needDetailCh = 120
	// minRatioSamples is how many integrated packages with an estimate it
	// takes before their actual/estimated ratio scales the ETA.
	minRatioSamples = 2
	unbounded       = 1 << 30
)

// Progress is the `progress` object of `wave status --json` and `wave wait`.
type Progress struct {
	RunID      *string   `json:"run_id"`
	Total      int       `json:"total"`
	Integrated int       `json:"integrated"`
	Pending    int       `json:"pending"`
	Blocked    int       `json:"blocked"`
	Running    []Running `json:"running"`
	// Interrupted are the in-flight packages whose runner is gone.
	Interrupted []Interrupted `json:"interrupted"`
	Needs       []Need        `json:"needs"`
	Next        []string      `json:"next"`
	Recent      []Recent      `json:"recent"`
	ElapsedS    int           `json:"elapsed_s"`
	EtaS        *int          `json:"eta_s"`
	EtaBasis    string        `json:"eta_basis"`
	CostUSD     *float64      `json:"cost_usd"`
	Held        *Held         `json:"held,omitempty"`
}

// Held is the start hold the runner is in: it starts no package while one
// lasts. HeldS is how long it has lasted, in seconds.
type Held struct {
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
	Since  int64  `json:"since"`
	HeldS  int    `json:"held_s"`
}

// Running is one package in flight.
type Running struct {
	ID         string   `json:"id"`
	Cls        *string  `json:"cls"`
	Executor   *string  `json:"executor"`
	Model      *string  `json:"model"`
	Phase      string   `json:"phase"`
	StartedAt  float64  `json:"started_at"`
	ElapsedS   int      `json:"elapsed_s"`
	EstSeconds *float64 `json:"est_seconds"`
}

// Interrupted is a package left in flight by a runner that no longer runs.
type Interrupted struct {
	ID    string  `json:"id"`
	Phase string  `json:"phase"`
	Since float64 `json:"since"`
}

// Need is one entry of the needs list; ID is nil for a wave-level need.
type Need struct {
	ID     *string `json:"id"`
	Kind   string  `json:"kind"`
	Detail string  `json:"detail"`
}

// Recent is one integrated package.
type Recent struct {
	ID string  `json:"id"`
	At float64 `json:"at"`
}

// CostFunc is a run's estimated cost in USD (`agent-exec usage --run`);
// false when there is none.
type CostFunc func(runID string) (float64, bool)

// Compute is the progress of the wave at statePath from its state, events,
// plan (state.plan; without a readable plan only the state's counts are
// known) and runner.pid. cost may be nil.
func Compute(statePath string, st *state.State, now float64, cost CostFunc) Progress {
	var p *plan.Plan
	if st.Plan != "" {
		p, _ = plan.Load(st.Plan)
	}
	rec, hasRec := ReadRecord(statePath)
	in := inputs{
		plan: p, state: st, events: state.ReadEvents(statePath, -1), now: now,
		alive: hasRec && rec.Alive(statePath),
	}
	if hasRec {
		in.record = &rec
	}
	pr := build(in)
	pr.Held = heldOf(st, now)
	if pr.RunID != nil && cost != nil {
		if usd, ok := cost(*pr.RunID); ok {
			pr.CostUSD = &usd
		}
	}
	return pr
}

// heldOf is the state's start hold with how long it has lasted; nil without one.
func heldOf(st *state.State, now float64) *Held {
	if st.Held == nil {
		return nil
	}
	return &Held{
		Kind: st.Held.Kind, Reason: st.Held.Reason,
		Since: int64(math.Floor(st.Held.Since)),
		HeldS: int(max(math.Floor(now-st.Held.Since), 0)),
	}
}

type inputs struct {
	plan   *plan.Plan // nil when unreadable
	state  *state.State
	events []map[string]any
	record *Record // nil without runner.pid
	alive  bool
	now    float64
}

// eventLog is what progress reads from events.jsonl.
type eventLog struct {
	// started is the latest move to implementing per package.
	started map[string]float64
	// model is the latest model an event detail named per package.
	model map[string]string
	// integrating is the task list of an integrate-start not yet ended.
	integrating map[string]bool
	// width is the latest max-in-flight event's integer (0 = unbounded).
	width int
	first float64
}

func readLog(events []map[string]any) eventLog {
	l := eventLog{started: map[string]float64{}, model: map[string]string{}}
	for i, ev := range events {
		at := num(ev["at"])
		if i == 0 {
			l.first = at
		}
		pkg, _ := ev["pkg"].(string)
		kind, _ := ev["event"].(string)
		detail := detailObject(ev["detail"])
		if pkg != "" {
			if m, ok := detail["model"].(string); ok && m != "" {
				l.model[pkg] = m
			}
		}
		switch kind {
		case "status":
			if ev["to"] == "implementing" && pkg != "" {
				l.started[pkg] = at
			}
		case "integrate-start":
			l.integrating = map[string]bool{}
			if tasks, ok := detail["tasks"].([]any); ok {
				for _, t := range tasks {
					if s, ok := t.(string); ok {
						l.integrating[s] = true
					}
				}
			}
		case "integrate-end":
			l.integrating = nil
		case "max-in-flight":
			l.width = 0
			if n, ok := detail["value"].(int64); ok && n > 0 {
				l.width = int(n)
			}
		}
	}
	return l
}

// detailObject decodes an event detail that is a JSON object (the runner's
// mechanical events); anything else is nil.
func detailObject(v any) map[string]any {
	s, ok := v.(string)
	if !ok || !strings.HasPrefix(s, "{") {
		return nil
	}
	doc, err := fsutil.DecodeJSON([]byte(s))
	if err != nil {
		return nil
	}
	o, _ := doc.(map[string]any)
	return o
}

func num(v any) float64 {
	switch x := v.(type) {
	case int64:
		return float64(x)
	case float64:
		return x
	}
	return 0
}

func ptr[T any](v T) *T { return &v }

func build(in inputs) Progress {
	st, now := in.state, in.now
	log := readLog(in.events)
	pr := Progress{
		Running: []Running{}, Interrupted: []Interrupted{}, Needs: []Need{}, Next: []string{}, Recent: []Recent{},
		EtaBasis: "none",
	}
	if in.record != nil {
		if id, ok := in.record.Flag("run-id"); ok && id != "" {
			pr.RunID = &id
		}
	}

	pkgOf := func(id string) (plan.Package, bool) {
		if in.plan == nil {
			return plan.Package{}, false
		}
		return in.plan.Package(id)
	}
	statuses := plan.Status{}
	for id, ps := range st.Packages {
		statuses[id] = ps.Status
	}
	var blocked map[string]string
	if in.plan != nil {
		blocked = plan.Blocked(in.plan, statuses)
	}

	// gone: a runner record exists for this state and its process is not
	// running. Without a record (foreground run) liveness is unknown.
	gone := in.record != nil && !in.alive

	ids := slices.Sorted(maps.Keys(st.Packages))
	pr.Total = len(ids)
	var recent []Recent
	for _, id := range ids {
		ps := st.Packages[id]
		switch {
		case ps.Status == "integrated":
			pr.Integrated++
			recent = append(recent, Recent{ID: id, At: ps.Since})
		case ps.Status == "blocked":
			pr.Blocked++
		case ps.Status == "pending":
			if _, b := blocked[id]; b {
				pr.Blocked++
			} else {
				pr.Pending++
			}
		case state.InFlight(ps.Status) && gone:
			pr.Interrupted = append(pr.Interrupted, Interrupted{ID: id, Phase: phase(id, ps, log), Since: ps.Since})
		case state.InFlight(ps.Status):
			r := Running{ID: id, Executor: ps.Executor, Phase: phase(id, ps, log)}
			if pkg, ok := pkgOf(id); ok {
				r.Cls = ptr(pkg.Cls)
				if pkg.EstSeconds > 0 {
					r.EstSeconds = ptr(pkg.EstSeconds)
				}
			}
			if m, ok := log.model[id]; ok {
				r.Model = ptr(m)
			}
			r.StartedAt = ps.Since
			if at, ok := log.started[id]; ok {
				r.StartedAt = at
			}
			r.ElapsedS = int(max(now-r.StartedAt, 0))
			pr.Running = append(pr.Running, r)
		}
	}
	sort.SliceStable(recent, func(i, j int) bool { return recent[i].At > recent[j].At })
	if len(recent) > listMax {
		recent = recent[:listMax]
	}
	pr.Recent = append(pr.Recent, recent...)

	for _, n := range st.Needs {
		pr.Needs = append(pr.Needs, Need{ID: n.ID, Kind: n.Kind, Detail: clip(firstLine(n.Detail), needDetailCh)})
	}

	start := log.first
	if in.record != nil {
		start = float64(in.record.StartedAt)
	}
	end := now
	if !in.alive {
		end = st.Updated
	}
	if start > 0 {
		pr.ElapsedS = int(max(end-start, 0))
	}

	if in.plan != nil {
		width := log.width
		if width <= 0 {
			width = unbounded
		}
		f := forecast(in.plan, st, log.started, now, width, gone)
		pr.Next = append(pr.Next, f.next...)
		if f.estimated {
			eta := int(math.Round(f.makespan * ratio(in.plan, st, log.started)))
			pr.EtaS, pr.EtaBasis = &eta, "critical-path"
		}
	}
	return pr
}

// phase is what an in-flight package is doing: its status, a ready one
// integrating only while an integrate call names it, an Orca/herdr session
// awaited after a dialog waiting.
func phase(id string, ps *state.Package, log eventLog) string {
	switch ps.Status {
	case "implementing":
		if ps.Detail == "await-orca" {
			return "waiting"
		}
		return "implementing"
	case "verifying":
		return "verifying"
	case "fixing":
		return "correcting"
	}
	if log.integrating[id] {
		return "integrating"
	}
	return "waiting"
}

// ratio is sum(actual)/sum(est) over the integrated packages that have an
// estimate and a recorded start; 1 until there are minRatioSamples of them.
func ratio(p *plan.Plan, st *state.State, started map[string]float64) float64 {
	var actual, est float64
	n := 0
	for _, pkg := range p.Packages {
		ps := st.Packages[pkg.ID]
		at, ok := started[pkg.ID]
		if ps == nil || ps.Status != "integrated" || pkg.EstSeconds <= 0 || !ok || ps.Since < at {
			continue
		}
		actual += ps.Since - at
		est += pkg.EstSeconds
		n++
	}
	if n < minRatioSamples || est <= 0 {
		return 1
	}
	return actual / est
}

type forecastResult struct {
	makespan  float64
	estimated bool // some unfinished package has est_seconds
	next      []string
}

// forecast replays the scheduler over the unfinished packages: the ones in
// flight occupy their slots for what is left of their estimate (never
// below 0), every other package that is not failed starts the moment
// plan.Select would start it (dependencies integrated, no overlapping
// files_owned, at most width in flight) and runs for its estimate (0 when
// it has none). makespan is when the last one finishes; next is the first
// packages it starts, in that order. A package in needs or blocked is
// assumed to be resolved, one depending on a failed package never starts.
func forecast(p *plan.Plan, st *state.State, started map[string]float64, now float64, width int, runnerGone bool) forecastResult {
	var res forecastResult
	sim := plan.Status{}
	finish := map[string]float64{}
	est := map[string]float64{}
	for _, pkg := range p.Packages {
		est[pkg.ID] = pkg.EstSeconds
		status := "pending"
		if ps := st.Packages[pkg.ID]; ps != nil {
			status = ps.Status
		}
		switch {
		case status == "integrated" || status == "failed":
			sim[pkg.ID] = status
			continue
		case state.InFlight(status) && !runnerGone:
			sim[pkg.ID] = "implementing"
			left := pkg.EstSeconds
			if at, ok := started[pkg.ID]; ok {
				left -= now - at
			} else if ps := st.Packages[pkg.ID]; ps != nil {
				left -= now - ps.Since
			}
			finish[pkg.ID] = max(left, 0)
		default:
			sim[pkg.ID] = "pending"
		}
		if pkg.EstSeconds > 0 {
			res.estimated = true
		}
	}
	t := 0.0
	for {
		for _, id := range plan.Select(p, sim, width) {
			sim[id] = "implementing"
			finish[id] = t + est[id]
			if len(res.next) < listMax {
				res.next = append(res.next, id)
			}
		}
		if len(finish) == 0 {
			break
		}
		t = math.Inf(1)
		for _, f := range finish {
			t = min(t, f)
		}
		for id, f := range finish {
			if f <= t {
				sim[id] = "integrated"
				delete(finish, id)
			}
		}
	}
	res.makespan = t
	return res
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSuffix(line, "\r")
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// Line is the one-line summary: `wave 12/40 · 3 running · 1 needs · ~35m
// left · ~$4.10`, the ETA and cost parts only when known.
func Line(p Progress) string {
	parts := []string{
		fmt.Sprintf("wave %d/%d", p.Integrated, p.Total),
		fmt.Sprintf("%d running", len(p.Running)),
	}
	if n := len(p.Interrupted); n > 0 {
		parts = append(parts, fmt.Sprintf("%d interrupted", n))
	}
	parts = append(parts, fmt.Sprintf("%d needs", len(p.Needs)))
	if p.Held != nil {
		parts = append(parts, "held: "+p.Held.Kind)
	}
	if p.EtaS != nil {
		parts = append(parts, "~"+formatETA(*p.EtaS)+" left")
	}
	if p.CostUSD != nil {
		parts = append(parts, "~$"+strconv.FormatFloat(*p.CostUSD, 'f', 2, 64))
	}
	return strings.Join(parts, " · ")
}

// formatETA is whole minutes rounded up: `35m`, `2h10m`.
func formatETA(seconds int) string {
	m := (max(seconds, 0) + 59) / 60
	if m < 60 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh%02dm", m/60, m%60)
}
