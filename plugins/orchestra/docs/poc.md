# PoC results

Measured on a run of 3 tasks in parallel, 8 agents total, 4 minutes 23 seconds, 246k total subagent tokens:

| Metric | Value |
|---|---|
| Haiku workers (4) — total output tokens | 9,638 |
| Sonnet reviewers (4) — total output tokens | 19,135 |
| Instructor's token consumption during execution | 0 |
| What the instructor received | ~2KB of structured JSON |
| Bug the worker's own 11 self-written tests missed | `formatBytes(1048575)` → `"1024 KiB"` (should be `"1 MiB"`, a rounding-carry boundary bug) |
| Round-trips to fix it | 1 (adversarial reviewer caught it, worker fixed it on retry with precise feedback) |

Reviewers cost roughly 2x the workers' output tokens — the adversarial test authoring is the main expense — but that cost bought detection of a bug the worker's own passing test suite completely missed.

## External executor PoC (Codex / Copilot / Claude model comparison)

The model policy in `skills/run/references/config.md` (which model/effort to use for Codex, Copilot, and Claude in each role) is backed by a 6-round PoC series, escalating from single-function traps up to a real 3-language (Go/Python/TypeScript) full-stack app, plus two follow-up rounds and a 5-run reproducibility check. Headline results:

- **5 straight rounds at single-file/small-multi-file scope found zero accuracy differentiation** across Codex/Copilot/Claude's cheapest tiers — including against a task deliberately engineered to catch a model proceeding on a mistaken belief. Cost and speed were the only differentiators.
- **The first round at real feature scope (3 languages, ~10 files, one shared spec) broke that ceiling**: the cheapest tier of two different providers (Codex `gpt-5.6-luna`/high, Claude Haiku) each produced one distinct, real, narrow bug, while their own mid/high tiers and a same-tier competitor (Copilot's `gpt-5.6-luna`) passed clean.
- **Follow-up: at least one of those failures was an effort-level artifact, not a model-capability one.** Re-running Codex Luna at `effort: medium` instead of `high` turned a 33/38 failing run into a 38/38 clean sweep — cheaper and faster besides. This plugin's default `standard` policy for Codex `gpt-5.6-luna` was changed from `effort: high` to `effort: medium` on the strength of this result.
- **Also resolved: `MAI-Code-1-Flash`'s real Copilot CLI model ID is `mai-code-1-flash-picker`** (not its display name). It's now a confirmed, cheap, fast, usable candidate — but it independently reproduced the same priority-sort inversion bug Codex Luna/high did, suggesting that specific trap may be a fairly generic failure mode for fast/cheap models.
- **A 5-run reproducibility check on Copilot Luna** (round 6's standout, and a candidate for regular use in place of Sonnet) found it strong but not flawless: 4 of 5 runs were a clean 38/38 sweep; the 5th hit one real defect (a `tsc --strict` type error) — of a kind that's cheap to catch and fix (the compiler flags it immediately and deterministically, unlike a logic bug that can hide behind passing tests), so it counts for less than the round-6 logic bugs even though it's still a real miss. Aggregate: 189/190 checks passed (99.5%) with tightly-clustered time/cost.

**Bottom line:** cost tier reliably predicted speed throughout, but never reliably predicted correctness at single-file/small-multi-file scope — only once task size crossed into real multi-file, multi-language feature territory did cheap tiers (on every provider tested, eventually including Copilot Luna itself once a large-enough sample was taken) start showing real, if narrow, defects. This is a concrete argument for orchestra's own design: treat the adversarial review stage as mandatory once a task exceeds "one small self-contained change," regardless of which model, provider, or effort level implemented the work.

**Full methodology, every round-by-round table, and the reasoning behind each policy change:** [`skills/run/references/poc-findings.md`](../skills/run/references/poc-findings.md) (Japanese). Read that file rather than this summary before making a model-policy decision that depends on the specific numbers.
