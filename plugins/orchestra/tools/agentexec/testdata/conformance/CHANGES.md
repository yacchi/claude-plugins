# Intentional divergences from the Python implementation

One line per divergence: area | case | python behavior | go behavior | reason.

- telemetry | `telemetry enable|disable` on a config whose last line is a bare `telemetry:` anchor without a trailing newline | the inserted child is glued onto the anchor (`telemetry:  enabled: true`), invalid YAML | the anchor line is ended first (`telemetry:\n  enabled: true\n`) | real bug; no conformance scenario covers it, so the goldens are unchanged
- telemetry | `record` with an integer field above int64 (e.g. `usage.input_tokens: 99999999999999999999`) | stored as a bignum | the field is dropped by the sanitizer | token counts never approach 2^63; dropping keeps the allowlist guarantee
- integrate | `--verify` command that exits while a background child still holds its output pipe | waits on the pipe until `--verify-timeout`, then reports a timeout | stops collecting output 5s after the command exits and grades its exit status | a daemonized child must not turn a finished verify into an hour-long hang
- integrate | the lock file under `.git/orchestra/` cannot be created | uncaught OSError (traceback, exit 1) | `status: error` with the reason in `note`, exit 3 | a repository failure belongs in the result, not a crash
- adopt | `isolate adopt --task` with no usable characters (e.g. `//`) | uncaught ValueError (traceback, exit 1) | usage error, exit 2, like every other isolate subcommand | real bug; no conformance scenario covers it
- adopt | `isolate adopt --baseline ""` | error "could not resolve --baseline ref ''" (exit 3) | treated as no `--baseline` (the tree's HEAD) | the domain API takes "" as "not given"; an empty ref is never meaningful
