# Intentional divergences from the Python implementation

One line per divergence: area | case | python behavior | go behavior | reason.

- telemetry | `telemetry enable|disable` on a config whose last line is a bare `telemetry:` anchor without a trailing newline | the inserted child is glued onto the anchor (`telemetry:  enabled: true`), invalid YAML | the anchor line is ended first (`telemetry:\n  enabled: true\n`) | real bug; no conformance scenario covers it, so the goldens are unchanged
- telemetry | `record` with an integer field above int64 (e.g. `usage.input_tokens: 99999999999999999999`) | stored as a bignum | the field is dropped by the sanitizer | token counts never approach 2^63; dropping keeps the allowlist guarantee
- check | a check whose shell cannot be started (e.g. `cwd` names a missing directory) | uncaught FileNotFoundError traceback, exit 1 | the check is reported as `fail` (exit null) with the start error as its output excerpt | a crash in the middle of a run lost every other result; no conformance scenario covers it
- wave lint | plan.json that is not valid JSON | `invalid JSON in plan <path>: <Python JSONDecodeError text>` | same prefix, Go's decoder text after it | the decoder wording cannot be reproduced; the scenario compares the prefix only
- wave lint | plan.json with a malformed shape the Python loader did not validate (`packages` not a list, a non-object package entry, non-string `files_owned`/`depends_on`/`preamble`/`external_done` items) | uncaught AttributeError/TypeError traceback | `plan.Error` with a message, exit 3 | a lint tool should say what is wrong; no conformance scenario covers it
- wave status | text view with pending approvals | `approvals: N pending` line from agent_exec_approvals | not printed until the approvals package (P11) installs `cli.ApprovalsPending` | the approvals package depends on wave/state, so it cannot be called from here; fixtures use already-allowed hosts so the goldens do not depend on it
