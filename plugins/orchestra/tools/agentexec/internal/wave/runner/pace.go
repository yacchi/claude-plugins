package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/plan"
)

// Pacing that needs no judgment from the instructor: the memory guard holds
// new starts while the OS reports memory pressure, a budget fixed up front
// stops new starts once the run's estimated cost reaches it, and a usage
// notice goes out every wave.usage_every integrated packages. None of them
// touches a package already in flight.

// memoryRecheck is how often held starts look at the memory pressure again.
var memoryRecheck = 30 * time.Second

// memoryPressure reports whether the OS is under memory pressure and why;
// an unsupported platform or a probe that fails is never pressure. A
// variable so tests can inject one.
var memoryPressure = probeMemoryPressure

const (
	// costRefreshSeconds is how long one estimate of the run's cost is
	// reused (it re-reads the ledger and the transcripts).
	costRefreshSeconds = 60
	costTimeout        = 60 * time.Second
)

// darwinPressure reads kern.memorystatus_vm_pressure_level: 1 normal, 2
// warn, 4 critical; warn or worse holds.
func darwinPressure(level uint32) (bool, string) {
	switch {
	case level >= 4:
		return true, "memory pressure critical (kern.memorystatus_vm_pressure_level 4)"
	case level >= 2:
		return true, fmt.Sprintf("memory pressure warn (kern.memorystatus_vm_pressure_level %d)", level)
	}
	return false, ""
}

// linuxPressure holds when /proc/pressure/memory's `some avg10` is 20 or
// more; without PSI it falls back to MemAvailable below 10 % of MemTotal.
// A nil input is a file that could not be read.
func linuxPressure(psi, meminfo []byte) (bool, string) {
	if psi != nil {
		for _, line := range splitLines(string(psi)) {
			fields := strings.Fields(line)
			if len(fields) == 0 || fields[0] != "some" {
				continue
			}
			for _, f := range fields[1:] {
				if v, ok := strings.CutPrefix(f, "avg10="); ok {
					if avg, err := strconv.ParseFloat(v, 64); err == nil {
						if avg >= 20 {
							return true, fmt.Sprintf("memory pressure (PSI some avg10 %.1f)", avg)
						}
						return false, ""
					}
				}
			}
		}
	}
	if meminfo == nil {
		return false, ""
	}
	kb := map[string]float64{}
	sc := bufio.NewScanner(bytes.NewReader(meminfo))
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		if fields := strings.Fields(rest); len(fields) > 0 {
			if v, err := strconv.ParseFloat(fields[0], 64); err == nil {
				kb[name] = v
			}
		}
	}
	total, okT := kb["MemTotal"]
	avail, okA := kb["MemAvailable"]
	if !okT || !okA || total <= 0 {
		return false, ""
	}
	if avail/total < 0.10 {
		return true, fmt.Sprintf("low memory (MemAvailable %.0f%% of MemTotal)", 100*avail/total)
	}
	return false, ""
}

// holdStarts reports whether new starts wait for memory pressure to ease
// (in-flight packages carry on); it records one start-held event when
// holding begins and one start-resumed when it ends.
func (r *runner) holdStarts() bool {
	held, reason := false, ""
	if r.memoryGuard {
		held, reason = memoryPressure()
	}
	switch {
	case held && !r.holding:
		r.holding = true
		r.emit("start-held", "", map[string]any{"reason": reason})
		fmt.Fprintf(r.stderr, "start-held: %s\n", reason)
	case !held && r.holding:
		r.holding = false
		r.emit("start-resumed", "", map[string]any{})
		fmt.Fprintln(r.stderr, "start-resumed")
	}
	return held
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
