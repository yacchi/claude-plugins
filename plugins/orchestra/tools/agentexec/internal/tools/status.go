package tools

import (
	"sort"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
)

// Item is one `tools status` or `tools install` row, in output key order.
type Item = *jsonx.Object

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// KnownNames lists the tool names status and install accept: the configured
// tools, then every executor agent-exec drives that is not configured.
func KnownNames(cfg *config.Resolved) []string {
	names := Names(cfg)
	seen := map[string]bool{}
	for _, n := range names {
		seen[n] = true
	}
	for _, n := range knownOrder() {
		if !seen[n] {
			names = append(names, n)
		}
	}
	return names
}

func knownOrder() []string {
	out := make([]string, 0, len(KnownBinaries))
	for n := range KnownBinaries {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Status resolves every configured tool plus every known executor.
func Status(cfg *config.Resolved) []Item {
	items := []Item{}
	for _, name := range KnownNames(cfg) {
		r := Resolve(name, cfg)
		items = append(items, jsonx.NewObject().
			Set("name", r.Name).
			Set("path", nullable(r.Path)).
			Set("source", nullable(r.Source)).
			Set("version", nullable(r.Version)).
			Set("wanted", nullable(r.Wanted)).
			Set("reason", nullable(r.Reason)).
			Set("hint", nullable(r.Hint)))
	}
	return items
}

// StatusLine is one row of `tools status` text output.
func StatusLine(it Item) string {
	cells := []string{strOr(it, "name", ""), strOr(it, "source", "-"), strOr(it, "version", "-"), strOr(it, "path", "-")}
	if s := strOr(it, "reason", ""); s != "" {
		cells = append(cells, s)
	}
	if s := strOr(it, "hint", ""); s != "" {
		cells = append(cells, s)
	}
	return strings.Join(cells, "  ")
}

// InstallLine is one row of `tools install` text output.
func InstallLine(it Item) string {
	line := strings.Join([]string{strOr(it, "name", ""), strOr(it, "status", ""), strOr(it, "version", "-"), strOr(it, "path", "-")}, "  ")
	if e := strOr(it, "error", ""); e != "" {
		line += "  " + e
	}
	return line
}

// Failed reports whether any install row failed.
func Failed(items []Item) bool {
	for _, it := range items {
		if s, _ := it.Get("status"); s == "failed" {
			return true
		}
	}
	return false
}

func strOr(it Item, key, def string) string {
	if s, ok := it.Get(key); ok {
		if str, isStr := s.(string); isStr && str != "" {
			return str
		}
	}
	return def
}
