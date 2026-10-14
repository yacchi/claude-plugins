// Package usage aggregates token usage and estimated cost: Claude Code
// transcripts (read-only), the pi run ledger and orchestra's own telemetry.
package usage

import (
	"os"
	"sort"
	"syscall"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/ledger"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/telemetry"
)

// Sources lists the known sources in report order.
var Sources = []string{"claude", "pi"}

// ReportOptions selects what BuildReport reads. Home, Cwd and ConfigDir
// default to $HOME, the process's working directory and $CLAUDE_CONFIG_DIR.
type ReportOptions struct {
	Since, Now  time.Time
	Sources     []string
	AllProjects bool
	Cfg         *config.Resolved
	Home, Cwd   string
	ConfigDir   string
	RunIDs      []string
	SessionIDs  []string
}

// RunCount is one run's record count in a session-scoped pi source.
type RunCount struct {
	Run   string
	Count int64
}

// Source is one source's entry in the report. A scope entry that cannot be
// attributed carries only Reason.
type Source struct {
	NotAttributable bool
	Reason          string

	Status, Scope, Note string
	NoteLast            bool // the note follows the counts (pi's telemetry entry)

	Tokens, Main, Sidechain Tokens
	Breakdown               bool // main/sidechain/by_model/sessions are reported (claude)
	ByModel                 map[string]Tokens
	ByModelDetail           map[string]Detail
	HasDetail               bool
	Sessions, Records       int64
	Cost                    *Cost

	CostMicroUSD    int64
	HasCostMicroUSD bool
	Runs            []RunCount
}

func notAttributable(reason string) *Source {
	return &Source{NotAttributable: true, Reason: reason}
}

// MarshalJSON writes the entry with a fixed key order.
func (s Source) MarshalJSON() ([]byte, error) {
	o := map[string]any{}
	if s.NotAttributable {
		o["attributable"] = false
		o["reason"] = s.Reason
		return fsutil.MarshalJSON(o)
	}
	o["status"] = s.Status
	o["scope"] = s.Scope
	if s.Note != "" && !s.NoteLast {
		o["note"] = s.Note
	}
	o["tokens"] = s.Tokens
	if s.Breakdown {
		o["main"] = s.Main
		o["sidechain"] = s.Sidechain
		o["by_model"] = s.ByModel
		if s.HasDetail {
			o["by_model_detail"] = s.ByModelDetail
		}
	}
	if s.HasCostMicroUSD {
		o["cost_micro_usd"] = s.CostMicroUSD
	}
	if s.Breakdown {
		o["sessions"] = s.Sessions
	}
	o["records"] = s.Records
	if s.Cost != nil {
		o["cost"] = s.Cost
	}
	if len(s.Runs) > 0 {
		runs := map[string]any{}
		for _, r := range s.Runs {
			runs[r.Run] = r.Count
		}
		o["runs"] = runs
	}
	if s.Note != "" && s.NoteLast {
		o["note"] = s.Note
	}
	return fsutil.MarshalJSON(o)
}

// Scope says what the report covers.
type Scope struct {
	Kind       string   `json:"kind"`
	Since      string   `json:"since,omitempty"`
	Until      string   `json:"until,omitempty"`
	RunIDs     []string `json:"run_ids,omitempty"`
	SessionIDs []string `json:"session_ids,omitempty"`
}

// CostBlock is the report's cost summary.
type CostBlock struct {
	EstimatedMicroUSD int64            `json:"estimated_micro_usd"`
	Components        map[string]int64 `json:"components"`
	PricedSources     []string         `json:"priced_sources"`
	UnpricedSources   []string         `json:"unpriced_sources"`
	PriceList         PriceList        `json:"price_list"`
}

// Report is the usage report. Since and Now are only set for a window.
type Report struct {
	Since   string     `json:"since,omitempty"`
	Now     string     `json:"now,omitempty"`
	Scope   Scope      `json:"scope"`
	Sources SourceList `json:"sources"`
	Totals  Tokens     `json:"totals"`
	Cost    CostBlock  `json:"cost"`
}

// SourceList is the ordered source name -> entry list (claude before pi).
type SourceList struct {
	Names   []string
	Entries map[string]*Source
}

func (s SourceList) MarshalJSON() ([]byte, error) {
	o := map[string]any{}
	for _, n := range s.Names {
		o[n] = *s.Entries[n]
	}
	return fsutil.MarshalJSON(o)
}

func getwd() string {
	if wd, err := syscall.Getwd(); err == nil {
		return wd
	}
	wd, _ := os.Getwd()
	return wd
}

// BuildReport assembles the full report. Every requested source gets an
// entry, even an unavailable one.
func BuildReport(o ReportOptions) Report {
	scoped := len(o.RunIDs) > 0 || len(o.SessionIDs) > 0
	var overrides map[string]any
	if o.Cfg != nil {
		overrides = o.Cfg.Section("pricing")
	}
	entries := map[string]*Source{}
	var names []string
	for _, name := range Sources {
		if !contains(o.Sources, name) {
			continue
		}
		names = append(names, name)
		switch {
		case scoped && name == "claude":
			entries[name] = collectScoped(o)
		case scoped && name == "pi":
			entries[name] = scopedLedger(o)
		case scoped:
			entries[name] = notAttributable("session scope is not attributable")
		case name == "claude":
			entries[name] = collectWindow(o)
		default:
			entries[name] = telemetryUsage(o.Since, o.Cfg, name)
		}
	}
	srcs := SourceList{Names: names, Entries: entries}

	r := Report{Sources: srcs, Totals: sumTotals(srcs), Cost: costBlock(srcs, overrides)}
	switch {
	case len(o.RunIDs) > 0 && len(o.SessionIDs) > 0:
		r.Scope = Scope{Kind: "run+session", RunIDs: o.RunIDs, SessionIDs: o.SessionIDs}
	case len(o.RunIDs) > 0:
		r.Scope = Scope{Kind: "run", RunIDs: o.RunIDs}
	case len(o.SessionIDs) > 0:
		r.Scope = Scope{Kind: "session", SessionIDs: o.SessionIDs}
	default:
		r.Scope = Scope{Kind: "window", Since: fsutil.FormatISO(o.Since), Until: fsutil.FormatISO(o.Now)}
		r.Since, r.Now = r.Scope.Since, r.Scope.Until
	}
	return r
}

func contains(list []string, s string) bool {
	for _, a := range list {
		if a == s {
			return true
		}
	}
	return false
}

// sumTotals sums the sources that actually reported ("ok"): unavailable and
// empty ones must not pass zeros off as measurements.
func sumTotals(s SourceList) Tokens {
	var t Tokens
	for _, n := range s.Names {
		if e := s.Entries[n]; !e.NotAttributable && e.Status == "ok" {
			t.add(e.Tokens, 1)
		}
	}
	return t
}

// costBlock prices the sources and (side effect) stores Claude's own
// per-model estimate on its entry. Claude is estimated from list prices; pi
// is the figure the provider itself reported.
func costBlock(s SourceList, overrides map[string]any) CostBlock {
	cb := CostBlock{
		Components:      map[string]int64{},
		PricedSources:   []string{},
		UnpricedSources: []string{},
		PriceList:       PriceList{AsOf: PriceListAsOf, Source: PriceListSource},
	}
	if c := s.Entries["claude"]; c != nil && !c.NotAttributable && c.Status == "ok" {
		est := Estimate(c.ByModelDetail, overrides)
		c.Cost = &est
		cb.Components["claude"] = est.MicroUSD
		cb.PricedSources = append(cb.PricedSources, "claude")
		models := make([]string, 0, len(est.UnpricedModels))
		for m := range est.UnpricedModels {
			models = append(models, m)
		}
		sort.Strings(models)
		for _, m := range models {
			cb.UnpricedSources = append(cb.UnpricedSources, "claude:"+m)
		}
	}
	if p := s.Entries["pi"]; p != nil && !p.NotAttributable && p.Status == "ok" {
		cb.Components["pi"] = p.CostMicroUSD
		cb.PricedSources = append(cb.PricedSources, "pi")
	}
	for _, v := range cb.Components {
		cb.EstimatedMicroUSD += v
	}
	return cb
}

// telemetryUsage is a cli-dispatch executor's windowed usage, which only
// exists in orchestra's own telemetry log. A disabled or absent log is
// "unavailable": reporting zeros would read as "that executor cost nothing".
func telemetryUsage(cutoff time.Time, cfg *config.Resolved, executor string) *Source {
	s := &Source{Status: "unavailable", Scope: "global", NoteLast: true}
	if !telemetry.Enabled(cfg) {
		s.Note = "telemetry disabled; no " + executor + " usage is recorded"
		return s
	}
	path := telemetry.RecordsPath(cfg)
	if fi, err := os.Stat(path); err != nil || !fi.Mode().IsRegular() {
		s.Note = "no telemetry records file yet"
		return s
	}
	s.Status = "empty"
	for _, rec := range telemetry.ReadRecords(path) {
		obj, ok := rec.(map[string]any)
		if !ok {
			continue
		}
		if ev, _ := obj["event"]; ev != "dispatch" {
			continue
		}
		if ex, _ := obj["executor"]; ex != executor {
			continue
		}
		tsV := obj["ts"]
		ts, ok := ParseTimestamp(tsV)
		if !ok || ts.Before(cutoff) {
			continue
		}
		usage := config.ObjectAt(obj, "usage")
		if usage == nil {
			continue
		}
		s.Tokens.add(tokensFrom(usage), 1)
		s.Records++
	}
	if s.Records > 0 {
		s.Status = "ok"
	}
	return s
}

// scopedLedger is the pi source of a run/session-scoped report, read from
// the run ledger.
func scopedLedger(o ReportOptions) *Source {
	dir := ledger.Dir(o.Cfg)
	selected := ledger.SelectedPaths(dir, o.RunIDs, o.SessionIDs)
	s := &Source{Status: "ok", Scope: "session", HasCostMicroUSD: true}
	if len(o.RunIDs) > 0 {
		s.Scope = "run"
	}
	counts := map[string]int64{}
	var order []string
	for _, path := range selected {
		var n int64
		for _, rec := range ledger.ReadFile(path) {
			if ex, _ := rec["executor"]; ex != "pi" {
				continue
			}
			n++
			s.Records++
			s.Tokens.add(tokensFrom(rec), 1)
			cost := rec["cost_micro_usd"]
			s.CostMicroUSD += coerceCount(cost)
		}
		if n == 0 {
			continue
		}
		_, run, _ := ledger.FileInfo(path)
		if run == "no.run" {
			run = "no-run"
		}
		if _, seen := counts[run]; !seen {
			order = append(order, run)
		}
		counts[run] = n
	}
	if s.Records == 0 {
		return notAttributable("no matching run ledger data")
	}
	if len(o.SessionIDs) > 0 && len(order) > 0 {
		ordinals := ledger.SessionOrdinalMap(dir, "")
		rank := func(run string) int {
			if n, ok := ordinals[run]; ok {
				return n
			}
			return 1_000_000_000
		}
		sort.SliceStable(order, func(i, j int) bool {
			a, b := order[i], order[j]
			if (a == "no-run") != (b == "no-run") {
				return b == "no-run"
			}
			if rank(a) != rank(b) {
				return rank(a) < rank(b)
			}
			return a < b
		})
		for _, run := range order {
			s.Runs = append(s.Runs, RunCount{Run: run, Count: counts[run]})
		}
	}
	return s
}
