"""Price list and cost estimation for `agent-exec usage`.

Pure functions only: model -> price, token breakdown -> micro-USD. No I/O.

WHAT IS AND IS NOT PRICED. Only models with a known per-token price get a
dollar figure. Anything else (a model missing from the table and from the
user's `pricing.models` override, Codex's OpenAI models) is reported as UNPRICED with its token counts -- never guessed. A
total built from the priced part alone is labelled as such by the caller.

THE PRICE LIST. USD per 1M tokens, Anthropic first-party API list prices as of
PRICE_LIST_AS_OF. Cache writes are derived, not tabulated: 1.25x the input
price for the 5-minute TTL and 2x for the 1-hour TTL. Cache reads are
tabulated per model because they are not a fixed fraction (0.025x on Fable
5.1, 0.05x on Opus 5.5, 0.1x elsewhere). Not modelled: batch discounts, fast
mode premiums, and the data-residency multiplier -- an estimate, not an invoice.

Override or extend it from orchestra.yaml (values are USD per 1M tokens;
cache_write_5m / cache_write_1h default to 1.25x / 2x of `input`):

    pricing:
      models:
        claude-opus-5-5: {input: 4, output: 20, cache_read: 0.2}
"""

PRICE_LIST_AS_OF = "2026-09-25"
PRICE_LIST_SOURCE = (
    "Anthropic first-party API list prices (claude-api skill, cached "
    + PRICE_LIST_AS_OF
    + "); cache write = 1.25x (5m) / 2x (1h) of input"
)

CACHE_WRITE_5M_MULTIPLIER = 1.25
CACHE_WRITE_1H_MULTIPLIER = 2.0

# (model id prefix, input, output, cache_read), USD per 1M tokens. Matched
# longest-prefix-first, so "claude-fable-5-1" wins over "claude-fable-5" and a
# dated id such as "claude-haiku-4-5-20251001" still resolves.
_LIST = (
    ("claude-fable-5-1", 10.0, 50.0, 0.25),
    ("claude-mythos-5-1", 10.0, 50.0, 0.25),
    ("claude-fable-5", 10.0, 50.0, 1.0),
    ("claude-mythos-5", 10.0, 50.0, 1.0),
    ("claude-opus-5-5", 4.0, 20.0, 0.2),
    ("claude-opus-5", 5.0, 25.0, 0.5),
    ("claude-opus-4-8", 5.0, 25.0, 0.5),
    ("claude-opus-4-7", 5.0, 25.0, 0.5),
    ("claude-opus-4-6", 5.0, 25.0, 0.5),
    ("claude-sonnet-5-5", 2.0, 10.0, 0.2),
    ("claude-sonnet-5", 2.0, 10.0, 0.2),
    ("claude-sonnet-4-6", 3.0, 15.0, 0.3),
    ("claude-haiku-4-5", 1.0, 5.0, 0.1),
)

# The per-model token breakdown the estimate consumes (see
# agent_exec.aggregate_claude_lines): every key is a non-negative int.
DETAIL_KEYS = ("fresh_input", "cache_write_5m", "cache_write_1h", "cache_read", "output")


def _number(value):
    if isinstance(value, bool) or not isinstance(value, (int, float)) or value < 0:
        return None
    return float(value)


def _price(input_, output, cache_read, write_5m=None, write_1h=None):
    return {
        "input": input_,
        "output": output,
        "cache_read": cache_read,
        "cache_write_5m": input_ * CACHE_WRITE_5M_MULTIPLIER if write_5m is None else write_5m,
        "cache_write_1h": input_ * CACHE_WRITE_1H_MULTIPLIER if write_1h is None else write_1h,
    }


def price_for(model, overrides=None):
    """Model id -> price dict, or None when the model is not priced. The user's
    `pricing.models` entries win over the built-in list; a malformed entry is
    ignored rather than raising (the report must still render)."""
    if not isinstance(model, str) or not model:
        return None
    configured = (overrides or {}).get("models") if isinstance(overrides, dict) else None
    if isinstance(configured, dict):
        for key in sorted(configured, key=len, reverse=True):
            spec = configured[key]
            if not isinstance(key, str) or not model.startswith(key) or not isinstance(spec, dict):
                continue
            input_, output = _number(spec.get("input")), _number(spec.get("output"))
            cache_read = _number(spec.get("cache_read"))
            if input_ is None or output is None:
                continue
            if cache_read is None:
                cache_read = input_ * 0.1
            return _price(input_, output, cache_read,
                          _number(spec.get("cache_write_5m")), _number(spec.get("cache_write_1h")))
    for prefix, input_, output, cache_read in sorted(_LIST, key=lambda row: len(row[0]), reverse=True):
        if model.startswith(prefix):
            return _price(input_, output, cache_read)
    return None


def cost_micro_usd(detail, price):
    """One model's token breakdown + its price -> micro-USD (an int, like every
    other number `usage` emits). A price in USD per 1M tokens is exactly
    micro-USD per token, so no scaling is needed."""
    total = 0.0
    for key in DETAIL_KEYS:
        count = detail.get(key, 0) if isinstance(detail, dict) else 0
        if isinstance(count, int) and not isinstance(count, bool) and count > 0:
            total += count * price[key if key != "fresh_input" else "input"]
    return int(round(total))


def estimate(by_model_detail, overrides=None):
    """{model: breakdown} -> {"micro_usd", "models", "unpriced_models", "price_list"}.

    `models[m]` carries the breakdown plus its own `micro_usd`. `unpriced_models`
    maps a model with no known price to its total token count, so the report
    can say exactly how much the dollar figure leaves out."""
    models = {}
    unpriced = {}
    total = 0
    for model, detail in sorted((by_model_detail or {}).items()):
        if not isinstance(detail, dict) or not any(detail.get(k) for k in DETAIL_KEYS):
            continue
        price = price_for(model, overrides)
        if price is None:
            unpriced[model] = sum(int(detail.get(k) or 0) for k in DETAIL_KEYS)
            continue
        entry = dict((k, int(detail.get(k) or 0)) for k in DETAIL_KEYS)
        entry["micro_usd"] = cost_micro_usd(detail, price)
        models[model] = entry
        total += entry["micro_usd"]
    return {
        "micro_usd": total,
        "models": models,
        "unpriced_models": unpriced,
        "price_list": {"as_of": PRICE_LIST_AS_OF, "source": PRICE_LIST_SOURCE},
    }
