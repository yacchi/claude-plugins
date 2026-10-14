package ui

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/cooldown"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/dispatch"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/ledger"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/usage"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/state"
)

const (
	// EventLimit is how many events of a wave the snapshot carries.
	EventLimit = 80
	// DispatchLimit is how many recent dispatches the snapshot carries.
	DispatchLimit = 50

	liveTTL  = 5 * time.Second
	usageTTL = 60 * time.Second
)

type liveEntry struct {
	at    time.Time
	count any
}

var (
	liveMu    sync.Mutex
	liveCache = map[string]liveEntry{}

	usageMu    sync.Mutex
	usageAt    time.Time
	usageValue any
)

// ResetCaches forgets the live-change and usage caches (tests).
func ResetCaches() {
	liveMu.Lock()
	liveCache = map[string]liveEntry{}
	liveMu.Unlock()
	usageMu.Lock()
	usageAt, usageValue = time.Time{}, nil
	usageMu.Unlock()
}

// plain turns any result type (ordered objects, tagged structs) into generic
// JSON values: maps (sorted on output), slices and json.Number scalars.
func plain(v any) any {
	b, err := fsutil.MarshalJSON(v)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return map[string]any{"error": err.Error()}
	}
	return out
}

func errorBody(err error) map[string]any {
	msg := err.Error()
	if msg == "" {
		msg = "error"
	}
	return map[string]any{"error": msg}
}

// Body renders a snapshot as JSON with sorted keys and no HTML escaping.
func Body(snapshot any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(snapshot); err != nil {
		return `{"error": "unencodable snapshot"}`
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// snapshotHash is the hash of the snapshot minus its timestamp, so an idle
// system sends nothing.
func snapshotHash(snapshot map[string]any) string {
	stable := make(map[string]any, len(snapshot))
	for k, v := range snapshot {
		if k != "generated_at" {
			stable[k] = v
		}
	}
	sum := sha256.Sum256([]byte(Body(stable)))
	return hex.EncodeToString(sum[:])
}

// registry is the registered waves, oldest registration first.
func registry() []state.Registration {
	list := state.ListWaves()
	out := make([]state.Registration, 0, len(list))
	for i := len(list) - 1; i >= 0; i-- {
		out = append(out, list[i])
	}
	return out
}

func loadJSONObject(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	v, err := fsutil.DecodeJSON(raw)
	if err != nil {
		return nil, err
	}
	if o, ok := v.(map[string]any); ok {
		return o, nil
	}
	return map[string]any{}, nil
}

func get(o map[string]any, k string) any {
	if o == nil {
		return nil
	}
	v := o[k]
	return v
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func parseEvent(rec map[string]any) any {
	detail, ok := get(rec, "detail").(string)
	if !ok {
		return plain(rec)
	}
	parsed, err := fsutil.DecodeJSON([]byte(detail))
	if err != nil {
		return plain(rec)
	}
	switch parsed.(type) {
	case map[string]any, []any:
		out := maps.Clone(rec)
		out["detail"] = parsed
		return plain(out)
	}
	return plain(rec)
}

func liveChanges(pid, status, tree string) any {
	key, err := filepath.Abs(tree)
	if err != nil {
		key = tree
	}
	liveMu.Lock()
	cached, ok := liveCache[key]
	liveMu.Unlock()
	if ok && time.Since(cached.at) < liveTTL {
		return cached.count
	}
	st := &state.State{Packages: map[string]*state.Package{pid: {Status: status, Tree: &tree}}}
	count := state.LiveChanges(st)[pid]
	liveMu.Lock()
	liveCache[key] = liveEntry{at: time.Now(), count: count}
	liveMu.Unlock()
	return count
}

// absJoin is p made absolute, relative to dir when it is relative.
func absJoin(dir, p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

func isFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

// statusString is a package status as a counts key; a null status is counted
// under "None" (part of the snapshot's JSON contract).
func statusString(v any) string {
	if v == nil {
		return "None"
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := fsutil.MarshalJSON(v)
	return string(b)
}

func waveSnapshot(entry state.Registration) map[string]any {
	statePath := entry.State
	out := map[string]any{
		"state": statePath, "repo": nilIfEmpty(entry.Repo), "into": nilIfEmpty(entry.Into),
		"wave": nil, "counts": map[string]any{}, "stopped": nil,
		"packages": []any{}, "needs": []any{}, "events": []any{},
	}
	st, err := loadJSONObject(statePath)
	if err != nil {
		out["error"] = err.Error()
	} else {
		out["wave"] = plain(get(st, "wave"))
		out["stopped"] = plain(get(st, "stopped"))
		if needs, ok := get(st, "needs").([]any); ok {
			out["needs"] = plain(needs)
		}
		counts := map[string]any{}
		packages := []any{}
		rawPackages, _ := get(st, "packages").(map[string]any)
		var order []string
		if rawPackages != nil {
			order = slices.Sorted(maps.Keys(rawPackages))
		}
		plan, _ := get(st, "plan").(string)
		planDir := ""
		if plan != "" {
			planDir = filepath.Dir(absJoin("", plan))
		}
		specs := planSpecs(plan)
		stateDir := filepath.Dir(statePath)
		for _, pid := range order {
			pkg, _ := get(rawPackages, pid).(map[string]any)
			status := get(pkg, "status")
			key := statusString(status)
			n, _ := counts[key].(int)
			counts[key] = n + 1
			row := map[string]any{
				"id": pid, "status": plain(status), "since": plain(get(pkg, "since")),
				"executor": plain(get(pkg, "executor")), "files_changed": plain(get(pkg, "files_changed")),
				"attempts": plain(get(pkg, "attempts")), "detail": plain(get(pkg, "detail")),
				"files_live": nil, "spec": nil, "context": nil, "correction": nil,
			}
			if tree, ok := get(pkg, "tree").(string); ok {
				s, _ := status.(string)
				row["files_live"] = liveChanges(pid, s, tree)
			}
			if spec, ok := specs[pid]; ok && planDir != "" {
				row["spec"] = absJoin(planDir, spec)
			}
			if p := filepath.Join(stateDir, "context", pid+".md"); isFile(p) {
				row["context"] = p
			}
			if p := filepath.Join(stateDir, "corrections", pid+".md"); isFile(p) {
				row["correction"] = p
			}
			packages = append(packages, row)
		}
		out["counts"] = counts
		out["packages"] = packages
	}
	events := state.ReadEvents(statePath, EventLimit)
	list := make([]any, 0, len(events))
	for _, rec := range events {
		list = append(list, parseEvent(rec))
	}
	out["events"] = list
	return out
}

// planSpecs maps package id -> spec path (relative to the plan) for a plan
// file that is JSON; an unreadable plan has none.
func planSpecs(plan string) map[string]string {
	out := map[string]string{}
	if plan == "" {
		return out
	}
	data, err := loadJSONObject(plan)
	if err != nil {
		return out
	}
	items, _ := get(data, "packages").([]any)
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id, ok := get(obj, "id").(string)
		if !ok {
			continue
		}
		spec := get(obj, "spec")
		if s, _ := spec.(string); s == "" {
			spec = get(obj, "path")
		}
		if s, ok := spec.(string); ok {
			out[id] = s
		}
	}
	return out
}

func worktreesSnapshot(entries []state.Registration) []any {
	var seen []string
	for _, e := range entries {
		if e.Repo == "" {
			continue
		}
		dup := false
		for _, s := range seen {
			dup = dup || s == e.Repo
		}
		if !dup {
			seen = append(seen, e.Repo)
		}
	}
	out := []any{}
	for _, repo := range seen {
		items, err := isolate.List(repo, "")
		if err != nil {
			out = append(out, map[string]any{"repo": repo, "items": []any{}, "error": err.Error()})
			continue
		}
		if items == nil {
			items = []isolate.Entry{}
		}
		out = append(out, map[string]any{"repo": repo, "items": plain(items)})
	}
	return out
}

var (
	dispatchKeys  = map[string]bool{"executor": true, "model": true, "cls": true, "class": true, "status": true, "corr": true}
	dispatchHints = []string{"time", "duration", "seconds", "_at", "ts", "date"}
)

func keepDispatchKey(key string) bool {
	if dispatchKeys[key] {
		return true
	}
	lowered := strings.ToLower(key)
	for _, hint := range dispatchHints {
		if strings.Contains(lowered, hint) {
			return true
		}
	}
	return false
}

func dispatchSnapshot(cfg *config.Resolved) any {
	var records []map[string]any
	for _, path := range ledger.Paths(ledger.Dir(cfg)) {
		records = append(records, ledger.ReadFile(path)...)
	}
	if len(records) > DispatchLimit {
		records = records[len(records)-DispatchLimit:]
	}
	out := make([]any, 0, len(records))
	for i := len(records) - 1; i >= 0; i-- {
		kept := map[string]any{}
		for _, k := range slices.Sorted(maps.Keys(records[i])) {
			if keepDispatchKey(k) {
				kept[k] = get(records[i], k)
			}
		}
		out = append(out, plain(kept))
	}
	return out
}

// cooldownSnapshot is the cooldown state file as stored (expired entries
// included).
func cooldownSnapshot(cfg *config.Resolved) any {
	st, err := loadJSONObject(cooldown.StatePath(cfg))
	if err != nil {
		return map[string]any{}
	}
	return plain(st)
}

func startedOf(row map[string]any) float64 {
	switch x := row["started"].(type) {
	case json.Number:
		f, _ := x.Float64()
		return f
	}
	return 0
}

func runningSnapshot(cfg *config.Resolved, entries []state.Registration) map[string]any {
	rows := []any{}
	if cfg != nil {
		for _, item := range dispatch.ListDetached(cfg) {
			row, _ := plain(item).(map[string]any)
			if row == nil {
				continue
			}
			switch {
			case item.Alive:
				row["state"] = "running"
			case !item.ResultPresent:
				row["state"] = "dead-no-result"
			case !item.Collected:
				row["state"] = "done-uncollected"
			default:
				continue
			}
			if n, _ := row["wait_calls"].(json.Number); n == "" {
				row["wait_calls"] = 0
			}
			rows = append(rows, row)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return startedOf(rows[i].(map[string]any)) > startedOf(rows[j].(map[string]any))
	})
	sessions := []any{}
	inFlight := 0
	byExecutor := map[string]any{}
	for _, entry := range entries {
		st, err := loadJSONObject(entry.State)
		if err != nil {
			continue
		}
		if pkgs, ok := get(st, "packages").(map[string]any); ok {
			for _, id := range slices.Sorted(maps.Keys(pkgs)) {
				pkg, _ := get(pkgs, id).(map[string]any)
				switch get(pkg, "status") {
				case "implementing", "verifying", "fixing", "ready":
					inFlight++
				}
			}
		}
		raw, err := os.ReadFile(filepath.Join(filepath.Dir(entry.State), "orca-sessions.json"))
		if err != nil {
			continue
		}
		doc, err := fsutil.DecodeJSON(raw)
		if err != nil {
			continue
		}
		var records []any
		switch x := doc.(type) {
		case map[string]any:
			for _, k := range slices.Sorted(maps.Keys(x)) {
				records = append(records, get(x, k))
			}
		case []any:
			records = x
		}
		for _, rec := range records {
			obj, ok := rec.(map[string]any)
			if !ok {
				continue
			}
			ws := get(obj, "wave_state")
			if s, _ := ws.(string); s == "" {
				ws = entry.State
			}
			sessions = append(sessions, map[string]any{
				"wave_state": ws, "pkg": plain(get(obj, "pkg")),
				"terminal": plain(get(obj, "terminal")), "worktree": plain(get(obj, "worktree")),
			})
		}
	}
	for _, r := range rows {
		row := r.(map[string]any)
		if name, ok := row["executor"].(string); ok && name != "" && row["state"] == "running" {
			n, _ := byExecutor[name].(int)
			byExecutor[name] = n + 1
		}
	}
	return map[string]any{"dispatches": rows, "orca_sessions": sessions,
		"by_executor": byExecutor, "waves_in_flight": inFlight}
}

func usageSnapshot(cfg *config.Resolved, cfgErr error) any {
	usageMu.Lock()
	defer usageMu.Unlock()
	if usageValue != nil && time.Since(usageAt) < usageTTL {
		return usageValue
	}
	if cfgErr != nil {
		return errorBody(cfgErr)
	}
	value := plain(usage.Snapshot24h(cfg))
	if m, ok := value.(map[string]any); ok {
		if _, failed := m["error"]; failed {
			return value
		}
	}
	usageAt, usageValue = time.Now(), value
	return value
}

// BuildSnapshot is the dashboard's data: every registered wave, the
// worktrees of their repos, recent dispatches, cooldowns, detached runs and
// 24-hour usage, from files already on disk. A failing source becomes
// {"error": ...} in its own slot, never a failure of the whole.
func BuildSnapshot(cfg *config.Resolved) any {
	return buildSnapshot(cfg, nil)
}

func buildSnapshot(cfg *config.Resolved, cfgErr error) map[string]any {
	entries := registry()
	waves := make([]any, 0, len(entries))
	for _, e := range entries {
		waves = append(waves, waveSnapshot(e))
	}
	slot := func(fn func(*config.Resolved) any) any {
		if cfgErr != nil {
			return errorBody(cfgErr)
		}
		return fn(cfg)
	}
	return map[string]any{
		"generated_at": float64(time.Now().UnixMicro()) / 1e6,
		"waves":        waves,
		"worktrees":    worktreesSnapshot(entries),
		"dispatch":     slot(dispatchSnapshot),
		"cooldown":     slot(cooldownSnapshot),
		"running":      runningSnapshot(cfg, entries),
		"usage":        usageSnapshot(cfg, cfgErr),
	}
}
