package quota

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// Fetcher looks one group up live. It returns an Entry with Source "api"
// and, on failure, one of the fixed error labels.
type Fetcher func(ctx context.Context, cfg *config.Resolved, now time.Time) Entry

// fetchers are the live sources per group (replaced by tests).
var fetchers = map[string]Fetcher{
	GroupClaude: fetchClaude,
	GroupCodex:  fetchCodex,
}

// requestTimeout bounds one group's whole live lookup (credential read and
// request).
const requestTimeout = 5 * time.Second

func seconds(t time.Time) float64 { return float64(t.UnixMicro()) / 1e6 }

// CachePath is the machine-local cache of the last lookups.
func CachePath() string {
	return filepath.Join(paths.StateDir(), "usage-limits.json")
}

// Snapshot is the usage of the requested groups that have a source: the
// ORCHESTRA_USAGE_SNAPSHOT fixture when set, else the cache entries younger
// than usage_routing.cache_seconds and a concurrent live lookup of the rest
// (failures included, so a failing source costs one timeout per TTL). It is
// nil when usage routing is disabled. It never fails.
func Snapshot(cfg *config.Resolved, groups []string, now time.Time) map[string]Entry {
	if !Enabled(cfg) {
		return nil
	}
	wanted := []string{}
	for _, g := range groups {
		if _, ok := fetchers[g]; ok && !slices.Contains(wanted, g) {
			wanted = append(wanted, g)
		}
	}
	if path := os.Getenv(SnapshotEnv); path != "" {
		return fixture(path, wanted)
	}
	out := map[string]Entry{}
	if len(wanted) == 0 {
		return out
	}
	ttl := float64(CacheSeconds(cfg))
	nowS := seconds(now)
	cached := loadCache(CachePath())
	missing := []string{}
	for _, g := range wanted {
		if e, ok := cached[g]; ok && ttl > 0 && e.FetchedAt <= nowS && nowS-e.FetchedAt < ttl {
			e.Source = SourceCache
			out[g] = e
			continue
		}
		missing = append(missing, g)
	}
	if len(missing) == 0 {
		return out
	}
	fresh := fetchAll(cfg, missing, now)
	for g, e := range fresh {
		out[g] = e
	}
	if ttl > 0 {
		for g, e := range fresh {
			cached[g] = e
		}
		saveCache(CachePath(), cached, nowS, ttl)
	}
	return out
}

func fetchAll(cfg *config.Resolved, groups []string, now time.Time) map[string]Entry {
	out := map[string]Entry{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, g := range groups {
		wg.Add(1)
		go func(g string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
			defer cancel()
			e := fetchers[g](ctx, cfg, now)
			e.Source = SourceAPI
			e.FetchedAt = seconds(now)
			mu.Lock()
			out[g] = e
			mu.Unlock()
		}(g)
	}
	wg.Wait()
	return out
}

func readObject(path string) map[string]any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	v, err := fsutil.DecodeJSON(raw)
	if err != nil {
		return nil
	}
	o, _ := v.(map[string]any)
	return o
}

// fixture reads the snapshot file; a missing or garbled file is no data.
func fixture(path string, groups []string) map[string]Entry {
	out := map[string]Entry{}
	doc := readObject(path)
	for _, g := range groups {
		if e, ok := EntryFrom(doc[g]); ok {
			e.Source = SourceFixture
			out[g] = e
		}
	}
	return out
}

func loadCache(path string) map[string]Entry {
	out := map[string]Entry{}
	for g, v := range readObject(path) {
		if _, known := fetchers[g]; !known {
			continue
		}
		if e, ok := EntryFrom(v); ok {
			out[g] = e
		}
	}
	return out
}

// saveCache writes the entries still fresh at now (best effort, 0600,
// atomic): snapshot fields only.
func saveCache(path string, entries map[string]Entry, nowS, ttl float64) {
	doc := map[string]any{}
	for g, e := range entries {
		if e.FetchedAt <= nowS && nowS-e.FetchedAt < ttl {
			e.Source = SourceAPI
			doc[g] = e.Object()
		}
	}
	data, err := fsutil.MarshalJSON(doc)
	if err != nil {
		return
	}
	if fsutil.EnsureDir(filepath.Dir(path), 0o700) != nil {
		return
	}
	_ = fsutil.AtomicWriteFile(path, data, 0o600)
}
