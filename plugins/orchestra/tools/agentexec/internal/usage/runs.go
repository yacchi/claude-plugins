package usage

import (
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/ledger"
)

// Run is one recorded run: a Claude workflow directory ("wf_...") and/or a
// pi ledger file. Ordinal is the execution ordinal in the current session.
type Run struct {
	RunID   string `json:"run_id"`
	Files   int    `json:"files"`
	Ordinal *int   `json:"ordinal"`
	FirstTS string `json:"first_ts"`
	LastTS  string `json:"last_ts"`
}

// RunsOptions selects the runs ListRuns looks at.
type RunsOptions struct {
	Sources     []string
	Cfg         *config.Resolved
	AllProjects bool
	Home, Cwd   string
	ConfigDir   string
}

type runItem struct {
	files   int
	ordinal *int
	stamps  []time.Time
}

// ListRuns lists the recorded runs oldest-last-activity first.
func ListRuns(o RunsOptions) []Run {
	requested := o.Sources
	if len(requested) == 0 {
		requested = Sources
	}
	items := map[string]*runItem{}
	var order []string
	item := func(id string) *runItem {
		it := items[id]
		if it == nil {
			it = &runItem{}
			items[id] = it
			order = append(order, id)
		}
		return it
	}

	if contains(requested, "claude") {
		for _, root := range projectsRoots(o.Home, o.ConfigDir) {
			for _, project := range projectDirs(root, o.AllProjects, o.Cwd) {
				walkWorkflows(project, item)
			}
		}
	}
	if contains(requested, "pi") {
		dir := ledger.Dir(o.Cfg)
		ordinals := ledger.SessionOrdinalMap(dir, "")
		for _, path := range ledger.Paths(dir) {
			name := filepath.Base(path)
			if name == "no.run.jsonl" || !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			_, run, _ := ledger.FileInfo(path)
			records := ledger.ReadFile(path)
			if len(records) == 0 {
				continue
			}
			it := item(run)
			if n, ok := ordinals[run]; ok {
				n := n
				it.ordinal = &n
			}
			for _, rec := range records {
				tsV, _ := rec.Get("ts")
				if ts, ok := ParseTimestamp(tsV); ok {
					it.stamps = append(it.stamps, ts)
				}
			}
		}
	}

	out := []Run{}
	for _, id := range order {
		it := items[id]
		if len(it.stamps) == 0 {
			continue
		}
		first, last := it.stamps[0], it.stamps[0]
		for _, t := range it.stamps {
			if t.Before(first) {
				first = t
			}
			if t.After(last) {
				last = t
			}
		}
		out = append(out, Run{RunID: id, Files: it.files, Ordinal: it.ordinal,
			FirstTS: fsutil.FormatISO(first), LastTS: fsutil.FormatISO(last)})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastTS < out[j].LastTS })
	return out
}

// walkWorkflows visits every "wf_..." directory below project (symlinked
// directories are not entered) and records its transcripts' timestamps.
func walkWorkflows(project string, item func(string) *runItem) {
	var walk func(dir string)
	walk = func(dir string) {
		files, _, enter := walkEntries(dir)
		base := filepath.Base(dir)
		if strings.HasPrefix(base, "wf_") && base != "wf_" {
			it := item(base)
			for _, name := range files {
				if !strings.HasSuffix(name, ".jsonl") {
					continue
				}
				it.files++
				ledger.ForEachLine(filepath.Join(dir, name), func(line string) {
					v, err := jsonx.Decode([]byte(line))
					if err != nil {
						return
					}
					if obj, ok := v.(*jsonx.Object); ok {
						ts, _ := obj.Get("timestamp")
						if t, ok := ParseTimestamp(ts); ok {
							it.stamps = append(it.stamps, t)
						}
					}
				})
			}
		}
		for _, name := range enter {
			walk(filepath.Join(dir, name))
		}
	}
	walk(project)
}
