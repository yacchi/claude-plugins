package usage

import (
	"math"
	"sort"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
)

// WHAT IS AND IS NOT PRICED. Only models with a known per-token price get a
// dollar figure. Anything else (a model missing from the table and from the
// user's pricing.models override) is reported as UNPRICED with its token
// counts, never guessed. A total built from the priced part alone is
// labelled as such by the caller.
//
// THE PRICE LIST. USD per 1M tokens, Anthropic first-party API list prices as
// of PriceListAsOf. Cache writes are derived, not tabulated: 1.25x the input
// price for the 5-minute TTL and 2x for the 1-hour TTL. Cache reads are
// tabulated per model. Not modelled: batch discounts, fast mode premiums and
// the data-residency multiplier: an estimate, not an invoice.
//
// Override or extend it from orchestra.yaml (USD per 1M tokens; cache_write_5m
// and cache_write_1h default to 1.25x / 2x of input):
//
//	pricing:
//	  models:
//	    claude-opus-5-5: {input: 4, output: 20, cache_read: 0.2}
const (
	PriceListAsOf   = "2026-09-25"
	PriceListSource = "Anthropic first-party API list prices (claude-api skill, cached " +
		PriceListAsOf + "); cache write = 1.25x (5m) / 2x (1h) of input"

	cacheWrite5mMultiplier = 1.25
	cacheWrite1hMultiplier = 2.0
)

// Price is USD per 1M tokens (equivalently micro-USD per token).
type Price struct {
	Input, Output, CacheRead, CacheWrite5m, CacheWrite1h float64
}

// Detail is the five-way token breakdown a price estimate consumes.
type Detail struct {
	FreshInput   int64 `json:"fresh_input"`
	CacheWrite5m int64 `json:"cache_write_5m"`
	CacheWrite1h int64 `json:"cache_write_1h"`
	CacheRead    int64 `json:"cache_read"`
	Output       int64 `json:"output"`
}

func (d Detail) any() bool {
	return d.FreshInput != 0 || d.CacheWrite5m != 0 || d.CacheWrite1h != 0 || d.CacheRead != 0 || d.Output != 0
}

func (d Detail) total() int64 {
	return d.FreshInput + d.CacheWrite5m + d.CacheWrite1h + d.CacheRead + d.Output
}

// listRow is (model id prefix, input, output, cache_read). Matched
// longest-prefix-first, so "claude-fable-5-1" wins over "claude-fable-5" and
// a dated id such as "claude-haiku-4-5-20251001" still resolves.
type listRow struct {
	prefix                   string
	input, output, cacheRead float64
}

var list = []listRow{
	{"claude-fable-5-1", 10.0, 50.0, 0.25},
	{"claude-mythos-5-1", 10.0, 50.0, 0.25},
	{"claude-fable-5", 10.0, 50.0, 1.0},
	{"claude-mythos-5", 10.0, 50.0, 1.0},
	{"claude-opus-5-5", 4.0, 20.0, 0.2},
	{"claude-opus-5", 5.0, 25.0, 0.5},
	{"claude-opus-4-8", 5.0, 25.0, 0.5},
	{"claude-opus-4-7", 5.0, 25.0, 0.5},
	{"claude-opus-4-6", 5.0, 25.0, 0.5},
	{"claude-sonnet-5-5", 2.0, 10.0, 0.2},
	{"claude-sonnet-5", 2.0, 10.0, 0.2},
	{"claude-sonnet-4-6", 3.0, 15.0, 0.3},
	{"claude-haiku-4-5", 1.0, 5.0, 0.1},
}

// number is a non-negative, non-bool JSON number as a float.
func number(v any) (float64, bool) {
	var f float64
	switch x := v.(type) {
	case int64:
		f = float64(x)
	case int:
		f = float64(x)
	case float64:
		f = x
	default:
		return 0, false
	}
	if f < 0 {
		return 0, false
	}
	return f, true
}

func derived(input, output, cacheRead float64, w5, w1 *float64) Price {
	p := Price{Input: input, Output: output, CacheRead: cacheRead,
		CacheWrite5m: input * cacheWrite5mMultiplier, CacheWrite1h: input * cacheWrite1hMultiplier}
	if w5 != nil {
		p.CacheWrite5m = *w5
	}
	if w1 != nil {
		p.CacheWrite1h = *w1
	}
	return p
}

func optNumber(v any) *float64 {
	if f, ok := number(v); ok {
		return &f
	}
	return nil
}

// PriceFor is the price of a model id, or false when it is not priced. The
// user's pricing.models entries win over the built-in list; a malformed entry
// is ignored rather than failing (the report must still render).
func PriceFor(model string, overrides *jsonx.Object) (Price, bool) {
	if model == "" {
		return Price{}, false
	}
	if configured := overrides.Object("models"); configured != nil {
		keys := configured.Keys()
		sort.SliceStable(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
		for _, key := range keys {
			specV, _ := configured.Get(key)
			spec, ok := specV.(*jsonx.Object)
			if !ok || !strings.HasPrefix(model, key) {
				continue
			}
			inV, _ := spec.Get("input")
			outV, _ := spec.Get("output")
			input, ok1 := number(inV)
			output, ok2 := number(outV)
			if !ok1 || !ok2 {
				continue
			}
			crV, _ := spec.Get("cache_read")
			cacheRead, ok := number(crV)
			if !ok {
				cacheRead = input * 0.1
			}
			w5, _ := spec.Get("cache_write_5m")
			w1, _ := spec.Get("cache_write_1h")
			return derived(input, output, cacheRead, optNumber(w5), optNumber(w1)), true
		}
	}
	rows := append([]listRow(nil), list...)
	sort.SliceStable(rows, func(i, j int) bool { return len(rows[i].prefix) > len(rows[j].prefix) })
	for _, r := range rows {
		if strings.HasPrefix(model, r.prefix) {
			return derived(r.input, r.output, r.cacheRead, nil, nil), true
		}
	}
	return Price{}, false
}

// CostMicroUSD is one model's token breakdown at a price, in integer
// micro-USD (a price in USD per 1M tokens is exactly micro-USD per token).
// Rounded half to even, as Python's round().
func CostMicroUSD(d Detail, p Price) int64 {
	total := 0.0
	for _, c := range []struct {
		count int64
		price float64
	}{
		{d.FreshInput, p.Input}, {d.CacheWrite5m, p.CacheWrite5m},
		{d.CacheWrite1h, p.CacheWrite1h}, {d.CacheRead, p.CacheRead}, {d.Output, p.Output},
	} {
		if c.count > 0 {
			total += float64(c.count) * c.price
		}
	}
	return int64(math.RoundToEven(total))
}

// ModelCost is one priced model: its breakdown plus its own estimate.
type ModelCost struct {
	Detail
	MicroUSD int64 `json:"micro_usd"`
}

// PriceList carries the provenance of the prices used.
type PriceList struct {
	AsOf   string `json:"as_of"`
	Source string `json:"source"`
}

// Cost is an estimate over several models.
type Cost struct {
	MicroUSD       int64                `json:"micro_usd"`
	Models         map[string]ModelCost `json:"models"`
	UnpricedModels map[string]int64     `json:"unpriced_models"`
	PriceList      PriceList            `json:"price_list"`
}

// Estimate prices {model: breakdown}. Models with nothing in them are
// skipped; a model with no known price is listed in UnpricedModels with its
// token total, so the report can say exactly how much the dollar figure
// leaves out.
func Estimate(byModel map[string]Detail, overrides *jsonx.Object) Cost {
	c := Cost{
		Models:         map[string]ModelCost{},
		UnpricedModels: map[string]int64{},
		PriceList:      PriceList{AsOf: PriceListAsOf, Source: PriceListSource},
	}
	for model, d := range byModel {
		if !d.any() {
			continue
		}
		price, ok := PriceFor(model, overrides)
		if !ok {
			c.UnpricedModels[model] = d.total()
			continue
		}
		mc := ModelCost{Detail: d, MicroUSD: CostMicroUSD(d, price)}
		c.Models[model] = mc
		c.MicroUSD += mc.MicroUSD
	}
	return c
}
