package state

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/gitx"
)

// ReadEvents returns the last limit event records (all when limit < 0),
// tolerating a truncated last line.
func ReadEvents(statePath string, limit int) []map[string]any {
	data, err := os.ReadFile(EventsPath(statePath))
	if err != nil {
		return nil
	}
	var records []map[string]any
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		doc, err := fsutil.DecodeJSON(line)
		if err != nil {
			continue // a concurrent writer's last line may still be mid-flush
		}
		if o, ok := doc.(map[string]any); ok {
			records = append(records, o)
		}
	}
	if limit >= 0 && limit < len(records) {
		records = records[len(records)-limit:]
	}
	return records
}

func formatElapsed(seconds float64) string {
	s := max(int(seconds), 0)
	if s < 3600 {
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh%02dm", s/3600, s%3600/60)
}

// firstLine is the first line of s ("" when empty).
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSuffix(line, "\r")
}

// valText renders a decoded JSON value for text output; "-" for null/absent.
func valText(v any) string {
	switch x := v.(type) {
	case nil:
		return "-"
	case string:
		if x == "" {
			return "-"
		}
		return x
	}
	b, _ := fsutil.MarshalJSON(v)
	return string(b)
}

func formatEventLine(ev map[string]any) string {
	get := func(k string) any { return ev[k] }
	pkg := "-"
	if s, ok := get("pkg").(string); ok && s != "" {
		pkg = s
	}
	event, _ := get("event").(string)
	parts := []string{pkg, event}
	if from, to := valText(ev["from"]), valText(ev["to"]); from != "-" || to != "-" {
		parts = append(parts, from+"->"+to)
	}
	if d, ok := get("detail").(string); ok && d != "" {
		parts = append(parts, firstLine(d))
	}
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "  ")
}

// LiveChanges counts, per in-flight package whose tree exists, the lines of
// `git status --porcelain`; nil when git fails there.
func LiveChanges(st *State) map[string]any {
	result := map[string]any{}
	for id, p := range st.Packages {
		if !InFlight(p.Status) || p.Tree == nil {
			continue
		}
		if info, err := os.Stat(*p.Tree); err != nil || !info.IsDir() {
			continue
		}
		out, err := gitx.Run(*p.Tree, "status", "--porcelain", "--untracked-files=all")
		if err != nil {
			result[id] = nil
			continue
		}
		result[id] = strings.Count(strings.TrimRight(out, "\n"), "\n") + boolInt(out != "")
	}
	return result
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// RenderOptions are the optional parts of RenderStatus.
type RenderOptions struct {
	PlanTitles map[string]string
	Events     []map[string]any
	Live       bool
	// ApprovalsPending > 0 adds the "approvals: N pending" line.
	ApprovalsPending int
	StatePath        string
	// RunnerGone: the runner is known to have ended, so in-flight packages
	// are interrupted, not running. False also when liveness is unknown.
	RunnerGone bool
}

func statusIndex(status string) int {
	for i, s := range Statuses {
		if s == status {
			return i
		}
	}
	return len(Statuses)
}

// RenderStatus is the human-readable status: header, per-package rows,
// needs, network denials, approvals and recent events.
func RenderStatus(st *State, now float64, o RenderOptions) string {
	var lines []string
	counts := map[string]int{}
	running := 0
	for _, p := range st.Packages {
		counts[p.Status]++
		if InFlight(p.Status) {
			running++
		}
	}
	header := "wave " + strconv.Itoa(st.Wave)
	var countParts []string
	for _, s := range Statuses {
		if counts[s] > 0 {
			countParts = append(countParts, s+"="+strconv.Itoa(counts[s]))
		}
	}
	if len(countParts) > 0 {
		header += "  " + strings.Join(countParts, " ")
	}
	if o.RunnerGone && running > 0 {
		header += "  interrupted: " + strconv.Itoa(running) + " (runner gone)"
	} else {
		header += "  running: " + strconv.Itoa(running) + " in flight"
	}
	if st.Stopped != nil {
		header += "  stopped: " + st.Stopped.Reason
	}
	lines = append(lines, header)

	var ids []string
	for id, p := range st.Packages {
		if p.Status != "pending" {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := statusIndex(st.Packages[ids[i]].Status), statusIndex(st.Packages[ids[j]].Status)
		if a != b {
			return a < b
		}
		return ids[i] < ids[j]
	})
	var changes map[string]any
	if o.Live {
		changes = LiveChanges(st)
	}
	for _, id := range ids {
		p := st.Packages[id]
		since := p.Since
		if since == 0 {
			since = now
		}
		executor := "-"
		if p.Executor != nil && *p.Executor != "" {
			executor = *p.Executor
		}
		files := "-"
		if p.FilesChanged != nil {
			files = strconv.Itoa(*p.FilesChanged)
		}
		if c, ok := changes[id]; ok && c != nil {
			files = strconv.Itoa(c.(int)) + "*"
		}
		row := fmt.Sprintf("  %s  %s  %s  %s  %s  %d", id, p.Status, formatElapsed(now-since), executor, files, p.Attempts)
		if o.RunnerGone && InFlight(p.Status) {
			row += "  (interrupted)"
		}
		if title, ok := o.PlanTitles[id]; ok {
			row += "  " + title
		}
		lines = append(lines, row)
	}

	if len(st.Needs) > 0 {
		lines = append(lines, "needs:")
		for _, n := range st.Needs {
			id := "(wave)"
			if n.ID != nil {
				id = *n.ID
			}
			lines = append(lines, fmt.Sprintf("  %s  %s  %s", id, n.Kind, firstLine(n.Detail)))
		}
	}

	var denied []string
	for _, id := range ids {
		for _, d := range st.Packages[id].NetworkDenials {
			obj, _ := d.(map[string]any)
			denied = append(denied, fmt.Sprintf("  %s  %s:%s", id, valText(obj["host"]), valText(obj["port"])))
		}
	}
	if len(denied) > 0 {
		lines = append(lines, "network denied:")
		lines = append(lines, denied...)
	}

	if o.ApprovalsPending > 0 {
		lines = append(lines, fmt.Sprintf("approvals: %d pending — agent-exec approvals --state %s", o.ApprovalsPending, o.StatePath))
	}

	if len(o.Events) > 0 {
		lines = append(lines, "recent:")
		recent := o.Events
		if len(recent) > 5 {
			recent = recent[len(recent)-5:]
		}
		for _, ev := range recent {
			lines = append(lines, "  "+formatEventLine(ev))
		}
	}
	return strings.Join(lines, "\n")
}
