package runner

import (
	"encoding/json"
	"maps"
	"sort"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/approvals"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/plan"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/state"
)

func (r *runner) loop() {
	if r.verifyMode == "final" {
		r.runWaves()
		r.converge()
		return
	}
	for {
		r.runWaves()
		r.mu.Lock()
		pending := r.integratedSinceFull
		r.mu.Unlock()
		if r.opts.Full == "" || pending == 0 {
			return
		}
		// A final red --full that sent packages back re-enters the loop.
		fullLock.Lock()
		r.intLock.Lock()
		reset := r.full()
		r.intLock.Unlock()
		fullLock.Unlock()
		if len(reset) == 0 || r.stopped() {
			return
		}
	}
}

type job struct{ pid, phase, note string }

// pool runs package jobs on at most width goroutines at once; done
// receives each finished job's id.
type pool struct {
	sem  chan struct{}
	done chan int
	next int
}

func newPool(width int) *pool {
	return &pool{sem: make(chan struct{}, max(1, width)), done: make(chan int, 64)}
}

func (p *pool) submit(r *runner, j job) int {
	id := p.next
	p.next++
	go func() {
		p.sem <- struct{}{}
		defer func() {
			<-p.sem
			p.done <- id
		}()
		r.runPackage(j.pid, j.phase, j.note)
	}()
	return id
}

// schedule submits every job that can start now. It returns whether
// anything was picked and whether the run is stopping.
func (r *runner) schedule(p *pool, running map[int]string) (progressed, halted bool) {
	if r.stopped() {
		return false, true
	}
	if r.isUnavailable() {
		return false, false
	}
	if n, ok := r.host.(host.Network); ok && r.host != nil {
		n.NetworkTick()
	}
	st := r.store.Snapshot()
	busy := map[string]bool{}
	for _, pid := range running {
		busy[pid] = true
	}
	view := plan.Status{}
	for id, ps := range st.Packages {
		view[id] = ps.Status
	}
	// A submitted package may not have left `pending` yet in its goroutine.
	for pid := range busy {
		if s, ok := view[pid]; !ok || s == "pending" {
			view[pid] = "implementing"
		}
	}
	status := func(pid string) *state.Package { return st.Packages[pid] }
	var carried, awaiting []string
	for _, pkg := range r.plan.Packages {
		ps := status(pkg.ID)
		if ps == nil || busy[pkg.ID] {
			continue
		}
		if ps.Status == "verifying" || ps.Status == "ready" {
			carried = append(carried, pkg.ID)
		}
		if ps.Status == "implementing" && ps.Detail == "await-orca" && r.hostSession(pkg.ID) != nil {
			awaiting = append(awaiting, pkg.ID)
		}
	}
	selected := plan.Select(r.plan, view, r.maxInFlight)
	if len(selected) == 0 && len(carried) == 0 && len(awaiting) == 0 {
		return false, false
	}
	if reason := r.stopCheck(); reason != "" {
		r.stop(reason)
		halted = true
		selected = nil
		if len(carried) == 0 {
			return false, true
		}
	} else if r.opts.MaxPackages != nil {
		r.mu.Lock()
		left := max(*r.opts.MaxPackages-r.started, 0)
		r.mu.Unlock()
		if len(selected) > left {
			selected = selected[:left]
		}
	}
	var verify []string
	for _, pid := range carried {
		if status(pid).Status == "verifying" {
			verify = append(verify, pid)
		}
	}
	if len(verify) == 0 && len(awaiting) == 0 && len(selected) == 0 {
		return len(carried) > 0, halted
	}

	r.mu.Lock()
	r.waves++
	r.mu.Unlock()
	_ = r.store.SetWave(st.Wave + 1)
	r.event("wave-start", "", strings.Join(append(append(append([]string{}, carried...), awaiting...), selected...), ","))
	var jobs []job
	for _, pid := range verify {
		jobs = append(jobs, job{pid, "verify", ""})
	}
	for _, pid := range awaiting {
		jobs = append(jobs, job{pid, "await", ""})
	}
	r.intLock.Lock()
	head := r.intHead()
	for _, pid := range selected {
		if ok, note := r.refresh(pid, head); ok {
			jobs = append(jobs, job{pid, "implement", note})
		}
	}
	r.intLock.Unlock()
	r.mu.Lock()
	r.started += len(selected)
	r.mu.Unlock()
	for _, j := range jobs {
		running[p.submit(r, j)] = j.pid
	}
	return true, halted
}

// afterJobs integrates every ready package; in gated mode it runs --full
// every full_every integrating rounds. Serialized on the scheduler.
func (r *runner) afterJobs() {
	r.intLock.Lock()
	defer r.intLock.Unlock()
	integrated := r.integrateReady()
	if integrated > 0 {
		r.integrateRounds++
	}
	every := r.opts.FullEvery
	if every < 1 {
		every = 1
	}
	r.mu.Lock()
	pending := r.integratedSinceFull
	r.mu.Unlock()
	if r.verifyMode == "gated" && r.opts.Full != "" && integrated > 0 && pending > 0 &&
		r.integrateRounds%every == 0 && !r.stopped() {
		fullLock.Lock()
		r.full()
		fullLock.Unlock()
	}
}

// runWaves is the rolling scheduler: keep up to max_in_flight jobs running;
// each finished job is integrated at once and frees its slot.
func (r *runner) runWaves() {
	running := map[int]string{}
	p := newPool(r.maxInFlight)
	halted := false
	for {
		progressed, stopNow := r.schedule(p, running)
		halted = halted || stopNow
		if len(running) > 0 {
			delete(running, <-p.done)
			for drained := false; !drained; {
				select {
				case id := <-p.done:
					delete(running, id)
				default:
					drained = true
				}
			}
			r.afterJobs()
			continue
		}
		if r.isUnavailable() {
			if r.stopped() || !r.waitForReset() {
				break
			}
			continue
		}
		r.afterJobs()
		if halted || r.stopped() || !progressed {
			break
		}
	}
}

// --- report ------------------------------------------------------------------------

func (r *runner) report() Report {
	st := r.store.Snapshot()
	view := plan.Status{}
	for id, ps := range st.Packages {
		view[id] = ps.Status
	}
	blocked := plan.Blocked(r.plan, view)
	withStatus := func(status string) []string {
		var out []string
		for _, pkg := range r.plan.Packages {
			if ps := st.Packages[pkg.ID]; ps != nil && ps.Status == status {
				out = append(out, pkg.ID)
			}
		}
		return out
	}
	threshold := r.checksConfig().flakyThreshold
	flaky := map[string]int{}
	for _, ev := range state.ReadEvents(r.statePath, -1) {
		if kind, _ := ev["event"]; kind != "flaky" {
			continue
		}
		detail := ev["detail"]
		var d struct {
			Files []any `json:"files"`
		}
		_ = json.Unmarshal([]byte(str(detail)), &d)
		for _, f := range d.Files {
			flaky[pyStr(f)]++
		}
	}
	approvalsN := 0
	if items, err := approvals.PendingIn(r.statePath, r.configOrDefaults()); err == nil {
		approvalsN = len(items)
	}
	r.mu.Lock()
	rep := Report{Reason: r.stopReason, Waves: r.waves}
	r.mu.Unlock()
	rep.Status = "done"
	if rep.Reason != nil {
		rep.Status = "stopped"
	}
	rep.Approvals, rep.State = approvalsN, r.statePath
	rep.Integrated = withStatus("integrated")
	for _, n := range st.Needs {
		rep.Needs = append(rep.Needs, NeedRef{ID: n.ID, Kind: n.Kind})
	}
	for _, pkg := range r.plan.Packages {
		ps := st.Packages[pkg.ID]
		if ps == nil {
			continue
		}
		for _, d := range ps.NetworkDenials {
			m := asMap(d)
			if m == nil {
				continue
			}
			o := maps.Clone(m)
			o["id"] = pkg.ID
			rep.NetworkDenials = append(rep.NetworkDenials, o)
		}
	}
	for _, pid := range withStatus("pending") {
		if _, isBlocked := blocked[pid]; isBlocked {
			rep.Blocked = append(rep.Blocked, pid)
		} else {
			rep.Pending = append(rep.Pending, pid)
		}
	}
	files := make([]string, 0, len(flaky))
	for f, n := range flaky {
		if n >= threshold {
			files = append(files, f)
		}
	}
	sort.Slice(files, func(i, j int) bool {
		if flaky[files[i]] != flaky[files[j]] {
			return flaky[files[i]] > flaky[files[j]]
		}
		return files[i] < files[j]
	})
	for _, f := range files {
		rep.FlakyTests = append(rep.FlakyTests, FlakyTest{File: f, Count: flaky[f]})
	}
	return rep
}
