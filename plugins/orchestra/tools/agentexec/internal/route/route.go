// Package route maps a capability class (and task archetype) to the concrete
// executor that should run it: the first candidate of the configured
// priority list that is not exhausted, cooled down, misconfigured or not
// ready. claude is the terminal fallback.
package route

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/cooldown"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/readiness"
)

// Skipped is one candidate that lost, with why.
type Skipped struct {
	Executor string
	Reason   string
}

// CooldownApplied is a candidate skipped because of a cooldown.
type CooldownApplied struct {
	Executor string
	Reason   string
	Until    float64
}

// Route is the `route --json` document. Model and Effort are whatever the
// config holds (nil when unset); Executor and Dispatch are empty when
// nothing resolves.
type Route struct {
	Class            string
	Archetype        string
	Candidates       []string
	Skipped          []Skipped
	Source           string
	CooldownsApplied []CooldownApplied
	CooldownBypassed bool
	Executor         string
	Dispatch         any
	Model            any
	Effort           any
	AgentType        any
	Remaining        []string
}

// Routable reports whether an executor was chosen.
func (r Route) Routable() bool { return r.Executor != "" }

func strs(l []string) []any {
	out := make([]any, len(l))
	for i, s := range l {
		out[i] = s
	}
	return out
}

// Object renders the route as `route --json` prints it.
func (r Route) Object() map[string]any {
	skipped := make([]any, len(r.Skipped))
	for i, s := range r.Skipped {
		skipped[i] = map[string]any{"executor": s.Executor, "reason": s.Reason}
	}
	applied := make([]any, len(r.CooldownsApplied))
	for i, c := range r.CooldownsApplied {
		applied[i] = map[string]any{"executor": c.Executor, "reason": c.Reason, "until": c.Until}
	}
	o := map[string]any{
		"class":             r.Class,
		"archetype":         r.Archetype,
		"candidates":        strs(r.Candidates),
		"skipped":           skipped,
		"source":            r.Source,
		"cooldowns_applied": applied,
		"cooldown_bypassed": r.CooldownBypassed,
	}
	if !r.Routable() {
		for _, k := range []string{"executor", "dispatch", "model", "effort", "agent_type"} {
			o[k] = nil
		}
		o["remaining"] = []any{}
		return o
	}
	o["remaining"] = strs(r.Remaining)
	o["executor"] = r.Executor
	o["dispatch"] = r.Dispatch
	o["model"] = r.Model
	o["effort"] = r.Effort
	o["agent_type"] = r.AgentType
	return o
}

func candidates(cfg *config.Resolved, cls, archetype string) ([]string, string) {
	cp := config.ObjectAt(cfg.Section("priority"), cls)
	for _, key := range []string{archetype, "default"} {
		v := cp[key]
		if list, ok := v.([]any); ok && len(list) > 0 {
			out := make([]string, 0, len(list))
			for _, e := range list {
				if s, isStr := e.(string); isStr {
					out = append(out, s)
				}
			}
			return out, "priority"
		}
	}
	return []string{"claude"}, "tiers-default"
}

func skipReason(name, cls string, exhausted map[string]bool, external map[string]any,
	ready map[string]readiness.ExecReady, cd map[string]cooldown.Entry) string {
	if _, known := readiness.KnownExecutors[name]; name != "claude" && !known {
		return "unknown-executor:" + name
	}
	if exhausted[name] {
		return "exhausted"
	}
	if entry, ok := cd[name]; ok {
		reason := entry.Reason
		if reason == "" {
			reason = "unknown"
		}
		return "cooldown:" + reason
	}
	if name == "claude" {
		return ""
	}
	ecfg := config.ObjectAt(external, name)
	if ecfg == nil {
		return "not-configured"
	}
	if v, _ := ecfg["enabled"]; v != true {
		return "disabled"
	}
	if config.ObjectAt(config.ObjectAt(ecfg, "class_policy"), cls) == nil {
		return "no-class-policy"
	}
	dispatch := ecfg["dispatch"]
	switch dispatch {
	case "cli":
		entry, ok := ready[name]
		if !ok || !entry.OK {
			if ok && len(entry.Missing) > 0 {
				return "not-ready:" + strings.Join(entry.Missing, ",")
			}
			return "not-ready:unknown"
		}
		return ""
	case "agent":
		// Binary presence is informational for agent dispatch; the real
		// availability is the subagent's session, which is not knowable here.
		return ""
	}
	return fmt.Sprintf("unknown-dispatch:%s", show(dispatch))
}

// Resolve walks the candidates for cls/archetype and returns the first
// survivor. When every candidate is skipped and some cooldown was active,
// the walk is repeated ignoring cooldowns (cooldown_bypassed).
func Resolve(cfg *config.Resolved, ready map[string]readiness.ExecReady, cls, archetype string,
	exhausted []string, cd map[string]cooldown.Entry) Route {
	exSet := map[string]bool{}
	for _, e := range exhausted {
		exSet[e] = true
	}
	cands, source := candidates(cfg, cls, archetype)
	external := cfg.Section("external_executors")

	walk := func(cdm map[string]cooldown.Entry) (string, int, []Skipped) {
		skipped := []Skipped{}
		for i, name := range cands {
			reason := skipReason(name, cls, exSet, external, ready, cdm)
			if reason == "" {
				return name, i, skipped
			}
			skipped = append(skipped, Skipped{Executor: name, Reason: reason})
		}
		return "", -1, skipped
	}

	winner, idx, skipped := walk(cd)
	bypassed := false
	if winner == "" && len(cd) > 0 {
		winner, idx, skipped = walk(nil)
		bypassed = true
	}

	r := Route{
		Class: cls, Archetype: archetype, Candidates: cands, Skipped: skipped,
		Source: source, CooldownBypassed: bypassed,
		CooldownsApplied: []CooldownApplied{},
	}
	for _, s := range skipped {
		if entry, ok := cd[s.Executor]; ok && strings.HasPrefix(s.Reason, "cooldown:") {
			r.CooldownsApplied = append(r.CooldownsApplied,
				CooldownApplied{Executor: s.Executor, Reason: entry.Reason, Until: entry.Until})
		}
	}
	if winner == "" {
		return r
	}
	r.Executor = winner
	r.Remaining = append([]string{}, cands[idx+1:]...)
	if winner == "claude" {
		model := cfg.Section("tiers")[cls]
		r.Dispatch, r.Model = "claude", model
		return r
	}
	ecfg := config.ObjectAt(external, winner)
	dispatch := ecfg["dispatch"]
	policy := config.ObjectAt(config.ObjectAt(ecfg, "class_policy"), cls)
	r.Dispatch = dispatch
	r.Model = policy["model"]
	r.Effort = policy["effort"]
	if dispatch == "agent" {
		r.AgentType = ecfg["agent_type"]
	}
	return r
}

// show formats a nullable route field for text output: nil as "-".
func show(v any) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprint(v)
}

// FormatText is the human-readable `route --text` rendering (with trailing
// newline).
func FormatText(r Route) string {
	var b strings.Builder
	b.WriteString("agent-exec route\n-----------------\n")
	fmt.Fprintf(&b, "class: %s (archetype: %s)\n", r.Class, r.Archetype)
	fmt.Fprintf(&b, "candidates: %s\n", strings.Join(r.Candidates, ", "))
	if r.Routable() {
		fmt.Fprintf(&b, "resolved: %s (dispatch=%s model=%s effort=%s)\n",
			r.Executor, show(r.Dispatch), show(r.Model), show(r.Effort))
	} else {
		b.WriteString("resolved: UNROUTABLE\n")
	}
	if len(r.Skipped) > 0 {
		b.WriteString("skipped:\n")
		for _, s := range r.Skipped {
			fmt.Fprintf(&b, "  - %s: %s\n", s.Executor, s.Reason)
		}
	}
	for _, c := range r.CooldownsApplied {
		fmt.Fprintf(&b, "cooldown: %s (%s) until %s\n", c.Executor, c.Reason, strconv.FormatFloat(c.Until, 'f', -1, 64))
	}
	if r.CooldownBypassed {
		b.WriteString("cooldown: BYPASSED (all candidates were cooled down)\n")
	}
	if len(r.Remaining) > 0 {
		fmt.Fprintf(&b, "remaining: %s\n", strings.Join(r.Remaining, ", "))
	}
	fmt.Fprintf(&b, "source: %s\n", r.Source)
	return b.String()
}
