# Intentional divergences from the Python implementation

One line per divergence: area | case | python behavior | go behavior | reason.
The goldens still pin Python's observable output; entries here are changes no
scenario can see (or, after the cutover, golden updates recorded with
`-impl=go -update`).

- telemetry | `telemetry enable|disable` on a config whose last line is a bare `telemetry:` anchor without a trailing newline | the inserted child is glued onto the anchor (`telemetry:  enabled: true`), invalid YAML | the anchor line is ended first (`telemetry:\n  enabled: true\n`) | real bug; no conformance scenario covers it, so the goldens are unchanged
- telemetry | `record` with an integer field above int64 (e.g. `usage.input_tokens: 99999999999999999999`) | stored as a bignum | the field is dropped by the sanitizer | token counts never approach 2^63; dropping keeps the allowlist guarantee
- sandbox | concurrent `sandbox allow` / `allow-domain` / `forget*` and auto grants | unlocked read-modify-write loses entries | updated under an exclusive lock (`sandbox-learned.json.lock`, 10 s timeout); store byte format unchanged | lost grants are a real bug
- watchdog | `watchdog.repeat_limit` that is not a number (or a bool, or <= 0) | a string/None crashes the watchdog thread (no budget enforced at all); 0 trips `repeat` at the first check | the repeat check is off, every other budget still applies | a typo must not silently disable the wall/idle budgets
- watchdog | `idle_seconds` / `tool_idle_seconds` / a `wall_seconds` entry that is not a number | the watchdog thread dies on the comparison (no budget enforced) | the value is ignored and its default applies | same reason
- pi | a JSONL line holding the non-standard literals `NaN` / `Infinity` | parsed by json.loads | not JSON: the line is unparseable and goes to the availability scan | pi never emits them; encoding/json rejects them
- capture | child stdout/stderr bytes that are a truncated multi-byte UTF-8 sequence | one U+FFFD per maximal invalid subpart | one U+FFFD per invalid byte | only invalid UTF-8 differs; valid output is identical
- capture | `_merge_usage` across grant cycles | merged keys in set (hash) order, varying between runs | the first run's keys, then the resumed run's new keys | deterministic; key order is outside the contract
- tools | the managed pi agent dir cannot be created | OSError propagates and the dispatch crashes | PI_CODING_AGENT_DIR still names it and pi reports the failure | a dispatch result beats a traceback
- fakepi | `FAKEPI-HANG` | (Go fakepi) a bare `select {}` the runtime aborted as a deadlock (exit 2) | blocks until signalled | the directive is documented as "block until killed"; no golden used it
