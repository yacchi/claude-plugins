# orchestra

A distributable, reusable Claude Code plugin for **cost-tiered multi-agent orchestration**: an expensive instructor model (e.g. Fable/Opus) decomposes work and writes orchestration scripts, `agent-exec route` picks the cheapest ready implementer for each task (an external executor like Copilot by default, Claude Haiku as fallback), Sonnet reviewers adversarially check the result, and only structured pass/fail verdicts flow back up.

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
  │     (routed via agent-exec; haiku/sonnet fallback), reviewed   │
  │     by the instructor itself, no review pipeline.              │
  │   → scope moves mid-flight? abort → re-route to ORCHESTRATED.  │
  │                                                                 │
  └─ ORCHESTRATED lane (the `orchestra:run` skill)                 │
      │  decomposes tasks, defines contracts, writes the script     │
      │  NEVER reads implementation files, NEVER sees logs/diffs    │
      ▼                                                             │
    Workflow script  ── or ──  orchestra-delegate (Sonnet fallback) │
      │  drives the loop, holds context across rounds               │
      ▼                                                             │
    light-class impl ──implements──────────▶ orchestra-review       │
    (agent-exec route: Copilot default,                             │
     Claude Haiku fallback)                    (Sonnet, adversarial)│
      ▲                                                             │
      └─ ONE correction round: cited, family-swept packet to a      │
         FRESH worker, then an incremental re-gate  (2 gates max)   │
      │                                                             │
      ▼                                                             │
    Structured verdict only: { pass, summary, feedback?,            │
                               optional_hardening?, needsInstructor? }│
      │                                                             │
      ▼                                                             ▼
    Instructor receives ~2KB of JSON, zero tokens spent on execution
```

Two execution paths are supported:

1. **Dynamic Workflows** (preferred, when available): the instructor writes a pure-JavaScript workflow script using `agent()` and `pipeline()`. See `skills/run/SKILL.md` for the template.
2. **Nested subagents** (fallback, when Workflows are unavailable): the instructor spawns `orchestra-delegate`, which internally spawns `orchestra-light` and `orchestra-review` and manages the retry loop itself, reporting back only a structured verdict.

**The one rule that matters in both paths**: every agent invocation must explicitly set `model` or `agentType`. Omitting both causes the spawned agent to silently inherit the session's (expensive) model, which defeats the entire cost-tiering strategy.

## Gate discipline (v0.14.0)

Rounds are the pipeline's real cost, and most slow runs are slow because rounds were spent badly rather than because a model was weak. v0.14.0 replaces the blind 3-retry counter with four rules (`skills/run/SKILL.md` §11):

- **A rejection must cite the contract.** A reviewer may not invent a stricter requirement, tighten a tolerance the spec left open, or reject a design choice the spec delegated; `cited_contract` is a required field on every finding. Improvements it noticed but the contract doesn't require go to `optional_hardening`, which never blocks a pass. Gold-plating reviewers were the most expensive failure mode available — they burn every remaining round on work nobody asked for and still fail the task.
- **Findings carry a defect `family`, and the reviewer sweeps the family before reporting.** One round closes a whole class of bug (rounding-carry, input normalization, error-type mismatch, ordering, serialization round-trip, …) instead of one instance; a one-finding-per-round loop is how a 3-round budget gets spent on three siblings of the same defect.
- **Two gates, then a human-shaped decision.** One initial gate plus one incremental re-gate — the re-gate gets the previous verdict, the closed finding ids and the changed paths, and inspects only those plus plausible regressions (6 read-only shell invocations, first line `VERDICT: PASS|FAIL`). If it surfaces a family that predates the correction, the loop stops and returns `needsInstructor`: the *sweep* was wrong, so another automatic round would just find the next sibling.
- **Escalate the class; never resume the failed worker.** Workers report `ESCALATE` when the packet doesn't settle a needed decision, the change crosses their file ownership, or a failure isn't locally explainable — that bumps `light`→`standard`→`deep` without consuming a gate, since a mis-sized packet is not a defect. The same family failing twice bumps it too, and security/auth/concurrency work starts at the higher tier rather than proving itself through a cheap failure. Corrections always go to a *fresh* invocation via a self-contained packet: the rejected instance is anchored on the reasoning that produced the defect.

Workers are also capped at ~12 shell invocations (reviews 12, re-gates 6). An interrupted or budget-exhausted review is a FAIL, never a PASS.

## Rollback, diffs, and aggressive isolation (v0.14.0)

Version control is effectively free, so the pipeline assumes it (`skills/run/references/isolation.md`). A baseline snapshot is taken before the first worker — `git init` if the tree isn't a repo — and between attempts, which buys two things: the reviewer diffs against something real instead of inferring the worker's footprint from prose, and a bad correction is rolled back rather than patched on top of.

That in turn makes `isolation: 'worktree'` worth using aggressively. It lifts the constraint that parallel workers must own disjoint files, so genuinely tangled work can be parallelized, exploratory failures cost nothing, and **competing implementations of one contract** become practical: run N variants, judge them against one adversarial suite, and treat their disagreements as a defect report about the *spec* — ambiguity found before the bug ships. It costs N× implementation tokens, so it's for the risky core, not the default shape.

Workers never touch VCS state; snapshots, merges, and cleanup belong to the supervising layer. Descendant agent branches are unrestricted and disposable, but the branch that becomes the PR is built from the accepted diff and carries no checkpoints, rejected variants, or worker-only files. Non-interactive CLI executors also get their approvals front-loaded, since a permission prompt they can't surface becomes a silent stall.

## Components

| File | Role | Model |
|---|---|---|
| `agents/orchestra-light.md` | Mechanical implementation worker. Implements literally, no scope expansion, self-verifies before reporting. | Haiku |
| `agents/orchestra-deep.md` | Design-sensitive implementation worker for tasks with real design latitude (algorithm choice, API shape, tradeoffs). Decides within the contract's bounds and reports decisions with rationale. | Opus |
| `agents/orchestra-review.md` | Adversarial review. Re-runs the worker's tests, writes ≥3 adversarial edge-case tests of its own, returns a strict verdict. Two hard boundaries as of v0.14.0: it reviews but never repairs (`Write` is for its own new test files only — patching the implementation destroys the signal), and a FAIL requires a cited contract violation, with everything else demoted to non-blocking `optional_hardening`. Classifies findings by defect family and sweeps siblings before reporting; supports an incremental re-gate mode and a hard inspection budget. | Sonnet |
| `agents/orchestra-delegate.md` | Middle-manager fallback for environments without Dynamic Workflows. Holds context across retry rounds, dispatches each implementation round itself via `agent-exec dispatch --class light` (no relay needed — it already has Bash) rather than spawning a Claude implementer directly, drives review→retry, escalates only genuine ambiguity. | Sonnet |
| `skills/run/SKILL.md` | The playbook (`/run`, or `orchestra:run` cross-plugin). Read by the instructor. Instructor code of conduct, express-lane criteria, capability-class table, the one model-pinning rule, the workflow template, worker-prompt requirements, and short condensed sections on latency, fallback, configuration, telemetry, gate discipline, and isolation — each pointing at the reference file that holds the detail. Deliberately lean (English, for token efficiency): it is loaded on every orchestrated run, so as of v0.12.0 rationale and detail live in `references/` and only rules that change what the instructor *does* stay inline. | — |
| `skills/run/references/authoring.md` | Detail split out of the playbook in v0.12.0 (English, verbatim): same-tree parallelism safety and `agentType` resolution, shortening the per-task critical path (overlapping adversarial-test authoring with implementation, never serializing independent work behind a barrier, sizing tasks to the concurrency width), prompt density, the measured PoC findings, and the no-Workflow `orchestra-delegate` fallback shape. Not loaded automatically; read while writing a workflow script. | — |
| `skills/run/references/gates.md` | New in v0.14.0 (English): why each gate rule exists — the gold-plating reviewer as the most expensive failure mode, why defects arrive in families, why a re-gate is incremental and why `new_family` stops the loop (and when a third round *is* right), the escalation ladder in both directions, why corrections go to a fresh worker while the delegate is resumed, and invocation budgets as a diagnostic. Not loaded automatically. | — |
| `skills/run/references/isolation.md` | New in v0.14.0 (English): baseline/checkpoint mechanics and jj recovery, branch hygiene (disposable descendants, a clean PR branch built from the accepted diff), worktree-isolated parallelism and supervisor merge, competing implementations and how to read their divergence, verifying on a context-free integration tree, and front-loading approvals for non-interactive workers. Not loaded automatically. | — |
| `skills/run/references/config.md` | Detail split out of the playbook in v0.12.0 (English, verbatim): the four-layer config schema and deep-merge algorithm (plus its pre-v0.11.0 upgrade trap), `agent-exec route`/`dispatch` gating, external-executor dispatch mechanisms and the `dispatch: cli` sandboxing caveat, `enforcement.light_class` and its four escape hatches, and the full telemetry field allowlist and CLI. Not loaded automatically; read only when a configuration question actually arises. | — |
| `skills/setup/SKILL.md` | Companion config skill (`/setup`, or `orchestra:setup` cross-plugin). Detects Codex/Copilot CLI + agent availability, then interactively edits `.claude/orchestra.yaml` (project) or `~/.claude/orchestra.yaml` (user) — enabling/disabling executors, choosing scope, and migrating an old `orchestra.json`. Never runs the pipeline itself. | — |
| `skills/run/references/external-executors.md` | Operational reference for external executors (Japanese) — Codex's Sol/Terra/Luna + effort policy, Copilot's model catalog and CLI usage recipes (including session continuation), and official per-token pricing for Codex/Copilot/Claude. Not loaded automatically; read on demand when actually dispatching to Codex/Copilot. | — |
| `skills/run/references/poc-findings.md` | The full research log behind the external-executor model policy (Japanese) — every PoC round, bug found, and policy change, with the reasoning. Not loaded automatically; read on demand. | — |
| `skills/run/references/poc-fixtures/` | Reusable fixtures for every PoC round (task specs, verification harnesses, reference/buggy implementations) so a new model can be re-tested and compared without rebuilding anything from scratch. See its own `README.md` for exact reproduction commands. | — |
| `tools/agent_exec.py` (`agent-exec` on `PATH` after `agent-exec install`) | The `agent-exec` CLI. `route --class <cls>` and `dispatch --class <cls> ... --capture` execute the `priority` walk deterministically (config load → enabled/binary/`ready.<x>.ok`/`class_policy`/`--exhausted` gating → optional CLI run) so the instructor never performs the walk by hand — it asks, via one relay agent, and branches on the JSON reply. Also `config`, `doctor`, `run`, `install`, and `telemetry`. See `skills/run/references/config.md` and `references/external-executors.md` §5. | — |
| `hooks/hooks.json` + `hooks/inject-router.sh` | Auto-activation. Injects the `<orchestra-router>` lane-classification protocol into context at session start and re-injects it after `/clear`, resume, and compaction. Also persists the model-gate verdict to a per-session state file for the reminder hook. | — |
| `hooks/remind-router.sh` | Per-prompt recency. A `UserPromptSubmit` hook that appends a ~45-token router reminder on every user prompt of instructor-model sessions, so the classification step stays adjacent to the request it must classify even in very long sessions. Gated via the state file written by `inject-router.sh`; emits nothing on cheap-model sessions. | — |
| `hooks/enforce-router.sh` | Opt-in (`enforcement.light_class: "block"`, ships `"off"` — quote the value, YAML 1.1 reads a bareword `off` as boolean) `PreToolUse` guardrail — a **nudge with a guaranteed escape, never a hard wall**. Fires only on a *generic* Claude implementer call (no `subagent_type`, or `subagent_type` in `general-purpose`/`claude`, at a Haiku-class model) while `agent-exec route --class light` reports a ready non-Claude executor; naming any other specific `subagent_type` is itself a carve-out (the Agent tool has no per-call tool-restriction params, so a named agent is how tool access/system prompt get pinned — Copilot can't substitute for that). Denies **at most once per session, full stop** — not per prompt/task, since this plugin's own retry convention mutates the prompt every round. Other escape hatches: a `[orchestra:allow-claude: reason]` marker, and `ORCHESTRA_ENFORCEMENT=off` plus fail-open on any uncertainty. See `skills/run/references/config.md`. | — |
| `examples/orchestra.yaml` | Sample configuration file (YAML, fully commented). Copy to `.claude/orchestra.yaml` (project) or `~/.claude/orchestra.yaml` (user) to override model tiers, declare external executors (now `enabled: true` by default), or set `enforcement.light_class`, or use `/setup` to author it interactively. | — |

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

- **EXPRESS**: one self-contained change (or conversational/read-only), no decomposition, no design decisions, small expected context bloat. File count is explicitly *not* a criterion — incidental doc updates may ride along. Two gates keep the lane honest in both directions: the criterion is **expected context bloat, not the session's model tier** (being on an expensive model is not itself a reason to delegate), and **content the session already holds** — a spec it just designed, a fix it already worked out — is express regardless of size, since delegation cannot shrink context that is already held. Handled directly or via a single disposable cheap subagent, with no review pipeline (though the *verification run* is still handed to a cheap subagent so its output never floods the instructor). If scope or success criteria move mid-flight, express is aborted and the task re-routes to the orchestrated lane. (Lane criteria ported from `discus0434/customizable-agent-teams`.)
- **ORCHESTRATED**: everything else, and *whenever in doubt*. The session loads the `run` skill and follows the full cost-tiered pipeline.

The router block also lists explicit **tripwires** that force a mid-conversation re-route to ORCHESTRATED: hand-writing substantive code in 2+ files, a design discussion turning into an implementation request, or the session planning a multi-step implement-then-verify sequence for itself. Lane choices never persist across requests.

Three design notes:

- **Hybrid gating**: the hook first tries to read the top-level `model` field from the SessionStart input JSON (via `python3`; the field is officially omittable). When the model is identified as Opus/Fable (case-insensitive substring match), the router is injected without its self-gate paragraph; when it's identifiably a cheaper model (Sonnet/Haiku), nothing is injected at all — zero context cost. Only when the model can't be determined (missing field, bad JSON, no `python3`) does it fall back to injecting the full self-gated text, which tells the session itself to ignore the block unless its main model is Opus or Fable. The script never exits nonzero, so a parsing failure can't disrupt session startup.
- **Compaction-proof**: injected context is not automatically re-added after compaction, so the hook registers the `SessionStart` matchers `startup|resume|clear|compact` — the router survives new sessions, resumes, `/clear`, and context compaction.
- **Recency-proof**: a single SessionStart injection sits at the very top of the context and decays in long sessions — transcript analysis found instructor-model sessions doing 200+ direct edits with zero mention of the lane classification. The `UserPromptSubmit` hook (`hooks/remind-router.sh`) therefore re-surfaces a short reminder next to every new prompt. Since UserPromptSubmit input is not guaranteed to carry a `model` field, `inject-router.sh` persists its gate verdict (`instructor`/`cheap`/`unknown`) to `${TMPDIR:-/tmp}/orchestra-router-state-<session_id>`; the reminder hook reads that file, staying silent on cheap-model sessions and falling back to a self-gated reminder when the verdict is unknown.

## Usage

Invoke the playbook explicitly:

```text
/run
```

Or just describe a task that needs cost-tiered delegation to many cheap workers with adversarial checking — Claude can invoke the skill automatically when it recognizes that shape of task, since it isn't marked for manual invocation only.

The skill instructs the instructor to:

1. Decompose the task and define per-task contracts (literal spec, edge cases as input/output examples, a verification command).
2. Write (or reuse) a Dynamic Workflow script that pipelines each task through a light-class implementer (resolved by `agent-exec route` — Copilot by default, `orchestra-light`/Haiku as fallback) → `orchestra-review` → at most one correction round followed by an incremental re-gate (two gates, not a blind retry count — see "Gate discipline" below), or fall back to spawning `orchestra-delegate` when Workflows aren't available. Design-latitude tasks route to `orchestra-deep` (Opus) instead of the light-class worker.
3. Receive only structured verdicts, never raw logs, diffs, or intermediate files.

## Configuration

At the start of each orchestration, the instructor resolves config from four layers, deep-merged in this order (later wins): built-in defaults ← user `~/.claude/orchestra.yaml` ← project `.claude/orchestra.yaml` ← project-local `.claude/orchestra.local.yaml`. The built-in defaults are `tiers` (light=haiku, standard=sonnet, deep=opus, review=sonnet — still the Claude-side fallback models) plus, as of v0.11.0, `external_executors.copilot` and `external_executors.codex` shipping **`enabled: true`** with their `class_policy` and a default `priority` (Copilot/Codex Luna first for `light`/`standard`, Claude first for `deep`/`review`), and `enforcement.light_class: off`. Shipping external executors on by default is safe because selection is gated on real availability, not assumed (see `route`/`dispatch` below) — on a machine with neither CLI installed, every call still resolves to `claude`. Because it's a deep merge and not a first-found-wins lookup, a project file only needs to state the keys it wants to change — e.g. just `external_executors.copilot.enabled: false` to opt back out — and everything else is inherited. This resolution can be done deterministically via `agent-exec config` (which emits the merged config as JSON with external-executor availability) instead of merging in-context. See `examples/orchestra.yaml` for the full schema, and `skills/run/references/config.md` for the exact merge algorithm.

- **`tiers`** overrides the default model for each capability class/role (`light`, `standard`, `deep`, `review`) — these are the Claude-side fallback used whenever routing resolves to `claude`.
- **`external_executors`** lets non-Claude executors (Codex, Copilot, ...) participate as implementers and/or independent reviewers. `dispatch: "agent"` routes through an installed plugin subagent (`agentType`/`subagent_type`), falling back to normal model tiers when the name doesn't resolve; `dispatch: "cli"` runs a non-interactive CLI via a cheap Haiku relay agent so the CLI's output never lands in the instructor's context. Each executor's `class_policy` pins a concrete model+effort per class/role (Codex: Sol/Terra/Luna; Copilot: its own multi-provider catalog) — see `skills/run/references/config.md` for the benchmark-sourced policy and a Copilot CLI usage/session-continuation recipe (Copilot has no dedicated Claude Code skill of its own, unlike Codex). Because Copilot uses `dispatch: cli`, the relay's CLI call must be allowlisted in Claude Code's `settings.json`. The recommended path is the bundled `agent-exec` wrapper (installed once via `agent-exec install`), which needs only a single `Bash(agent-exec:*)` rule in `permissions.allow` — the current Copilot CLI runs autonomously in `-p` mode with just `--add-dir`, so no `COPILOT_ALLOW_ALL` env or `--allow-all-tools` flag is required (and Copilot's own `--allow-tool`/`--deny-tool` flags don't reliably confine it, since the agent reroutes around a removed tool — real containment needs an external sandbox). Without the wrapper, the manual alternative is a `Bash(copilot:*)` allow rule. `/setup` offers to configure this.
- **`priority`** declares, per class/role (and, for `light`, per task-archetype — `investigation` vs `default`), an ordered list of executors to try (`external_executors` keys or the `claude` sentinel) — still the source of truth for preference order, but the instructor no longer walks it by hand. **`agent-exec route --class <cls> [--archetype A] [--exhausted a,b]`** executes the walk itself: it deep-merges config, then hard-gates each candidate on `enabled`, binary/agent resolution, `doctor`'s `ready.<x>.ok`, a matching `class_policy` entry, and the caller's `--exhausted` list, returning the surviving top candidate. **`agent-exec dispatch --class <cls> --prompt-file F --workdir W [--capture]`** goes further: it calls `route` and, for a `dispatch: cli` winner, also runs it and returns `{status: 'ok'|'unavailable', answer, ...}`; for Claude or a `dispatch: agent` winner it returns `{status: 'delegate', executor, model, agent_type}` for the instructor to actually spawn; with nothing viable, `{status: 'unroutable'}`. This reactive fallback triggers only on an *unavailable* signal (a rate-limit/usage-window cap for Claude/Codex, AI-credit/premium-request exhaustion for Copilot, or a disabled/unresolved executor) — a merely wrong result is not a fallback signal and stays on the same executor through the normal review/retry loop. The choice is sticky within a run (the instructor carries a growing `--exhausted` list forward) and `priority` supersedes `classes` for ordering when present. See `skills/run/SKILL.md` §5 (the `dispatchClass()` helper) and `skills/run/references/config.md` for the full schema.
- **`enforcement.light_class`** (`"off"` default | `"block"` — quote the value; YAML 1.1 reads bareword `off` as boolean `False`) is an opt-in, **nudge, never a hard wall**: when `"block"`, a `PreToolUse` hook denies **at most once per session** (not per task/prompt — this plugin's own retry convention mutates the prompt every round, so a prompt-keyed cap would just re-deny every round) a *generic* Claude implementer spawned (no `subagent_type`, or `general-purpose`/`claude`, at a Haiku-class model) while a ready external executor exists for `light`, and points the model at `agent-exec dispatch` instead. Naming any other specific `subagent_type` is itself a carve-out — the Agent tool has no per-call tool-restriction parameters, so a named agent is how tool access and a specialized system prompt actually get pinned, which Copilot can't substitute for. An explicit `[orchestra:allow-claude: reason]` marker and a kill switch with fail-open on any uncertainty round out the guarantee that it can never strand a legitimate direct-Claude need. See `skills/run/references/config.md`.
- Safety: `cli` dispatch executes only command templates from the user's own config file — the plugin ships no CLI command templates of its own (the optional `agent-exec` wrapper is a separate, user-installed and user-consented tool).
- Use `/setup` (`skills/setup/SKILL.md`, resolvable cross-plugin as `orchestra:setup`) to set this up interactively instead of hand-editing: it detects whether Codex/Copilot are actually available in this environment, asks which scope (project vs user) to write to, and edits in place without disturbing existing comments. It also handles converting a pre-YAML `orchestra.json`, which is no longer read.

## PoC results

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

**Full methodology, every round-by-round table, and the reasoning behind each policy change:** [`skills/run/references/poc-findings.md`](skills/run/references/poc-findings.md) (Japanese). Read that file rather than this summary before making a model-policy decision that depends on the specific numbers.

## Notes on schema conformance

Built by fetching and following the official Claude Code plugin documentation (`plugin-marketplaces`, `plugins-reference`, `sub-agents`, `skills`) rather than from memory. See the accompanying validation report for exact field-by-field conformance and any fields that were intentionally omitted.
