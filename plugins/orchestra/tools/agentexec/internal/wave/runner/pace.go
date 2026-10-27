package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/quota"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/sysload"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/plan"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/state"
)

// Pacing that needs no judgment from the instructor: the memory guard holds
// new starts while the OS reports memory pressure, the Claude 5-hour plan
// window holds them at its stop level (and paces them down before that), a
// budget fixed up front stops new starts once the run's estimated cost
// reaches it, and a usage notice goes out every wave.usage_every integrated
// packages. None of them touches a package already in flight.

// memoryRecheck is how often held starts look at the memory pressure again.
var memoryRecheck = 30 * time.Second

// memoryPressure reports whether the OS is under memory pressure (warn or
// worse) and why; an unsupported platform or a probe that fails is never
// pressure. A variable so tests can inject one.
var memoryPressure = func() (bool, string) {
	p := sysload.MemoryPressure()
	return p.High(), p.Reason
}

// heldLongAfter is how long a memory or usage hold lasts, in seconds, before
// start-held-long reports it: a hold this long reaches the instructor through
// `wave wait`.
var heldLongAfter = 600.0

const (
	// costRefreshSeconds is how long one estimate of the run's cost is
	// reused (it re-reads the ledger and the transcripts).
	costRefreshSeconds = 60
	costTimeout        = 60 * time.Second
)

// holdStarts reports whether new starts wait for an open login need, memory
// pressure or the Claude plan usage to ease (in-flight packages carry on);
// login wins over memory, and memory over usage, when several hold. It
// records one start-held event when holding begins and one start-resumed
// when it ends, and keeps the hold in state while it lasts. A memory or usage
// hold that outlasts heldLongAfter gets one start-held-long.
func (r *runner) holdStarts() bool {
	kind := "login"
	held, reason := r.loginHeld()
	if !held && r.memoryGuard {
		kind = "memory"
		held, reason = memoryPressure()
	}
	if !held {
		kind = "usage"
		held, reason = r.usageHeld()
	}
	switch {
	case held && !r.holding:
		r.holding = true
		r.hold = &state.Held{Kind: kind, Reason: reason, Since: r.clock()}
		_ = r.store.SetHeld(r.hold)
		r.emit("start-held", "", map[string]any{"reason": reason})
		fmt.Fprintf(r.stderr, "start-held: %s\n", reason)
	case held && (r.hold.Kind != kind || r.hold.Reason != reason):
		// The same hold, now for another reason: it keeps its start time.
		r.hold = &state.Held{Kind: kind, Reason: reason, Since: r.hold.Since}
		_ = r.store.SetHeld(r.hold)
	case !held && r.holding:
		r.endHold()
		r.emit("start-resumed", "", map[string]any{})
		fmt.Fprintln(r.stderr, "start-resumed")
	}
	if held && kind != "login" && !r.holdLongSent && r.clock()-r.hold.Since >= heldLongAfter {
		r.holdLongSent = true
		heldS := int(r.clock() - r.hold.Since)
		r.emit("start-held-long", "", map[string]any{"kind": kind, "reason": reason, "held_s": heldS})
		fmt.Fprintf(r.stderr, "start-held-long: %s\n", reason)
	}
	return held
}

// endHold ends the hold bookkeeping and clears the hold recorded in state.
func (r *runner) endHold() {
	r.holding = false
	if r.hold == nil {
		return
	}
	r.hold = nil
	r.holdLongSent = false
	_ = r.store.SetHeld(nil)
}

// runCost is the run's estimated cost in USD, as `agent-exec usage --run
// <run id>` prints it; false without a run id or an estimate. One figure
// is reused for costRefreshSeconds.
func (r *runner) runCost() (float64, bool) {
	if r.opts.RunID == "" {
		return 0, false
	}
	r.costMu.Lock()
	defer r.costMu.Unlock()
	now := r.clock()
	if r.costSeen && now-r.costAt < costRefreshSeconds {
		return r.costUSD, r.costOK
	}
	estimate := r.deps.Cost
	if estimate == nil {
		estimate = r.usageCost
	}
	r.costUSD, r.costOK = estimate(r.opts.RunID)
	r.costAt, r.costSeen = now, true
	return r.costUSD, r.costOK
}

// usageCost runs `agent-exec usage --run ID` and reads its
// cost.estimated_micro_usd.
func (r *runner) usageCost(runID string) (float64, bool) {
	ctx, cancel := context.WithTimeout(r.ctx, costTimeout)
	defer cancel()
	argv := append(append([]string{}, r.deps.AgentExec...), "usage", "--run", runID)
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
	if err != nil {
		return 0, false
	}
	var rep struct {
		Cost struct {
			EstimatedMicroUSD *int64 `json:"estimated_micro_usd"`
		} `json:"cost"`
	}
	if json.Unmarshal(out, &rep) != nil || rep.Cost.EstimatedMicroUSD == nil {
		return 0, false
	}
	return float64(*rep.Cost.EstimatedMicroUSD) / 1e6, true
}

// budgetCheck is the stop reason once the run's estimated cost reached
// the budget ("" = carry on, or no budget).
func (r *runner) budgetCheck() string {
	if r.budgetUSD <= 0 {
		return ""
	}
	if usd, ok := r.runCost(); ok && usd >= r.budgetUSD {
		return fmt.Sprintf("budget reached: ~$%.2f of $%.2f", usd, r.budgetUSD)
	}
	return ""
}

// noteIntegrated counts newly integrated packages and sends a usage notice
// each time the total crosses a multiple of wave.usage_every.
func (r *runner) noteIntegrated(n int) {
	before := r.integratedTotal
	r.integratedTotal += n
	if r.usageEvery <= 0 || before/r.usageEvery == r.integratedTotal/r.usageEvery {
		return
	}
	st := r.store.Snapshot()
	var integrated, inFlight, pending int
	for _, pkg := range r.plan.Packages {
		ps := st.Packages[pkg.ID]
		switch {
		case ps == nil || ps.Status == "pending":
			pending++
		case ps.Status == "integrated":
			integrated++
		case plan.InFlight(ps.Status):
			inFlight++
		}
	}
	detail := map[string]any{"integrated": integrated, "in_flight": inFlight, "pending": pending, "estimated_usd": nil}
	// The estimate and the notify command run off the scheduler, which never
	// waits for either; Run waits for them before it returns.
	r.notifyWG.Add(1)
	go func() {
		defer r.notifyWG.Done()
		if usd, ok := r.runCost(); ok {
			detail["estimated_usd"] = usd
		}
		r.emit("usage", "", detail)
		payload := map[string]any{"event": "usage", "at": r.clock()}
		for k, v := range detail {
			payload[k] = v
		}
		r.notify("usage", payload)
	}()
}

// usageMaxAgeSeconds is how old a cached Claude reading may be and still
// decide: an older one says nothing about the window now.
const usageMaxAgeSeconds = 900

const (
	// usageGuardDefault is enforcement.usage_guard when it is absent or invalid.
	usageGuardDefault = 95
	// usageAdviseDefault is enforcement.usage_advise when it is absent or invalid.
	usageAdviseDefault = 75
	// usageStopFloor is the lowest level new starts are held at, so a low
	// usage_guard still leaves the advice band below the hold.
	usageStopFloor = 95
	// usageAdviseStep is the percent between two usage-advice events.
	usageAdviseStep = 5
)

// claudeUsage returns the cached Claude entry; false when there is none. It
// never fetches, so the scheduler never waits on the network. A variable so
// tests can inject one.
var claudeUsage = func() (quota.Entry, bool) {
	path, source := quota.CachePath(), quota.SourceCache
	if p := os.Getenv(quota.SnapshotEnv); p != "" {
		path, source = p, quota.SourceFixture
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return quota.Entry{}, false
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		return quota.Entry{}, false
	}
	e, ok := quota.EntryFrom(doc["claude"])
	e.Source = source
	return e, ok
}

// usageLevel reads enforcement.<key> as a percent in (0, 100]. off is true
// for "off" or false; a missing key, a non-number or a number out of range
// is def.
func usageLevel(cfg *config.Resolved, key string, def float64) (level float64, off bool) {
	v, ok := cfg.Lookup("enforcement", key)
	if !ok {
		return def, false
	}
	if v == "off" || v == false {
		return 0, true
	}
	if n, isNum := quota.Number(v); isNum && n > 0 && n <= 100 {
		return n, false
	}
	return def, false
}

// usageReading is the Claude 5-hour window the pacing acts on, with the
// levels that apply (0 = that level is off).
type usageReading struct {
	pct    float64
	resets *float64
	stop   float64
	advise float64
}

// usageNow returns the reading when it can decide. The off switches, a
// missing, stale or garbled cache, no 5-hour window and a window that has
// already reset say nothing, so they are not ok: they never hold or advise.
func (r *runner) usageNow() (usageReading, bool) {
	if os.Getenv("ORCHESTRA_USAGE_GUARD") == "off" {
		return usageReading{}, false
	}
	cfg := r.config()
	guard, guardOff := usageLevel(cfg, "usage_guard", usageGuardDefault)
	advise, adviseOff := usageLevel(cfg, "usage_advise", usageAdviseDefault)
	entry, ok := claudeUsage()
	if !ok || entry.FiveHour == nil {
		return usageReading{}, false
	}
	now := r.clock()
	if entry.Source != quota.SourceFixture && (entry.FetchedAt <= 0 || now-entry.FetchedAt > usageMaxAgeSeconds) {
		return usageReading{}, false
	}
	w := entry.FiveHour
	if w.ResetsAt != nil && *w.ResetsAt <= now {
		return usageReading{}, false
	}
	u := usageReading{pct: w.UsedPercent, resets: w.ResetsAt}
	if !guardOff {
		u.stop = max(guard, usageStopFloor)
	}
	if !adviseOff {
		u.advise = advise
	}
	return u, true
}

// usageResetText renders a reset time (epoch seconds) as RFC3339 UTC; ""
// when unknown.
func usageResetText(p *float64) string {
	if p == nil {
		return ""
	}
	return time.Unix(int64(*p), 0).UTC().Format(time.RFC3339)
}

// usageHeld reports whether the Claude 5-hour window is at or above the stop
// level, and why. The hold covers every new start, not only Claude-routed
// ones: the executor is chosen inside dispatch, after the start decision.
func (r *runner) usageHeld() (bool, string) {
	u, ok := r.usageNow()
	if !ok || u.stop <= 0 || u.pct < u.stop {
		return false, ""
	}
	resets := usageResetText(u.resets)
	if resets == "" {
		resets = "unknown"
	}
	return true, fmt.Sprintf("Claude plan usage is %s%% of the 5-hour window (threshold %s%%, resets %s)",
		quota.FormatPercent(u.pct), quota.FormatPercent(u.stop), resets)
}

// usageAdvised is the last usage-advice step each runner emitted; it is
// cleared when the reading falls back below usage_advise. Only the scheduler
// goroutine touches it.
var (
	usageAdviseMu sync.Mutex
	usageAdvised  = map[*runner]float64{}
)

// paceStarts paces new starts down while the Claude 5-hour window is at or
// above usage_advise but below the stop level: one usage-advice event per
// usageAdviseStep above usage_advise, and at most one new start per
// scheduling round. It returns selected unchanged otherwise.
func (r *runner) paceStarts(selected []string) []string {
	if len(selected) == 0 {
		return selected
	}
	u, ok := r.usageNow()
	if !ok || u.advise <= 0 {
		return selected
	}
	usageAdviseMu.Lock()
	defer usageAdviseMu.Unlock()
	if u.pct < u.advise {
		delete(usageAdvised, r)
		return selected
	}
	if u.stop > 0 && u.pct >= u.stop {
		return selected // holdStarts already holds it
	}
	step := u.advise + math.Floor((u.pct-u.advise)/usageAdviseStep)*usageAdviseStep
	if step > usageAdvised[r] {
		usageAdvised[r] = step
		var resets any
		if text := usageResetText(u.resets); text != "" {
			resets = text
		}
		r.emit("usage-advice", "", map[string]any{"percent": u.pct, "resets_at": resets})
	}
	if len(selected) > 1 {
		return selected[:1]
	}
	return selected
}
