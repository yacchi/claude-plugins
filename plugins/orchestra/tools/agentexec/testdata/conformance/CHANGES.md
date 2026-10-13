# Intentional divergences from the Python implementation

One line per divergence: area | case | python behavior | go behavior | reason.
The goldens still pin Python's observable output; entries here are changes no
scenario can see (or, after the cutover, golden updates recorded with
`-impl=go -update`).

- telemetry | `telemetry enable|disable` on a config whose last line is a bare `telemetry:` anchor without a trailing newline | the inserted child is glued onto the anchor (`telemetry:  enabled: true`), invalid YAML | the anchor line is ended first (`telemetry:\n  enabled: true\n`) | real bug; no conformance scenario covers it, so the goldens are unchanged
- telemetry | `record` with an integer field above int64 (e.g. `usage.input_tokens: 99999999999999999999`) | stored as a bignum | the field is dropped by the sanitizer | token counts never approach 2^63; dropping keeps the allowlist guarantee
- sandbox | concurrent `sandbox allow` / `allow-domain` / `forget*` and auto grants | unlocked read-modify-write loses entries | updated under an exclusive lock (`sandbox-learned.json.lock`, 10 s timeout); store byte format unchanged | lost grants are a real bug
