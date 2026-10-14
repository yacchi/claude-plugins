package usage

import (
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
)

func overrides(t *testing.T, text string) map[string]any {
	t.Helper()
	v, err := fsutil.DecodeJSON([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	o, _ := v.(map[string]any)
	return o
}

func TestPriceForLongestPrefixWins(t *testing.T) {
	for _, c := range []struct {
		model            string
		input, cacheRead float64
	}{
		{"claude-fable-5-1", 10, 0.25},
		{"claude-fable-5", 10, 1.0},
		{"claude-opus-5-5", 4, 0.2},
		{"claude-opus-5", 5, 0.5},
		{"claude-haiku-4-5-20251001", 1, 0.1},
	} {
		p, ok := PriceFor(c.model, nil)
		if !ok || p.Input != c.input || p.CacheRead != c.cacheRead {
			t.Errorf("%s -> %+v %v", c.model, p, ok)
		}
	}
	if p, _ := PriceFor("claude-haiku-4-5-20251001", nil); p.Output != 5 {
		t.Errorf("output = %v", p.Output)
	}
}

func TestCacheWritesDeriveFromInput(t *testing.T) {
	p, _ := PriceFor("claude-sonnet-5-5", nil)
	if p.CacheWrite5m != 2.5 || p.CacheWrite1h != 4.0 {
		t.Errorf("%+v", p)
	}
}

func TestUnknownAndGarbageModelsAreUnpriced(t *testing.T) {
	for _, m := range []string{"gpt-5.6-luna", "", "<synthetic>"} {
		if _, ok := PriceFor(m, nil); ok {
			t.Errorf("%q priced", m)
		}
	}
}

func TestOverrideWinsAndDefaultsTheDerivedFields(t *testing.T) {
	p, ok := PriceFor("claude-opus-5-5", overrides(t, `{"models": {"claude-opus-5-5": {"input": 1, "output": 2}}}`))
	if !ok || p.Input != 1 || p.Output != 2 || p.CacheRead != 0.1 || p.CacheWrite5m != 1.25 || p.CacheWrite1h != 2 {
		t.Errorf("%+v", p)
	}
	p, _ = PriceFor("claude-opus-5-5-x", overrides(t, `{"models": {"claude-opus": {"input": 1, "output": 2},
		"claude-opus-5-5": {"input": 3, "output": 4, "cache_read": 0.5, "cache_write_5m": 7, "cache_write_1h": 8.5}}}`))
	if p.Input != 3 || p.CacheRead != 0.5 || p.CacheWrite5m != 7 || p.CacheWrite1h != 8.5 {
		t.Errorf("longest override prefix: %+v", p)
	}
}

func TestMalformedOverrideIsIgnoredNotFatal(t *testing.T) {
	for _, bad := range []string{
		`{"models": {"claude-opus-5-5": {"input": "four"}}}`,
		`{"models": {"claude-opus-5-5": {"input": true, "output": 2}}}`,
		`{"models": {"claude-opus-5-5": {"input": -1, "output": 2}}}`,
		`{"models": {"claude-opus-5-5": 7}}`,
		`{"models": "nope"}`,
	} {
		p, ok := PriceFor("claude-opus-5-5", overrides(t, bad))
		if !ok || p.Input != 4 {
			t.Errorf("%s -> %+v %v", bad, p, ok)
		}
	}
}

func TestEstimateUsesEachComponentsOwnRate(t *testing.T) {
	d := Detail{1_000_000, 1_000_000, 1_000_000, 1_000_000, 1_000_000}
	est := Estimate(map[string]Detail{"claude-opus-5-5": d}, nil)
	if want := int64((4 + 5 + 8 + 0.2 + 20) * 1_000_000); est.MicroUSD != want {
		t.Errorf("micro_usd = %d, want %d", est.MicroUSD, want)
	}
	if est.Models["claude-opus-5-5"].MicroUSD != est.MicroUSD {
		t.Error("model estimate differs from the total")
	}
}

func TestEstimateSkipsEmptyAndReportsUnpriced(t *testing.T) {
	est := Estimate(map[string]Detail{"claude-opus-5-5": {}}, nil)
	if est.MicroUSD != 0 || len(est.Models) != 0 || len(est.UnpricedModels) != 0 {
		t.Errorf("%+v", est)
	}
	est = Estimate(map[string]Detail{"mystery": {FreshInput: 5, Output: 7}}, nil)
	if est.UnpricedModels["mystery"] != 12 || est.MicroUSD != 0 {
		t.Errorf("%+v", est)
	}
	if est.PriceList.AsOf != PriceListAsOf {
		t.Error("price list provenance missing")
	}
}

func TestCostRoundsToNearest(t *testing.T) {
	// 0.5, 1.5, 2.5 and 3.5 micro-USD round half away from zero.
	for _, c := range []struct{ out, want int64 }{{1, 1}, {3, 2}, {5, 3}, {7, 4}} {
		if got := CostMicroUSD(Detail{Output: c.out}, Price{Output: 0.5}); got != c.want {
			t.Errorf("%d tokens at 0.5 -> %d, want %d", c.out, got, c.want)
		}
	}
}
