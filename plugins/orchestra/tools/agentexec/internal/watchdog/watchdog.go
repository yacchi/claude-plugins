// Package watchdog is the timer-driven safeguard of an external executor
// child: it observes the child's JSONL records and terminates its process
// group on runaway work (wall-clock budget, idle / tool-idle silence, or the
// same tool call repeated back to back).
package watchdog

import (
	"maps"
	"math"
	"slices"
	"sort"
	"sync"
	"time"
)

// DefaultWall is the wall-clock budget per class (watchdog.wall_seconds).
var DefaultWall = map[string]time.Duration{
	"light":              1200 * time.Second,
	"standard":           2400 * time.Second,
	"deep":               5400 * time.Second,
	"independent-review": 2400 * time.Second,
}

// Defaults of the other budgets.
const (
	DefaultIdle        = 600 * time.Second
	DefaultToolIdle    = 900 * time.Second
	DefaultRepeatLimit = 3
	// fallbackWall applies when wall_seconds names neither the class nor
	// standard.
	fallbackWall = 2400 * time.Second
	// maxWait bounds one sleep: a new record can move the idle deadline,
	// so it is re-evaluated twice a second and never missed by more.
	maxWait = 0.5
	// termGrace is how long a SIGTERMed child gets before SIGKILL.
	termGrace = 5 * time.Second
)

// Config is the `watchdog:` section. Wall nil means DefaultWall; a class
// missing from it falls back to standard, then to 2400 s. RepeatLimit <= 0
// disables the repeat check. Build it with ConfigFrom or DefaultConfig: the
// zero value has zero idle budgets.
type Config struct {
	Enabled        bool
	Idle, ToolIdle time.Duration
	RepeatLimit    int
	Wall           map[string]time.Duration
}

// DefaultConfig is the built-in `watchdog:` section.
func DefaultConfig() Config {
	return Config{Enabled: true, Idle: DefaultIdle, ToolIdle: DefaultToolIdle,
		RepeatLimit: DefaultRepeatLimit}
}

// ConfigFrom reads a `watchdog:` section from a decoded
// object: absent keys take the defaults, `enabled` must be literally true,
// a non-object wall_seconds leaves only the 2400 s fallback.
func ConfigFrom(o map[string]any) Config {
	c := DefaultConfig()
	if o == nil {
		return c
	}
	if v, ok := o["enabled"]; ok {
		c.Enabled = v == true
	}
	if d, ok := seconds(o, "idle_seconds"); ok {
		c.Idle = d
	}
	if d, ok := seconds(o, "tool_idle_seconds"); ok {
		c.ToolIdle = d
	}
	if v, ok := o["repeat_limit"]; ok {
		switch n := v.(type) {
		case int64:
			c.RepeatLimit = int(n)
		case float64:
			c.RepeatLimit = int(math.Ceil(n))
		default:
			c.RepeatLimit = 0
		}
	}
	if v, ok := o["wall_seconds"]; ok {
		c.Wall = map[string]time.Duration{}
		if w, isObj := v.(map[string]any); isObj {
			for _, k := range slices.Sorted(maps.Keys(w)) {
				if d, ok := seconds(w, k); ok {
					c.Wall[k] = d
				}
			}
		}
	}
	return c
}

func seconds(o map[string]any, k string) (time.Duration, bool) {
	v := o[k]
	switch n := v.(type) {
	case int64:
		return time.Duration(n) * time.Second, true
	case float64:
		return time.Duration(n * float64(time.Second)), true
	}
	return 0, false
}

// ToolInfo is the last tool call started before the trip.
type ToolInfo struct {
	Name        string
	ArgsPreview string
}

// Runaway is the trip report (`runaway` in a dispatch result).
type Runaway struct {
	Reason string // wall | idle | tool-idle | repeat
	// ElapsedS is the time spent in the budget that tripped (idle: since
	// the last record; wall/repeat: since start), RuntimeS since start,
	// both measured at the trip and rounded to 0.01 s.
	ElapsedS, RuntimeS float64
	LastTool           *ToolInfo
	RepeatCount        int
}

// Object is the report with its keys in a fixed order.
func (r *Runaway) Object() map[string]any {
	var last any
	if r.LastTool != nil {
		last = map[string]any{
			"name":         r.LastTool.Name,
			"args_preview": r.LastTool.ArgsPreview,
		}
	}
	return map[string]any{
		"reason":       r.Reason,
		"elapsed_s":    r.ElapsedS,
		"runtime_s":    r.RuntimeS,
		"last_tool":    last,
		"repeat_count": int64(r.RepeatCount),
	}
}

// Watchdog observes one child.
type Watchdog struct {
	cfg   Config
	cls   string
	clock func() float64 // seconds, monotonic

	mu          sync.Mutex
	started     float64
	lastRecord  float64
	active      map[string]bool
	sequence    int
	lastToolKey string
	hasLastTool bool
	lastTool    *ToolInfo
	repeatCount int
	reason      string
	firedAt     float64
	windowStart float64

	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

// New is a watchdog for a child of class cls (an unknown class uses the
// standard budget). Its clock starts now.
func New(c Config, cls string) *Watchdog {
	base := time.Now()
	return newWithClock(c, cls, func() float64 { return time.Since(base).Seconds() })
}

func newWithClock(c Config, cls string, clock func() float64) *Watchdog {
	if _, ok := DefaultWall[cls]; !ok {
		cls = "standard"
	}
	now := clock()
	return &Watchdog{cfg: c, cls: cls, clock: clock, started: now, lastRecord: now,
		active: map[string]bool{}, stop: make(chan struct{})}
}

// Enabled reports whether Start runs the timer.
func (w *Watchdog) Enabled() bool { return w.cfg.Enabled }

// Start runs the timer when enabled; terminate is called once, from the
// timer goroutine, when a budget trips (it should end the process group).
func (w *Watchdog) Start(terminate func()) {
	if !w.cfg.Enabled || w.done != nil {
		return
	}
	w.done = make(chan struct{})
	go w.watch(terminate)
}

// Stop ends the timer and waits for it (including a terminate in flight).
func (w *Watchdog) Stop() {
	w.stopOnce.Do(func() { close(w.stop) })
	if w.done != nil {
		<-w.done
	}
}

func (w *Watchdog) watch(terminate func()) {
	defer close(w.done)
	for {
		w.mu.Lock()
		remaining, fired := w.check(w.clock())
		w.mu.Unlock()
		if fired {
			if terminate != nil {
				terminate()
			}
			return
		}
		wait := math.Min(maxWait, math.Max(0.01, remaining))
		t := time.NewTimer(time.Duration(wait * float64(time.Second)))
		select {
		case <-w.stop:
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// OnLine takes one stdout record of the child.
func (w *Watchdog) OnLine(line string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastRecord = w.clock()
	if event, ok := decodeObject(line); ok {
		w.piEvent(event)
	}
}

func (w *Watchdog) piEvent(event map[string]any) {
	switch event["type"] {
	case "tool_execution_start":
		name, ok := event["toolName"]
		if !ok {
			name = ""
		}
		args, ok := event["args"]
		if !ok {
			args = map[string]any{}
		}
		w.startTool(name, args, event["toolCallId"])
	case "tool_execution_end":
		w.endTool(event["toolCallId"])
	case "turn_end", "agent_end":
		// Every tool of a turn has returned before the turn ends.
		w.active = map[string]bool{}
	}
}

// callKey keys an in-flight call: string ids as is (so they share the
// space of the anonymous ids), anything else by its JSON.
func callKey(id any) string {
	if s, ok := id.(string); ok {
		return s
	}
	return "\x00" + Canonical(id)
}

func (w *Watchdog) startTool(name, args, id any) {
	key := Canonical(name) + "\x00" + Canonical(args)
	if w.hasLastTool && key == w.lastToolKey {
		w.repeatCount++
	} else {
		w.lastToolKey, w.hasLastTool, w.repeatCount = key, true, 1
	}
	if id == nil {
		w.sequence++
		w.active["anonymous-"+itoa(w.sequence)] = true
	} else {
		w.active[callKey(id)] = true
	}
	w.lastTool = &ToolInfo{Name: toolName(name), ArgsPreview: preview(args)}
	if w.cfg.RepeatLimit > 0 && w.repeatCount >= w.cfg.RepeatLimit {
		w.fire("repeat", w.clock(), w.started)
	}
}

// endTool closes one in-flight call. An end without an id, or whose id was
// never seen while id-less starts are open, closes what it can match so a
// malformed pairing cannot pin the tool-idle budget forever.
func (w *Watchdog) endTool(id any) {
	if id == nil {
		w.active = map[string]bool{}
		return
	}
	if k := callKey(id); w.active[k] {
		delete(w.active, k)
		return
	}
	var anonymous []string
	for t := range w.active {
		if len(t) > 10 && t[:10] == "anonymous-" {
			anonymous = append(anonymous, t)
		}
	}
	if len(anonymous) > 0 {
		sort.Strings(anonymous)
		delete(w.active, anonymous[0])
	}
}

// ToolRunning reports whether a tool call is in flight.
func (w *Watchdog) ToolRunning() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.active) > 0
}

// fire records the trip: when it fired and where the exceeded budget
// began, so the report never includes the time spent terminating.
func (w *Watchdog) fire(reason string, now, window float64) {
	if w.reason == "" {
		w.reason, w.firedAt, w.windowStart = reason, now, window
	}
}

func (w *Watchdog) wallLimit() float64 {
	wall := w.cfg.Wall
	if wall == nil {
		wall = DefaultWall
	}
	if d, ok := wall[w.cls]; ok {
		return d.Seconds()
	}
	if d, ok := wall["standard"]; ok {
		return d.Seconds()
	}
	return fallbackWall.Seconds()
}

// check evaluates every budget at now (lock held). It fires at most one
// reason and returns the seconds to the nearest deadline, or fired.
func (w *Watchdog) check(now float64) (remaining float64, fired bool) {
	if w.reason != "" {
		return 0, true
	}
	limit := w.wallLimit()
	if now-w.started >= limit {
		w.fire("wall", now, w.started)
		return 0, true
	}
	if w.cfg.RepeatLimit > 0 && w.repeatCount >= w.cfg.RepeatLimit {
		w.fire("repeat", now, w.started)
		return 0, true
	}
	running := len(w.active) > 0
	idle, reason := w.cfg.Idle.Seconds(), "idle"
	if running {
		idle, reason = w.cfg.ToolIdle.Seconds(), "tool-idle"
	}
	if now-w.lastRecord >= idle {
		w.fire(reason, now, w.lastRecord)
		return 0, true
	}
	return math.Min(w.started+limit, w.lastRecord+idle) - now, false
}

// Result is the trip report, or nil when no budget tripped.
func (w *Watchdog) Result() *Runaway {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.reason == "" {
		return nil
	}
	var last *ToolInfo
	if w.lastTool != nil {
		cp := *w.lastTool
		last = &cp
	}
	return &Runaway{
		Reason:      w.reason,
		ElapsedS:    round2(math.Max(0, w.firedAt-w.windowStart)),
		RuntimeS:    round2(math.Max(0, w.firedAt-w.started)),
		LastTool:    last,
		RepeatCount: w.repeatCount,
	}
}

func round2(x float64) float64 { return math.Round(x*100) / 100 }

// TerminateGroup SIGTERMs process group pgid and, unless exited closes
// within the grace period, SIGKILLs it. A group already gone is left alone.
func TerminateGroup(pgid int, exited <-chan struct{}) {
	if err := killGroup(pgid, false); err != nil {
		return
	}
	t := time.NewTimer(termGrace)
	defer t.Stop()
	select {
	case <-exited:
	case <-t.C:
		_ = killGroup(pgid, true)
	}
}
