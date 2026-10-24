package watch

import (
	"bytes"
	"io"
	"os"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/state"
)

// DefaultMaxWait is `wave wait`'s default window, under the Bash tool's
// 600 s limit.
const DefaultMaxWait = 560 * time.Second

// readLockTimeout bounds the wait for the runner's state lock; past it the
// files are read unlocked.
const readLockTimeout = 5 * time.Second

// Answer is what `wave wait` prints.
type Answer struct {
	Status    string     `json:"status"`
	Seq       int        `json:"seq"`
	Line      string     `json:"line"`
	Progress  Progress   `json:"progress"`
	Approvals []Approval `json:"approvals"`
	Machine   Machine    `json:"machine"`
}

// WaitOptions are `wave wait`'s inputs.
type WaitOptions struct {
	State string
	// Since is the seq of an earlier answer: needs recorded by event lines
	// before it are not new.
	Since   int
	MaxWait time.Duration
	// Poll is the re-check interval (0 = 1 s).
	Poll time.Duration
	// UntilChange also ends the wait on a viewer-visible event recorded
	// after event line Since (changed).
	UntilChange bool
	Cost        CostFunc
	// Approvals are the wave's pending approvals (nil: none listed).
	Approvals func(statePath string) []Approval
	// Machine probes the machine (nil: nothing probed).
	Machine func() Machine
}

// Wait blocks until the runner of o.State is gone (done, stopped or
// crashed), a need recorded after event line o.Since is on the needs list
// (needs), with o.UntilChange a viewer-visible event was recorded after
// that line (changed), or o.MaxWait passed (running). A need wins over the
// runner being gone: deciding it and starting the runner again is the next
// step either way. Both win over a change: they say more.
func Wait(o WaitOptions) Answer {
	poll := o.Poll
	if poll <= 0 {
		poll = time.Second
	}
	deadline := time.Now().Add(max(o.MaxWait, 0))
	tail := &eventTail{path: state.EventsPath(o.State), since: o.Since,
		pkgs: map[string]bool{}, wave: map[string]bool{}}
	for {
		rec, hasRec := ReadRecord(o.State)
		alive := hasRec && rec.Alive(o.State)
		snap := read(o.State, tail)
		status := ""
		switch {
		case snap.exists && tail.newNeed(snap.st.Needs):
			status = "needs"
		case !alive:
			status = finalStatus(o.State, hasRec, snap)
		case o.UntilChange && tail.changed:
			status = "changed"
		case !time.Now().Before(deadline):
			status = "running"
		}
		if status != "" {
			pr := Compute(o.State, &snap.st, wallClock(), o.Cost)
			return Answer{
				Status: status, Seq: tail.lines, Line: Line(pr), Progress: pr,
				Approvals: o.approvals(), Machine: o.machine(),
			}
		}
		time.Sleep(min(poll, time.Until(deadline)))
	}
}

func (o WaitOptions) approvals() []Approval {
	var out []Approval
	if o.Approvals != nil {
		out = o.Approvals(o.State)
	}
	if out == nil {
		out = []Approval{}
	}
	return out
}

func (o WaitOptions) machine() Machine {
	if o.Machine == nil {
		return UnknownMachine()
	}
	return o.Machine()
}

func wallClock() float64 { return float64(time.Now().UnixNano()) / 1e9 }

type snapshot struct {
	st     state.State
	exists bool
}

// read loads state.json and the new event lines under a shared hold of
// the state lock, so the two agree: a need on the list has its event line
// counted.
func read(statePath string, tail *eventTail) snapshot {
	if unlock, err := fsutil.Lock(statePath+".lock", fsutil.Shared, readLockTimeout); err == nil {
		defer unlock()
	}
	snap := snapshot{st: state.State{Packages: map[string]*state.Package{}}}
	if store, err := state.Open(statePath); err == nil {
		if st, err := store.Load(); err == nil {
			snap.st, snap.exists = st, true
		}
	}
	tail.advance()
	return snap
}

// eventTail follows events.jsonl: lines counts its complete lines, pkgs
// and wave the packages moved to needs and the wave-level need details
// recorded at or after line since, changed whether a viewer-visible event
// was.
type eventTail struct {
	path    string
	since   int
	offset  int64
	lines   int
	pkgs    map[string]bool
	wave    map[string]bool
	changed bool
}

// viewerEvents are the events that change what a viewer of the wave sees:
// a package status, a dispatch starting or ending, an integration, a need,
// a network denial (a new approval) and a stop.
var viewerEvents = map[string]bool{
	"status": true, "dispatch-start": true, "dispatch-end": true,
	"integrate-start": true, "integrate-end": true, "integrate": true,
	"need": true, "network-denied": true, "stop": true,
}

func (t *eventTail) advance() {
	f, err := os.Open(t.path)
	if err != nil {
		return
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || info.Size() < t.offset {
		return // replaced under us; the next answer's seq still counts it
	}
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return
	}
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			return // a partial last line is read once it is complete
		}
		line := data[:i]
		data = data[i+1:]
		t.offset += int64(i + 1)
		if t.lines >= t.since {
			t.note(line)
		}
		t.lines++
	}
}

func (t *eventTail) note(line []byte) {
	doc, err := fsutil.DecodeJSON(bytes.TrimSpace(line))
	if err != nil {
		return
	}
	ev, _ := doc.(map[string]any)
	if kind, _ := ev["event"].(string); viewerEvents[kind] {
		t.changed = true
	}
	switch ev["event"] {
	case "status":
		if pkg, ok := ev["pkg"].(string); ok && ev["to"] == "needs" {
			t.pkgs[pkg] = true
		}
	case "need":
		if d, ok := ev["detail"].(string); ok {
			t.wave[d] = true
		}
	}
}

// newNeed reports whether a need on the list was recorded after since.
func (t *eventTail) newNeed(needs []state.Need) bool {
	for _, n := range needs {
		if (n.ID != nil && t.pkgs[*n.ID]) || (n.ID == nil && t.wave[n.Detail]) {
			return true
		}
	}
	return false
}

// finalStatus is how a gone runner ended. A detached one (runner.pid)
// printed its end report into runner.log: done or stopped from it, crashed
// without one. Without runner.pid only the state tells: stopped when it
// records a stop, crashed while a package is still in flight, else done.
func finalStatus(statePath string, detached bool, snap snapshot) string {
	if detached {
		data, _ := os.ReadFile(LogPath(statePath))
		lines := bytes.Split(data, []byte("\n"))
		for i := len(lines) - 1; i >= 0; i-- {
			doc, err := fsutil.DecodeJSON(bytes.TrimSpace(lines[i]))
			if err != nil {
				continue
			}
			o, _ := doc.(map[string]any)
			if s, _ := o["status"].(string); s == "done" || s == "stopped" {
				return s
			} else if s != "" {
				return "crashed"
			}
		}
		return "crashed"
	}
	if !snap.exists {
		return "crashed"
	}
	if snap.st.Stopped != nil {
		return "stopped"
	}
	for _, p := range snap.st.Packages {
		if state.InFlight(p.Status) {
			return "crashed"
		}
	}
	return "done"
}
