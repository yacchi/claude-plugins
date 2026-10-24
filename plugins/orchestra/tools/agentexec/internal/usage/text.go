package usage

import (
	"fmt"
	"sort"
	"strings"
)

func tokensLine(t Tokens) string {
	return fmt.Sprintf("in=%d out=%d cached=%d", t.InputTokens, t.OutputTokens, t.CachedInputTokens)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// FormatText is the compact human summary. It deliberately carries model
// names and counts only: no paths, session ids, project slugs or prompt text.
func FormatText(r Report) string {
	var b strings.Builder
	line := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }

	switch r.Scope.Kind {
	case "window":
		line("scope: window since=%s until=%s", r.Scope.Since, r.Scope.Until)
	case "run":
		line("scope: run %s", strings.Join(r.Scope.RunIDs, ","))
	case "session":
		line("scope: session %s", strings.Join(r.Scope.SessionIDs, ","))
	default:
		line("scope: run+session runs=%s sessions=%s",
			strings.Join(r.Scope.RunIDs, ","), strings.Join(r.Scope.SessionIDs, ","))
	}
	for _, name := range r.Sources.Names {
		e := r.Sources.Entries[name]
		if e.NotAttributable {
			line("%s: not attributable (%s)", name, e.Reason)
			continue
		}
		line("%-8s %-13s %s", name, "["+e.Status+"]", tokensLine(e.Tokens))
		if e.Note != "" {
			line("           note: %s", e.Note)
		}
		if e.Status != "ok" {
			continue
		}
		switch name {
		case "claude":
			line("           main: %s", tokensLine(e.Main))
			line("           sidechain: %s", tokensLine(e.Sidechain))
			var priced map[string]ModelCost
			if e.Cost != nil {
				priced = e.Cost.Models
			}
			for _, model := range sortedKeys(e.ByModel) {
				line("           %s: %s", model, tokensLine(e.ByModel[model]))
				if d, ok := priced[model]; ok {
					line("             fresh=%d cache_write(5m)=%d cache_write(1h)=%d cache_read=%d out=%d  ~$%.4f",
						d.FreshInput, d.CacheWrite5m, d.CacheWrite1h, d.CacheRead, d.Output,
						float64(d.MicroUSD)/1000000.0)
				}
			}
			if e.Cost != nil {
				for _, model := range sortedKeys(e.Cost.UnpricedModels) {
					line("             %s: no price (set pricing.models in orchestra.yaml)", model)
				}
			}
			line("           sessions: %d", e.Sessions)
		case "pi":
			line("           cost: $%.6f", float64(e.CostMicroUSD)/1000000.0)
		}
		if (r.Scope.Kind == "session" || r.Scope.Kind == "run+session") && len(e.Runs) > 0 {
			runs := append([]RunCount(nil), e.Runs...)
			sort.Slice(runs, func(i, j int) bool { return runs[i].Run < runs[j].Run })
			parts := make([]string, len(runs))
			for i, rc := range runs {
				parts[i] = fmt.Sprintf("%s=%d", rc.Run, rc.Count)
			}
			line("           runs: %s", strings.Join(parts, " "))
		}
	}
	line("total    %-13s %s", "", tokensLine(r.Totals))
	var parts []string
	for _, name := range sortedKeys(r.Cost.Components) {
		parts = append(parts, fmt.Sprintf("%s=$%.4f", name, float64(r.Cost.Components[name])/1000000.0))
	}
	summary := strings.Join(parts, ", ")
	if summary == "" {
		summary = "nothing priced"
	}
	partial := ""
	if len(r.Cost.UnpricedSources) > 0 {
		partial = "  PARTIAL: unpriced " + strings.Join(r.Cost.UnpricedSources, ", ")
	}
	line("cost     ~$%.4f  (%s)%s", float64(r.Cost.EstimatedMicroUSD)/1000000.0, summary, partial)
	line("         estimate from list prices as of %s; claude = list price, pi = provider-reported",
		r.Cost.PriceList.AsOf)
	return b.String()
}

// FormatRunsText is `usage --list-runs --text`: one line per run.
func FormatRunsText(runs []Run) string {
	var b strings.Builder
	for _, r := range runs {
		label := "-"
		if r.Ordinal != nil {
			label = fmt.Sprintf("%03d", *r.Ordinal)
		}
		fmt.Fprintf(&b, "%s %s %d %s %s\n", label, r.RunID, r.Files, r.FirstTS, r.LastTS)
	}
	return b.String()
}
