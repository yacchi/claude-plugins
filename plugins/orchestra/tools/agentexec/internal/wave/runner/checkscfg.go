package runner

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/checks"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/ledger"
)

// checksCfg is the `checks` section as the runner uses it.
type checksCfg struct {
	raw              []any // checks.items as written (with their `fast` flag)
	ensure           []checks.Ensure
	retryFailedAlone bool
	slotDir          string
	maxParallel      int
	flakyThreshold   int
}

// checksConfig reads the current `checks` section; the check slots are the
// machine-wide ones `agent-exec check` uses (beside the ledger dir), or
// the state dir's when the config cannot be read.
func (r *runner) checksConfig() checksCfg {
	c := checksCfg{
		retryFailedAlone: true, slotDir: filepath.Join(r.stateDir, "check-slots"),
		maxParallel: 2, flakyThreshold: 3,
	}
	cfg := r.config()
	if cfg == nil {
		return c
	}
	sec := cfg.Section("checks")
	c.raw = asList(objGet(sec, "items"))
	if loaded, err := checks.LoadConfig(cfg); err == nil {
		c.ensure, c.maxParallel, c.retryFailedAlone = loaded.Ensure, loaded.MaxParallel, loaded.RetryFailedAlone
	}
	c.slotDir = filepath.Join(filepath.Dir(absPath(ledger.Dir(cfg))), "check-slots")
	switch v := objGet(sec, "flaky_threshold").(type) {
	case int64:
		c.flakyThreshold = int(v)
	case float64:
		c.flakyThreshold = int(v)
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			c.flakyThreshold = n
		}
	}
	return c
}

// itemsWhere is the check items marked `fast: true` (fast) or the rest.
func (c checksCfg) itemsWhere(fast bool) []checks.Item {
	var out []checks.Item
	for _, raw := range c.raw {
		obj, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		isFast, _ := objGet(obj, "fast").(bool)
		if isFast != fast {
			continue
		}
		b, err := fsutil.MarshalJSON(obj)
		if err != nil {
			continue
		}
		var it checks.Item
		if json.Unmarshal(b, &it) == nil {
			out = append(out, it)
		}
	}
	return out
}

// ensureFailure runs `checks.ensure` in tree; the first failing entry, or
// nil.
func (r *runner) ensureFailure(tree string) *ensureFail {
	cfg := r.config()
	if cfg == nil {
		return nil
	}
	c := r.checksConfig()
	if len(c.ensure) == 0 {
		return nil
	}
	results := checks.Run(tree, nil, checks.Options{Ensure: c.ensure, SlotDir: c.slotDir, MaxParallel: c.maxParallel}).Checks
	for _, res := range results {
		if res.Status == "fail" {
			return &ensureFail{result: res, excerpt: checks.FailureExcerpt(res.Name + "\n" + res.Excerpt)}
		}
	}
	return nil
}

type ensureFail struct {
	result  checks.CheckResult
	excerpt string
}
