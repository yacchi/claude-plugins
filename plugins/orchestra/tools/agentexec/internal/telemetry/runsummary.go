package telemetry

import "strconv"

// RunTask is one wave package as the summary counts it: the plan package's
// cls, and its wave state status, executor ("" when none) and attempts.
type RunTask struct {
	Cls, Status, Executor string
	Attempts              int
}

// RunSummary is the (pre-sanitize) run_summary record of one run, from its
// wave packages (nil when the run had no wave) and its ledger dispatch records.
func RunSummary(tasks []RunTask, dispatches []map[string]any) map[string]any {
	out := map[string]any{"event": "run_summary", "lane": "orchestrated"}
	if len(tasks) > 0 {
		runSummaryTasks(out, tasks)
	} else {
		runSummaryDispatches(out, dispatches)
	}
	fallbacks := int64(0)
	usage := map[string]any{}
	sums := map[string]int64{}
	for _, d := range dispatches {
		if s, _ := d["status"].(string); s == "unavailable" {
			fallbacks++
		}
		for _, key := range usageKeys {
			if n, ok := nonNegInt(d[key]); ok {
				sums[key] += n
				usage[key] = sums[key]
			}
		}
	}
	out["fallbacks"] = fallbacks
	if len(usage) > 0 {
		out["usage"] = usage
	}
	return out
}

// runSummaryTasks counts the wave packages of a run that had a wave.
func runSummaryTasks(out map[string]any, tasks []RunTask) {
	var pass, fail, exhausted int64
	classes, rounds, executors := map[string]any{}, map[string]any{}, map[string]any{}
	for _, t := range tasks {
		switch t.Status {
		case "integrated":
			pass++
		case "failed", "blocked":
			fail++
		case "needs":
			exhausted++
		}
		if t.Cls != "" {
			runSummaryBump(classes, t.Cls)
		}
		if t.Attempts >= 1 {
			runSummaryBump(rounds, strconv.Itoa(t.Attempts))
		}
		switch t.Executor {
		case "":
		case "pi":
			runSummaryBump(executors, "pi")
		default:
			runSummaryBump(executors, "claude")
		}
	}
	out["task_count"] = int64(len(tasks))
	out["pass"] = pass
	out["fail"] = fail
	out["exhausted"] = exhausted
	runSummarySetHist(out, "classes", classes)
	runSummarySetHist(out, "rounds", rounds)
	runSummarySetHist(out, "executors_used", executors)
}

// runSummaryDispatches counts the dispatch records of a run that had no wave.
func runSummaryDispatches(out map[string]any, dispatches []map[string]any) {
	var pass, fail int64
	classes, executors := map[string]any{}, map[string]any{}
	for _, d := range dispatches {
		if s, _ := d["status"].(string); s == "ok" {
			pass++
		} else {
			fail++
		}
		if c, _ := d["cls"].(string); c != "" {
			runSummaryBump(classes, c)
		}
		if e, _ := d["executor"].(string); e != "" {
			runSummaryBump(executors, e)
		}
	}
	out["task_count"] = int64(len(dispatches))
	out["pass"] = pass
	out["fail"] = fail
	runSummarySetHist(out, "classes", classes)
	runSummarySetHist(out, "executors_used", executors)
}

// runSummaryBump adds one to the count under key.
func runSummaryBump(m map[string]any, key string) {
	n, _ := m[key].(int64)
	m[key] = n + 1
}

// runSummarySetHist writes a histogram only when it has entries.
func runSummarySetHist(out map[string]any, key string, hist map[string]any) {
	if len(hist) > 0 {
		out[key] = hist
	}
}
