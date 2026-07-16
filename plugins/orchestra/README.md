# orchestra

A distributable, reusable Claude Code plugin for **cost-tiered multi-agent orchestration**: an expensive instructor model (e.g. Fable/Opus) decomposes work and writes orchestration scripts, cheap Haiku workers implement, Sonnet verifiers adversarially check the result, and only structured pass/fail verdicts flow back up.

## Why

Running every step of a task on your most expensive model wastes money on work that doesn't need it. `orchestra` codifies a pattern validated by a proof of concept: route implementation to a cheap model, route verification to a mid-tier model that actively tries to break the implementation, and keep the expensive instructor model out of the loop entirely except for planning and exception handling.

## Architecture

```
User request
  ▼
Instructor (Fable / Opus) ── classifies each request (router injected at SessionStart)
  │
  ├─ EXPRESS lane ─────────────────────────────────────────────────┐
  │   one self-contained change, no decomposition, no design       │
  │   decisions, small context bloat (or: conversational /         │
  │   read-only). When in doubt → NOT express.                     │
  │   → handled directly, or by ONE disposable cheap worker        │
  │     (haiku/sonnet), instructor reviews, no verifier pipeline.  │
  │   → scope moves mid-flight? abort → re-route to ORCHESTRATED.  │
  │                                                                 │
  └─ ORCHESTRATED lane (the `orchestra:run` skill)                 │
      │  decomposes tasks, defines contracts, writes the script     │
      │  NEVER reads implementation files, NEVER sees logs/diffs    │
      ▼                                                             │
    Workflow script  ── or ──  orchestra-delegate (Sonnet fallback) │
      │  drives the loop, holds context across rounds               │
      ▼                                                             │
    orchestra-worker (Haiku) ──implements──▶ orchestra-verifier     │
      ▲                                        (Sonnet, adversarial)│
      └────── retry with precise feedback ──────┘  (up to 3 rounds) │
      │                                                             │
      ▼                                                             │
    Structured verdict only: { pass, summary, feedback? }           │
      │                                                             │
      ▼                                                             ▼
    Instructor receives ~2KB of JSON, zero tokens spent on execution
```

Two execution paths are supported:

1. **Dynamic Workflows** (preferred, when available): the instructor writes a pure-JavaScript workflow script using `agent()` and `pipeline()`. See `skills/run/SKILL.md` for the template.
2. **Nested subagents** (fallback, when Workflows are unavailable): the instructor spawns `orchestra-delegate`, which internally spawns `orchestra-worker` and `orchestra-verifier` and manages the retry loop itself, reporting back only a structured verdict.

**The one rule that matters in both paths**: every agent invocation must explicitly set `model` or `agentType`. Omitting both causes the spawned agent to silently inherit the session's (expensive) model, which defeats the entire cost-tiering strategy.

## Components

| File | Role | Model |
|---|---|---|
| `agents/orchestra-worker.md` | Mechanical implementation worker. Implements literally, no scope expansion, self-verifies before reporting. | Haiku |
| `agents/orchestra-hard-worker.md` | Design-sensitive implementation worker for tasks with real design latitude (algorithm choice, API shape, tradeoffs). Decides within the contract's bounds and reports decisions with rationale. | Opus |
| `agents/orchestra-verifier.md` | Adversarial verifier. Re-runs the worker's tests, writes ≥3 adversarial edge-case tests of its own, returns a strict verdict. | Sonnet |
| `agents/orchestra-delegate.md` | Middle-manager fallback for environments without Dynamic Workflows. Holds context across retry rounds, drives worker→verifier→retry, escalates only genuine ambiguity. | Sonnet |
| `skills/run/SKILL.md` | The playbook (`/run`, or `orchestra:run` cross-plugin). Read by the instructor. Express-lane criteria, model tier table, the workflow template, worker-prompt-writing guidance, the fallback pattern, and a short pointer into the external-executor policy below. Kept deliberately lean (English, for token efficiency) — detail lives in `references/`. | — |
| `skills/setup/SKILL.md` | Companion config skill (`/setup`, or `orchestra:setup` cross-plugin). Detects Codex/Copilot CLI + agent availability, then interactively edits `.claude/orchestra.yaml` (project) or `~/.claude/orchestra.yaml` (user) — enabling/disabling executors, choosing scope, and migrating an old `orchestra.json`. Never runs the pipeline itself. | — |
| `skills/run/references/external-executors.md` | Operational reference for external executors (Japanese) — Codex's Sol/Terra/Luna + effort policy, Copilot's model catalog and CLI usage recipes (including session continuation), and official per-token pricing for Codex/Copilot/Claude. Not loaded automatically; read on demand when actually dispatching to Codex/Copilot. | — |
| `skills/run/references/poc-findings.md` | The full research log behind the external-executor model policy (Japanese) — every PoC round, bug found, and policy change, with the reasoning. Not loaded automatically; read on demand. | — |
| `skills/run/references/poc-fixtures/` | Reusable fixtures for every PoC round (task specs, verification harnesses, reference/buggy implementations) so a new model can be re-tested and compared without rebuilding anything from scratch. See its own `README.md` for exact reproduction commands. | — |
| `hooks/hooks.json` + `hooks/inject-router.sh` | Auto-activation. Injects the `<orchestra-router>` lane-classification protocol into context at session start and re-injects it after `/clear`, resume, and compaction. | — |
| `examples/orchestra.yaml` | Sample configuration file (YAML, fully commented). Copy to `.claude/orchestra.yaml` (project) or `~/.claude/orchestra.yaml` (user) to override model tiers and declare external executors, or use `/setup` to author it interactively. | — |

## Installation

This plugin is distributed as part of the `yacchi-plugins` marketplace:

```text
/plugin marketplace add yacchi/claude-plugins
/plugin install orchestra@yacchi-plugins
```

For local development, add the marketplace repository root as a local marketplace instead:

```text
/plugin marketplace add ./
/plugin install orchestra@yacchi-plugins
```

Validate from the marketplace repository root before distributing:

```bash
claude plugin validate .
```

## Auto-activation

Once installed and enabled, the plugin's `SessionStart` hook (`hooks/hooks.json`) runs `hooks/inject-router.sh`, whose stdout is added to Claude's context. The injected `<orchestra-router>` block tells the session to classify **each** user request into one of two lanes before acting:

- **EXPRESS**: one self-contained change (or conversational/read-only), no decomposition, no design decisions, small expected context bloat. File count is explicitly *not* a criterion — incidental doc updates may ride along. Handled directly or via a single disposable cheap subagent, with no verifier pipeline. If scope or success criteria move mid-flight, express is aborted and the task re-routes to the orchestrated lane. (Lane criteria ported from `discus0434/customizable-agent-teams`.)
- **ORCHESTRATED**: everything else, and *whenever in doubt*. The session loads the `run` skill and follows the full cost-tiered pipeline.

Two design notes:

- **Hybrid gating**: the hook first tries to read the top-level `model` field from the SessionStart input JSON (via `python3`; the field is officially omittable). When the model is identified as Opus/Fable (case-insensitive substring match), the router is injected without its self-gate paragraph; when it's identifiably a cheaper model (Sonnet/Haiku), nothing is injected at all — zero context cost. Only when the model can't be determined (missing field, bad JSON, no `python3`) does it fall back to injecting the full self-gated text, which tells the session itself to ignore the block unless its main model is Opus or Fable. The script never exits nonzero, so a parsing failure can't disrupt session startup.
- **Compaction-proof**: injected context is not automatically re-added after compaction, so the hook registers the `SessionStart` matchers `startup|resume|clear|compact` — the router survives new sessions, resumes, `/clear`, and context compaction.

## Usage

Invoke the playbook explicitly:

```text
/run
```

Or just describe a task that needs cost-tiered delegation to many cheap workers with adversarial checking — Claude can invoke the skill automatically when it recognizes that shape of task, since it isn't marked for manual invocation only.

The skill instructs the instructor to:

1. Decompose the task and define per-task contracts (literal spec, edge cases as input/output examples, a verification command).
2. Write (or reuse) a Dynamic Workflow script that pipelines each task through `orchestra-worker` → `orchestra-verifier` → retry-with-feedback (capped at 3 rounds), or fall back to spawning `orchestra-delegate` when Workflows aren't available. Design-latitude tasks route to `orchestra-hard-worker` (Opus) instead of the Haiku worker.
3. Receive only structured verdicts, never raw logs, diffs, or intermediate files.

## Configuration

At the start of each orchestration, the instructor resolves config from three layers, deep-merged in this order (later wins): built-in defaults (haiku/opus/sonnet, no external executors) ← user `~/.claude/orchestra.yaml` ← project `.claude/orchestra.yaml`. Because it's a deep merge and not a first-found-wins lookup, a project file only needs to state the keys it wants to change — e.g. just `external_executors.copilot.enabled: true` — and everything else is inherited. See `examples/orchestra.yaml` for the full schema, and `skills/run/SKILL.md` §9 for the exact merge algorithm.

- **`tiers`** overrides the default model for each role (`worker`, `hard_worker`, `verifier`).
- **`external_executors`** lets non-Claude executors (Codex, Copilot, ...) participate as implementation workers and/or independent verifiers. `dispatch: "agent"` routes through an installed plugin subagent (`agentType`/`subagent_type`), falling back to normal model tiers when the name doesn't resolve; `dispatch: "cli"` runs a non-interactive CLI via a cheap Haiku relay agent so the CLI's output never lands in the instructor's context. Each executor's `model_policy` pins a concrete model+effort per role (Codex: Sol/Terra/Luna; Copilot: its own multi-provider catalog) — see `skills/run/SKILL.md` §9 for the benchmark-sourced policy and a Copilot CLI usage/session-continuation recipe (Copilot has no dedicated Claude Code skill of its own, unlike Codex).
- Safety: `cli` dispatch executes only command templates from the user's own config file — the plugin ships no CLI commands of its own.
- Use `/setup` (`skills/setup/SKILL.md`, resolvable cross-plugin as `orchestra:setup`) to set this up interactively instead of hand-editing: it detects whether Codex/Copilot are actually available in this environment, asks which scope (project vs user) to write to, and edits in place without disturbing existing comments. It also handles converting a pre-YAML `orchestra.json`, which is no longer read.

## PoC results

Measured on a run of 3 tasks in parallel, 8 agents total, 4 minutes 23 seconds, 246k total subagent tokens:

| Metric | Value |
|---|---|
| Haiku workers (4) — total output tokens | 9,638 |
| Sonnet verifiers (4) — total output tokens | 19,135 |
| Instructor's token consumption during execution | 0 |
| What the instructor received | ~2KB of structured JSON |
| Bug the worker's own 11 self-written tests missed | `formatBytes(1048575)` → `"1024 KiB"` (should be `"1 MiB"`, a rounding-carry boundary bug) |
| Round-trips to fix it | 1 (adversarial verifier caught it, worker fixed it on retry with precise feedback) |

Verifiers cost roughly 2x the workers' output tokens — the adversarial test authoring is the main expense — but that cost bought detection of a bug the worker's own passing test suite completely missed.

## External executor PoC (Codex / Copilot / Claude model comparison)

The model policy in `skills/run/SKILL.md` §9 (which model/effort to use for Codex, Copilot, and Claude in each role) is backed by a 6-round PoC series, escalating from single-function traps up to a real 3-language (Go/Python/TypeScript) full-stack app, plus two follow-up rounds and a 5-run reproducibility check. Headline results:

- **5 straight rounds at single-file/small-multi-file scope found zero accuracy differentiation** across Codex/Copilot/Claude's cheapest tiers — including against a task deliberately engineered to catch a model proceeding on a mistaken belief. Cost and speed were the only differentiators.
- **The first round at real feature scope (3 languages, ~10 files, one shared spec) broke that ceiling**: the cheapest tier of two different providers (Codex `gpt-5.6-luna`/high, Claude Haiku) each produced one distinct, real, narrow bug, while their own mid/high tiers and a same-tier competitor (Copilot's `gpt-5.6-luna`) passed clean.
- **Follow-up: at least one of those failures was an effort-level artifact, not a model-capability one.** Re-running Codex Luna at `effort: medium` instead of `high` turned a 33/38 failing run into a 38/38 clean sweep — cheaper and faster besides. This plugin's default `worker` policy for Codex `gpt-5.6-luna` was changed from `effort: high` to `effort: medium` on the strength of this result.
- **Also resolved: `MAI-Code-1-Flash`'s real Copilot CLI model ID is `mai-code-1-flash-picker`** (not its display name). It's now a confirmed, cheap, fast, usable candidate — but it independently reproduced the same priority-sort inversion bug Codex Luna/high did, suggesting that specific trap may be a fairly generic failure mode for fast/cheap models.
- **A 5-run reproducibility check on Copilot Luna** (round 6's standout, and a candidate for regular use in place of Sonnet) found it strong but not flawless: 4 of 5 runs were a clean 38/38 sweep; the 5th hit one real defect (a `tsc --strict` type error) — of a kind that's cheap to catch and fix (the compiler flags it immediately and deterministically, unlike a logic bug that can hide behind passing tests), so it counts for less than the round-6 logic bugs even though it's still a real miss. Aggregate: 189/190 checks passed (99.5%) with tightly-clustered time/cost.

**Bottom line:** cost tier reliably predicted speed throughout, but never reliably predicted correctness at single-file/small-multi-file scope — only once task size crossed into real multi-file, multi-language feature territory did cheap tiers (on every provider tested, eventually including Copilot Luna itself once a large-enough sample was taken) start showing real, if narrow, defects. This is a concrete argument for orchestra's own design: treat the adversarial verifier stage as mandatory once a task exceeds "one small self-contained change," regardless of which model, provider, or effort level implemented the work.

**Full methodology, every round-by-round table, and the reasoning behind each policy change:** [`skills/run/references/poc-findings.md`](skills/run/references/poc-findings.md) (Japanese). Read that file rather than this summary before making a model-policy decision that depends on the specific numbers.

## Notes on schema conformance

Built by fetching and following the official Claude Code plugin documentation (`plugin-marketplaces`, `plugins-reference`, `sub-agents`, `skills`) rather than from memory. See the accompanying validation report for exact field-by-field conformance and any fields that were intentionally omitted.
