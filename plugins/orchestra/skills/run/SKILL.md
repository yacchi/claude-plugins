---
name: run
description: Playbook for cost-tiered multi-agent delegation. An expensive instructor model (Fable/Opus) only decomposes tasks and writes Workflow scripts; execution is delegated to a cheap tier — light-class (Haiku) implementation, then a Sonnet review pass running adversarial checks, then feedback-driven retries on failure — and only structured verdicts flow back to the instructor. Invoke explicitly with /run (or, cross-plugin, `orchestra:run`), or whenever cost-tiered delegation or large parallel task execution is called for. Also invoked as the ORCHESTRATED lane by the <orchestra-router> protocol this plugin injects at SessionStart.
when_to_use: Use when delegating multiple tasks in parallel to cheap models, when building a light-class-implementation + Sonnet-review adversarial pipeline, or when the instructor (main session) must receive only structured verdicts — never logs, diffs, or intermediate artifacts — in its context.
---

# run: cost-tiered orchestration

The reader of this skill is the **instructor agent** of the main session (an expensive model such as Fable or Opus). This skill defines the instructor's own code of conduct and how to write the Workflow scripts (or, as a fallback, the nested-subagent hierarchy) that carry out execution.

## 1. Instructor code of conduct

What the instructor does and does not do is strictly separated.

**Do:**
- Task decomposition (what can run in parallel, where the dependencies are)
- Contract definition (per-task input/output spec, edge cases, verification command, pass criteria)
- Writing the Workflow script (see the template below), or the orchestra-delegate launch prompt on the fallback path
- Exception judgment on returned verdicts (resolving ambiguity, making design decisions, deciding what to do after retry exhaustion)

**Do NOT:**
- Read, write, or edit implementation files yourself
- Load test logs, diffs, intermediate artifacts, or raw implementation/review responses into your own context
- Execute any task's implementation or verification yourself

The instructor receives exactly one thing per task: a structured verdict (pass/summary/feedback, or the extended form with rounds/reason).

## 2. Express lane

Applying the full pipeline to every request is overkill. Route lightweight tasks to the express lane using the criteria below (ported from the express-lane design of discus0434/customizable-agent-teams).

**A request goes express only when ALL of the following hold:**

1. The essence of the request resolves as **one self-contained change** (or it is conversational, read-only, or a question)
2. **No** decomposition into multiple tasks, **no** design decisions, **no** cross-task coordination
3. Expected context bloat is small: few tool calls, little file reading, a mechanical or localized change

**File count is NOT a criterion.** Incidental doc updates may ride along and the task stays express. Conversely, when decomposition or design judgment is needed — **or whenever you are in doubt** — always hand off to the orchestrated lane (the rest of this skill).

**Express execution shape:**

- No verification pipeline (no orchestra-review). Delegate to ONE disposable implementer (explicit `model: 'haiku'` or `'sonnet'` — i.e. the `light` or `standard` class), or handle it directly yourself (subject to your usual direct-edit criteria, e.g. CLAUDE.md rules). The instructor reviews the result itself. At most one express task runs at a time.
- The justification for skipping verification is the same as the conclusion of section 7: skipping adversarial review is only acceptable for tasks whose verification is completely self-evident and low-risk. The express criteria exist precisely to carve out that subset.

**Abort rule:** the moment scope or success criteria turn out to move mid-flight (decomposition became necessary, a design decision surfaced, the blast radius is wider than assumed), **abort express immediately** and re-route to the orchestrated lane, carrying over the state so far. Never push through on express.

## 3. Model tiers

| Tier | Model | Role |
|---|---|---|
| Instructor | Fable / Opus | Decomposition, contracts, script writing, exception judgment ONLY. Never implements or reviews |
| `deep` — design-latitude implementation | Opus (`orchestra-deep`) | Implementation where the spec leaves real design latitude: algorithm choice, API shape, tradeoffs |
| `standard` implementation / judgment-based `review` | Sonnet | Ordinary implementation, or review requiring adversarial test design and failure interpretation |
| `light` implementation / fully-scripted `review` | Haiku | Implementation or review whose procedure is 100% prescribed |

**Review class selection:** if the review procedure is fully prescribed (exact commands and pass criteria given), the `light` class (Haiku) suffices. If failure interpretation, adversarial test design, or spec-ambiguity judgment is needed, use the `standard` class (Sonnet) — this is the default review class. For review requiring heavy design judgment, use the `deep` class (pass `model: 'opus'` inline) — there is deliberately no separate `deep`-class review agent definition.

These defaults can be overridden per project/user via a configuration file — see section 9.

## 4. The one rule that matters most

> **Omitting `model`/`agentType` in a Workflow `agent()` call or in the Agent tool makes the spawned agent inherit the session model — i.e. the instructor's expensive model.** If the instructor runs on Fable/Opus, an implementation agent whose model you forgot to pin runs at Fable/Opus cost, and the entire cost-tiering becomes pointless.
>
> **Every single agent invocation must set `model` or `agentType` explicitly. No exceptions.**

This is stated in the official docs (`/en/workflows`): "Every agent in a workflow uses your session's model unless the script routes a stage to a different one or the `CLAUDE_CODE_SUBAGENT_MODEL` environment variable is set". The same applies on the subagent (Agent tool) side: omitting `model` in frontmatter defaults to `inherit` (= session model).

## 5. Workflow template

Where Dynamic Workflows (`/en/workflows`) are available, adapt this template per task set. Constraints: `export const meta` must be a pure literal object (no variables, no function calls, no template interpolation), no TypeScript syntax, no nondeterministic calls like `Date.now()` / `Math.random()`. Write plain JavaScript.

```javascript
export const meta = {
  name: 'cost-tiered-pipeline',
  description: 'Light-class (Haiku) implementation for each task, a Sonnet review pass adversarially checks the result, failures retry with precise feedback up to 3 rounds, and only structured pass/fail verdicts are returned.',
}

// --- Verdict schema: forces the review reply into structured JSON ---
const VERDICT_SCHEMA = {
  type: 'object',
  required: ['pass', 'summary'],
  properties: {
    pass: { type: 'boolean' },
    summary: { type: 'string' },
    feedback: {
      type: 'array',
      items: {
        type: 'object',
        required: ['case', 'expected', 'actual'],
        properties: {
          case: { type: 'string' },
          expected: { type: 'string' },
          actual: { type: 'string' },
        },
      },
    },
  },
}

const MAX_RETRIES = 3

// `tasks` comes from the `args` parameter when this workflow is saved and
// re-run. Each task needs: id, workerPrompt (literal spec + edge cases +
// verify command), verifierPrompt (what to re-check + what adversarial
// cases to add on top of the implementation's own tests).
const tasks = (typeof args !== 'undefined' && args && args.tasks) ? args.tasks : []

async function runTask(task) {
  let feedback = null

  for (let attempt = 1; attempt <= MAX_RETRIES; attempt++) {
    const workerPrompt = feedback
      ? task.workerPrompt +
        '\n\nThis is retry ' + attempt + ' of ' + MAX_RETRIES + '. ' +
        'Your previous attempt already wrote files to disk at the paths you used before. ' +
        'Read those files first, then apply this feedback exactly, changing only what it names:\n' +
        JSON.stringify(feedback)
      : task.workerPrompt

    // Light tier: cheap, mechanical, literal implementation.
    // ALWAYS pass model or agentType explicitly - see rule #4 above.
    await agent(workerPrompt, {
      label: task.id + '-work-' + attempt,
      model: 'haiku',
      effort: 'low',
    })

    // Review tier: adversarial, structured verdict.
    const verdict = await agent(task.verifierPrompt, {
      label: task.id + '-verify-' + attempt,
      model: 'sonnet',
      schema: VERDICT_SCHEMA,
    })

    // agent() returns null if the agent is skipped or dies on a terminal
    // error - guard before dereferencing, or the whole task silently drops.
    if (!verdict) {
      return { id: task.id, pass: false, summary: 'review unavailable (skipped or errored)', rounds: attempt }
    }

    if (verdict.pass) {
      return { id: task.id, pass: true, summary: verdict.summary, rounds: attempt }
    }
    feedback = verdict.feedback
  }

  return { id: task.id, pass: false, summary: 'exhausted retries without a pass', rounds: MAX_RETRIES }
}

// pipeline() runs every task through work->verify->retry independently and
// in parallel (up to the runtime's concurrency cap) - task A can already be
// on retry 2 while task B is still on its first verify. No barrier between
// stages, unlike parallel() which waits for everything at once.
const results = await pipeline(tasks, runTask)

return results
```

**At run end:** if telemetry is enabled (check `doctor`'s `config.values.telemetry.enabled`), have a cheap haiku relay agent emit one `run_summary` record via `agent-exec telemetry record --json '...'` — never the instructor itself. If disabled, skip silently. See section 10.

**Same-tree parallelism safety.** `pipeline()` and `parallel()` run their file-changing workers concurrently against the **same working tree** — there is no worktree isolation unless you explicitly pass `isolation: 'worktree'` on the `agent()` call (and that spins up a fresh worktree per agent, so it only pays off for genuinely parallel file mutation; a sequential same-tree phase can't use it at all). Two file-changing workers whose file ownership overlaps will silently corrupt each other's writes. Guard against it two ways: (1) **assign disjoint file ownership up front** — pin each parallel worker's target files in its prompt so no two can touch the same path, and fix shared contracts/types in the prompts too; and (2) **keep workers small and watch the early spawns** — short-lived, narrowly-scoped workers shrink the overlap window, and a quick check that you haven't spawned two workers keyed to the same task/target catches an accidental duplicate before it reaches its write stage, while it's still harmless. When parallel file mutation genuinely can't be partitioned, use `isolation: 'worktree'` and merge afterward; when the phase is really sequential on one tree, run the workers in sequence rather than racing them.

**On `agentType`:** this plugin ships `agents/orchestra-light.md`, `agents/orchestra-deep.md`, and `agents/orchestra-review.md`. Whether the plugin-scoped names (e.g. `orchestra:orchestra-light`) resolve as the `agentType` option of `agent()` is environment-dependent and unconfirmed. Before relying on it, check the list of available subagents (the @-mention typeahead, or the names visible to the Agent tool); if `orchestra:orchestra-light` / `orchestra:orchestra-deep` / `orchestra:orchestra-review` resolve, pass e.g. `agentType: 'orchestra:orchestra-light'`. If they don't resolve, or you must run before confirming, fall back to explicit `model: 'haiku'` / `'opus'` / `'sonnet'` (the template's default). Either way, rule #4 stands: exactly one of `model` or `agentType` must always be explicit. The `standard` class has no dedicated Claude agent definition — dispatch it with `model: 'sonnet'` inline instead of an `agentType`.

For design-latitude tasks, route the work stage to `orchestra-deep` (Opus) instead of the `light`-class agent: `agentType: 'orchestra:orchestra-deep'` or `model: 'opus'`.

### 5.1 Shorten the per-task critical path — overlap authoring

`pipeline()` already runs tasks concurrently, so the latency you actually feel is not "tasks aren't parallel" — it is (a) the **sequential `work → verify → retry` chain inside a single task**, and (b) **barriers that serialize work with no real dependency** (§5.2). This subsection cuts (a).

**Overlap adversarial-test authoring with implementation, and author once.** §7 measured that the review pass's dominant cost is *authoring* adversarial tests — and those tests derive from the **spec, not the implementation** (the `formatBytes(1048575) → "1 MiB"` boundary test that caught the PoC bug is fully spec-derivable). So author them *concurrently with* the first implementation, and author them **once** — the spec doesn't change across retries, only the implementation does. Each verify step then merely *runs* the pre-authored tests plus a whitebox glance, which is cheap. The pre-authored tests are exactly `orchestra-review`'s "≥3 additional adversarial tests"; only their authoring moves earlier.

This restructures `runTask` (author-tests worker owns the `tests/` paths, impl worker owns the `src/` paths — disjoint, per the same-tree safety rule above):

```javascript
async function runTask(task) {
  // Author adversarial tests from the SPEC, concurrently with the first
  // implementation. Disjoint paths (tests/ vs src/), so no write conflict.
  await parallel([
    () => agent(task.workerPrompt,      { label: task.id + '-work-1',   model: 'haiku',  effort: 'low' }),
    () => agent(task.authorTestsPrompt, { label: task.id + '-authtests', model: 'sonnet' }),
  ])

  let feedback = null
  for (let attempt = 1; attempt <= MAX_RETRIES; attempt++) {
    if (feedback) {
      // Re-implement only. Tests are already on disk from the concurrent
      // author step and are NOT re-written across retries.
      await agent(
        task.workerPrompt +
          '\n\nThis is retry ' + attempt + ' of ' + MAX_RETRIES + '. Your previous ' +
          'attempt already wrote source files at the paths you used before. Read them ' +
          'first, then apply this feedback exactly, changing only what it names:\n' +
          JSON.stringify(feedback),
        { label: task.id + '-work-' + attempt, model: 'haiku', effort: 'low' },
      )
    }
    // Verify = RUN the pre-authored tests + whitebox glance. No re-authoring.
    const verdict = await agent(task.runTestsPrompt, {
      label: task.id + '-verify-' + attempt, model: 'sonnet', schema: VERDICT_SCHEMA,
    })
    if (!verdict) return { id: task.id, pass: false, summary: 'review unavailable (skipped or errored)', rounds: attempt }
    if (verdict.pass) return { id: task.id, pass: true, summary: verdict.summary, rounds: attempt }
    feedback = verdict.feedback
  }
  return { id: task.id, pass: false, summary: 'exhausted retries without a pass', rounds: MAX_RETRIES }
}
```

**Trade-off:** this adds one agent (the test author) and thus one spawn's overhead, so it pays off when authoring latency exceeds spawn overhead — true whenever the reviewer writes non-trivial tests, and the win *grows with retry depth* because authoring no longer repeats per round. For a change so small its review is a single obvious assertion, keep the plain §5 `runTask` instead. Split `task.verifierPrompt` into `task.authorTestsPrompt` (write spec-derived adversarial tests to `tests/`, do not run them) and `task.runTestsPrompt` (run the worker's tests **and** the pre-authored `tests/`, whitebox-inspect the diff, return `VERDICT_SCHEMA`).

### 5.2 Never serialize independent work behind a barrier

The most common reason an orchestration "feels sequential" is a barrier — a `parallel()` between phases, or an `await` — placed where there is no real dependency. The runtime does not add these; the instructor does, by writing phase-by-phase code. Guard against it:

- **Default to `pipeline()`, not phase-by-phase `parallel()`.** Writing `const impls = await parallel(tasks.map(work)); const revs = await parallel(impls.map(verify))` forces *every* implementation to finish before *any* verify starts — the single slowest task stalls all reviews. `pipeline(tasks, runTask)` lets each task's verify start the instant *its own* work is done. A barrier between stages is justified ONLY when stage N genuinely needs the whole of stage N-1: dedup/merge across all results, a global early-exit (`0 findings → skip`), or a synthesis that literally reads every task.
- **A "final review" is per-task unless it truly reads all tasks.** If a final check only re-validates task A, it belongs *inside* task A's pipeline chain, not in a global barrier that runs after every task finishes. Reserve a single whole-set barrier for a real cross-task synthesis, and scope it to the minimal set of tasks it actually consumes — not "all of them" by reflex.
- **Independent review passes run concurrently, not one after another.** When a task gets both a Claude `review` and an `independent-review` (§9 — a different provider's eyes), the two share no dependency. Dispatch them together — `await parallel([() => claudeReview(...), () => independentReview(...)])` — and merge the two verdicts, rather than `await`-ing one and then the other.

### 5.3 Size tasks to fill the concurrency width

`pipeline()`/`parallel()` run at most `min(16, cores − 2)` agents at once. Two failure modes waste that width: **too few, too coarse tasks** (three 10-minute tasks can occupy at most three slots — split independent sub-parts into separate pipeline items *when their file ownership is disjoint*), and **too many trivial tasks** (fixed spawn overhead then dominates useful work). Aim for tasks large enough to amortize a spawn yet numerous and independent enough to keep the slots full. Splitting helps *only* when the parts are genuinely independent and touch disjoint paths — a split that introduces a cross-task dependency just re-adds the barrier §5.2 told you to avoid.

## 6. Writing worker prompts

Mandatory requirements when writing each task's `workerPrompt`:

1. **Concretize the spec to literally-implementable level.** Not abstract instructions like "handle boundary values correctly" — enumerate input/output examples. Example: `formatBytes(1048575)` must return `"1 MiB"`, not `"1024 KiB"` (rounding-carry boundary).
2. **State the verification command.** Give a concrete, runnable command the worker can execute itself and confirm fully passes (e.g. `npm test -- formatBytes.test.js`). Without one, the worker can only self-attest, which is not trustworthy.
3. **Constrain the response format.** Explicitly forbid pasting code, logs, and diffs (the `orchestra-light` agent's system prompt already enforces this, but restating it in the prompt is safer when calling `agent()` directly from a Workflow).
4. **On retry, say that the previous files are still on disk.** The template's `workerPrompt` assembly adds this automatically ("Your previous attempt already wrote files to disk... Read those files first"). When writing prompts by hand, always include an equivalent sentence.

### 6.1 Prompt density: compress the scaffolding, never the spec

Worker and verifier prompts are read by cheap models, not humans. They owe nothing to readability, politeness, or a target language — write them terse, imperative, and in **English** (more token-efficient than Japanese for the same content, and the workers need no Japanese). Drop honorifics, hedges, and prose transitions; nominal/telegraphic style is fine. This trims the instructor's authoring output and the worker's input at essentially no risk.

**But this applies only to the scaffolding — never to the contract itself.** The token-compression techniques that circulate for chat replies (caveman/genshijin-style particle-dropping, "essence only") are an *output*-compression trick, and their own authors report the catch: on complex tasks completeness drops and internal thinking tokens balloon (+200–400%), erasing most of the nominal saving. A worker's job *is* the complex-task case, so:

- **Do NOT compress the spec.** The `formatBytes(1048575) → "1 MiB"` boundary example (requirement 1 above) cannot lose a character without losing meaning, and the one example you delete to save tokens is exactly where §7's rounding-carry class of bug hides. Enumerated I/O examples, boundary values, and the verification command are compression-exempt.
- **Reduce variance with structure, not with prose compression.** Terse natural language is *more* ambiguous, not less. When you want to pin down behaviour and kill wording drift, reach for tables, enumerated example rows, and the response `schema` — structure removes ambiguity; dropping particles adds it.
- **Do NOT compress worker/verifier *output*.** It is already lean: the verdict is forced into `VERDICT_SCHEMA` JSON and code/log/diff pasting is already forbidden (§6-3). The review pass's main output is adversarial *test code*, which does not compress. Haiku workers at `effort: low` think little, so the thinking-inflation risk is low there — but never ask the Sonnet reviewer to write tersely at the cost of the tests it authors.

Rule of thumb: **strip everything a human would want and a machine does not; keep every concrete fact the worker must reproduce exactly.**

## 7. Findings proven by the PoC

From the measured PoC (3 tasks in parallel, 8 agents, 4 min 23 s, 246k total subagent tokens):

- **4 `light`-class (Haiku) implementations**: 9,638 output tokens total. **4 Sonnet `review` passes**: 19,135 output tokens total. Review passes consume roughly 2x the implementation's output tokens — the main cost is authoring adversarial tests.
- **The instructor consumed zero tokens during execution.** What came back to the instructor was ~2KB of structured JSON.
- **Review value, demonstrated (formatBytes task):** all 11 of the implementation's self-written tests passed, but they didn't cover boundary values. The review pass's added adversarial test caught a rounding-carry bug (`1048575` → returned `"1024 KiB"`; correct is `"1 MiB"`). One feedback-driven retry fixed it and passed.

Conclusion: adversarial review costs ~2x tokens but catches bugs that the implementation's self-attested tests structurally cannot. Skip review only for tasks whose review procedure is completely self-evident and low-risk.

## 8. Fallback (environments without the Workflow tool)

Where Dynamic Workflows are unavailable or disabled, fall back to a 3-level nesting: launch the `orchestra-delegate` subagent (Sonnet) via the Agent tool:

```
Instructor (Fable/Opus)
  └─ Agent tool: launch orchestra-delegate (no model needed - pinned to sonnet in its own frontmatter)
       └─ orchestra-delegate internally launches orchestra-light (haiku) via the Agent tool
       └─ orchestra-delegate internally launches orchestra-review (sonnet) via the Agent tool
       └─ on FAIL, orchestra-delegate relaunches orchestra-light with feedback (cap: 3 rounds)
  └─ instructor receives only the structured verdict from orchestra-delegate
```

When rework needs multiple rounds of back-and-forth, do not spawn a fresh `orchestra-delegate` each time — resume the same instance with `SendMessage` so it keeps its context (previous failures, approaches already tried) without re-explanation.

## 9. Configuration file and external executors

At the start of every orchestration, the instructor resolves configuration from up to four layers, merged in this order (later layers win):

1. **Defaults**: light: haiku, standard: sonnet, deep: opus, review: sonnet; no external executors.
2. **User**: `~/.claude/orchestra.yaml` (or `.yml`), if present.
3. **Project**: `.claude/orchestra.yaml` (or `.yml`), if present — checked into git, shared with the team.
4. **Project-local**: `.claude/orchestra.local.yaml` (or `.yml`), if present — this developer's personal override for this one project. Mirrors Claude Code's own `settings.json` / `settings.local.json` split; never commit this file (see `setup` SKILL.md §1).

This is a **deep merge**, not a first-found-wins lookup: object/mapping keys are merged recursively key-by-key, so a project file only needs to state the keys it actually wants to change — e.g. a project override of just `external_executors.copilot.enabled: true` inherits everything else (codex's whole block, copilot's `class_policy`, etc.) from the user config or defaults. Scalars and arrays are replaced wholesale by the more specific layer, not concatenated or element-merged. An explicit `null`/`~` is a value, not "reset to default" — omit the key entirely to inherit.

The format is YAML, not JSON, specifically so the file can carry comments (JSON can't). `.claude/orchestra.json` / `~/.claude/orchestra.json` (the pre-YAML format) are no longer read — use the `setup` skill (`orchestra:setup`) to convert an old one.

Instead of merging these four layers in-context, the instructor can obtain the already-resolved configuration deterministically via **`agent-exec config [--json]`** — it deep-merges the same four layers with the same precedence/merge rules described above and additionally reports, per `external_executors.<name>` with `enabled: true` and `dispatch: cli`, whether its executable resolves on `PATH` (`"available": true/false`, or `null` if the name has no built-in profile). This keeps the merge logic out of the instructor's own context. **Prefer a single startup call, though:** `agent-exec doctor --json` (needed anyway for the `dispatch: cli` pre-flight below) now embeds this exact resolved config under `config.values` alongside its readiness report, so one `doctor` call yields both the `ready.<executor>.ok` verdicts *and* the resolved `tiers` / `external_executors` / `priority` — a standalone `config` call is only worth it when you want the config and nothing else. Likewise, Copilot's `dispatch: cli` invocation may use **`agent-exec run copilot --model M --effort E --workdir W --prompt-file F [--resume SID] [--capture]`** as a normalized entry point that centralizes the `--disable-builtin-mcps`/`--add-dir`/`--output-format` conventions instead of assembling the raw `agent-exec copilot ...` command by hand each time; with `--capture`, it also does what the relay used to hand-parse: it runs copilot as a subprocess and prints one normalized `{ status, answer, session_id, reason, exit_code }` JSON object to stdout instead of handing off via `execvpe`, so the relay just reads that JSON — see `references/external-executors.md` §5 for details.

As of v0.4.0, the configuration vocabulary itself was renamed from role names to capability/performance classes: `tiers.worker/hard_worker/verifier` → `tiers.light/standard/deep` + `review`, `external_executors.*.roles` → `classes`, `model_policy` → `class_policy`, `role_priority` → `priority`, and `long_context_escalation.{model, effort}` → `long_context_escalation.{class}`. Pre-0.4 keys using the old role vocabulary are no longer read — use the `setup` skill (`orchestra:setup`) to convert an old config to the new vocabulary, the same way it converts legacy JSON to YAML.

A copy of the schema, fully commented, ships with this plugin at `examples/orchestra.yaml`. Full shape:

```yaml
tiers:
  # implementation capability classes (cheapest/fastest -> hardest)
  light: haiku       # fast, mechanical, fully-specified work
  standard: sonnet   # normal implementation with modest design latitude
  deep: opus         # design-sensitive / hard problems needing real judgment
  # verification role
  review: sonnet     # mandatory same-run adversarial review of the work
  # `independent-review` has no built-in Claude model on purpose - it exists to
  # add a DIFFERENT provider's eyes; see `priority.independent-review` below.

external_executors:
  codex:
    enabled: true
    dispatch: agent
    agent_type: codex:codex-rescue
    classes:
      - standard
      - deep
      - review
    class_policy:
      standard:
        model: gpt-5.6-luna
        effort: medium
      deep:
        model: gpt-5.6-sol
        effort: xhigh
      review:
        model: gpt-5.6-sol
        effort: low
    long_context_escalation:
      when: task requires deep traversal of a large repo (Luna's long-context recall is weak)
      class: deep

  copilot:
    enabled: false
    dispatch: cli
    # dispatched via the agent-exec wrapper: copilot -p 1.0.74 runs file/shell/
    # network tools autonomously in non-interactive mode without any allow-all
    # flag or env var, so agent-exec injects nothing — the only permission
    # rule needed is Bash(agent-exec:*). --disable-builtin-mcps drops the
    # builtin github-mcp-server/customize-cloud-agent tools from the surface
    # (lower token/latency cost); --add-dir scopes the filesystem to workdir.
    # (see the dispatch: "cli" notes below, and the M2 sandboxing caveat)
    command: >-
      agent-exec copilot -p {promptfile} --model {model} --effort {effort}
      --add-dir {workdir} --output-format json --disable-builtin-mcps
    resume_command: >-
      agent-exec copilot --resume={session_id} -p {promptfile} --model {model} --effort {effort}
      --add-dir {workdir} --output-format json --disable-builtin-mcps
    classes:
      - light
      - standard
    class_policy:
      light:
        model: gpt-5.6-luna
        effort: medium
      standard:
        model: gpt-5.6-luna
        effort: medium

priority:
  light:
    investigation: [copilot, claude]
    default: [copilot, claude]
  standard:
    default: [copilot, claude, codex]
  deep:
    default: [claude, codex]
  review:
    default: [claude]
  independent-review:
    default: [codex]
```

To set this up interactively (detect whether Codex/Copilot are actually available in this environment, choose project vs user scope, edit the file in place without clobbering existing comments), use the `setup` skill instead of hand-editing — see its own SKILL.md.

**`tiers`** overrides the model-class defaults of section 3. Values are model aliases (or full model IDs) used wherever this skill says haiku/opus/sonnet for the respective class or role.

**`external_executors`** declares non-Claude executors (Codex, Copilot, etc.) that may be woven into the pipeline. Only entries with `"enabled": true` are used. Two dispatch mechanisms:

- **`dispatch: "agent"`** — the executor is an installed plugin subagent. Pass `agent_type`'s value as `agentType` in Workflow `agent()` calls, or as `subagent_type` in the Agent tool. If the name doesn't resolve in this environment, fall back to the normal model tier for that class/role.
- **`dispatch: "cli"`** — the executor is a non-interactive CLI. Write the task prompt to a temp file, substitute `{promptfile}` (and, if the config declares them, `{model}`/`{effort}`/`{workdir}`/`{session_id}`) in `command`, and have a **cheap relay agent** (`model: haiku`, with Bash) run the command and return only its final output. The instructor must never run the CLI via Bash itself — the relay exists to keep CLI stdout/stderr out of the instructor's context. For Copilot, the command is `agent-exec run copilot --capture ...`: it parses the CLI's JSONL itself and hands the relay one normalized `{ status, answer, session_id, reason }` JSON object, so the relay no longer parses JSONL or judges ok/unavailable by hand — it just reads that JSON and echoes it into the `STATUS:` reply (see `references/external-executors.md` §2, §5). **Because the relay runs non-interactively, the CLI command must already be allowlisted in Claude Code's Bash permissions** — an un-allowlisted call is denied outright (a subagent can't surface an approval prompt at call time), so the dispatch fails before the CLI ever runs. **Setup for Copilot: install the `agent-exec` wrapper** (via the `setup` skill, or `agent-exec install` directly). Measurement on Copilot CLI 1.0.74 found `copilot -p` runs file/shell/network tool calls autonomously in non-interactive mode with no allow-all flag or env var needed, so `agent-exec` injects nothing — the only permission rule needed is a single `Bash(agent-exec:*)` in `permissions.allow` (project `.claude/settings.json` or user `~/.claude/settings.json`, matching the scope where you enabled Copilot). The shipped command templates dispatch through it (`agent-exec copilot ... --add-dir {workdir} --output-format json --disable-builtin-mcps`, see the example config above): `--add-dir` scopes the filesystem, `--disable-builtin-mcps` drops the builtin `github-mcp-server`/`customize-cloud-agent` tools from the surface (lower token/latency cost). There is no `COPILOT_ALLOW_ALL` env var and no manual `Bash(copilot:*)` fallback path anymore — `Bash(agent-exec:*)` is the single authorization rule. Codex's `dispatch: "agent"` needs none of this, since it routes through a subagent rather than a raw Bash command. Treat that authorization as a **pre-flight availability check**, but determine it with a single call rather than separate reads: before including a `dispatch: cli` executor in a `priority` candidate list, the instructor runs **`agent-exec doctor --json`** once and reads `ready.<executor>.ok` (backed by `permission.found`, `shim.installed`, `shim.on_path`, and the executor's own `enabled`/`available` state — see `references/external-executors.md` §4 for the report shape) instead of separately reading `settings.json` and running `which` per executor. If `ready.<executor>.ok` is false, drop it to a later candidate (or out) for this run — an unauthorized/unavailable cli executor can never succeed here, so discovering it through a failed dispatch merely wastes a relay round (the relay's `STATUS: unavailable` stays as the runtime backstop). `doctor`'s permission scan is best-effort (it does not see enterprise policy or CLI `--allowedTools`), so a `false` there means "not confirmed," not "definitely absent" — this pre-flight call is a cheap local check and is explicitly exempt from the "no proactive quota probing" rule (quota is transient and remote; this readiness check is static and local).
  **Not a security boundary (M2):** `copilot -p` is not reliably confinable by its own tool-permission flags — excluding a named tool (e.g. `--excluded-tools=bash`) has been observed to be routed around via the `task` tool rather than actually blocked. `--allow-tool`/`--deny-tool`/`--excluded-tools` are defense-in-depth at best, never treat them as a containment guarantee; real containment needs an external OS sandbox (container, `sandbox-exec`, restricted user, network egress control) or a disposable worktree, which this plugin does not implement — treat any `dispatch: cli` Copilot task as an autonomous, directory-scoped agent, and prefer a disposable workdir (a scratch clone or worktree it can freely write/execute in) over the user's primary working tree when the task doesn't need to persist there. A `--deny-tool` list may still be set by the user as an optional, explicitly-advisory knob, but document it to them as narrowing the *named* surface only, not as a boundary.

**`classes`** controls where the executor is used:
- `"light"`: as a light-class implementer (in place of, or alongside, the Claude `light` tier).
- `"standard"`: as a standard-class implementer (in place of, or alongside, the Claude `standard` tier / `model: 'sonnet'`).
- `"deep"`: as a design-latitude implementation class (in place of, or alongside, `orchestra-deep`/Opus) — only meaningful for an executor whose `class_policy` names a model strong enough for that class (see the Codex policy below; Copilot's shipped example intentionally omits this class — see the Copilot section).
- `"review"`: as the same-run adversarial review pass (in place of, or alongside, the Claude `review` tier / `orchestra-review`).
- `"independent-review"`: as a third-party review pass in addition to the Claude review — useful to avoid single-provider model bias. An independent review supplements `orchestra-review`; it does not replace the structured-verdict contract, so wrap its output into the same verdict shape. Because an external, CLI-backed reviewer may ignore the Workflow `schema:` option, force JSON in the prompt and normalize the reply with a tolerant parser — see `references/external-executors.md` §4 (`parseExternalVerdict`).

**`class_policy`** maps each class this executor participates in to a concrete `{ model, effort }` pair to pass to that executor. Without it, the executor runs on its own default model, which defeats the purpose of cost-tiering by external provider just as surely as an unpinned Claude `agent()` call does (rule #4, section 4). Treat this the same way: every external-executor class in `classes` should have a matching `class_policy` entry.

**`priority`** declares, per class/role (and, for `light`, per task archetype — `investigation` vs `default`), an ordered candidate list to try. Each entry is an `external_executors` key or the `claude` sentinel (the built-in `tiers.<class>` model). The instructor tries candidates left-to-right and drops to the next one only on a **reactive fallback signal**: the current executor is *unavailable* — a rate-limit/usage-window cap (Claude, Codex), credit exhaustion (Copilot), `enabled: false`, an agent/CLI that didn't resolve, or a missing `class_policy` entry. This is the crucial **unavailable-vs-failed** distinction: a task that runs but returns a wrong result is NOT a fallback signal — it stays on the same executor and goes through the normal review/retry loop. Once an executor is found unavailable in a run it stays skipped for every remaining task in that run (**sticky exhaustion**) — no re-probing. `priority` **supersedes `classes` for ordering** when present for a class/role; `classes` remains the legacy fallback when `priority` is absent. Operational detail — per-executor unavailable signals, the Haiku relay's `STATUS:` discriminator, and a JS priority-walk helper — lives in `references/external-executors.md`; read it before implementing fallback logic.

**Safety note:** `cli` dispatch only ever executes command templates that come from the user's own configuration file (`.claude/orchestra.yaml` or `~/.claude/orchestra.yaml`). This plugin ships no CLI commands of its own and must never invent one; if no config file declares a CLI executor, `cli` dispatch is unavailable. The `agent-exec` binary those templates call through is not part of this plugin either — it's a separate tool the user explicitly installs and authorizes via their own `Bash(agent-exec:*)` permission rule (see the `setup` skill); this plugin only ever writes the command template that invokes it, never runs it directly, and never grants it permission on the user's behalf.

### 9.1 External executor model policy (Codex / Copilot), pricing, and CLI usage

Full detail lives in `references/external-executors.md` (Japanese) — Codex's Sol/Terra/Luna + effort policy and class assignment, Copilot's model catalog and `light`/`standard`-class candidates, exact CLI usage recipes (one-shot and session-continuation for retry rounds), and official per-token pricing for Codex/Copilot/Claude. Read it before dispatching to an external executor, and before second-guessing any model/effort choice in the example config.

In brief, as of this plugin's own validation (full reasoning and every round-by-round result: `references/poc-findings.md`):

- **Codex `standard`** → `gpt-5.6-luna` at **`effort: medium`** (not `high` — this plugin's own PoC found `high` produced a real bug on a realistic task that `medium` did not, cheaper and faster besides). **`deep`** → `gpt-5.6-sol`/`xhigh`. **`review`** → `gpt-5.6-sol`/`low` (this is the `class_policy` entry Codex uses when the `priority.independent-review` list dispatches to it). `gpt-5.6-terra` is not in the default policy but hasn't been shown to be dominated either — worth reconsidering for `independent-review` if cost is a concern.
- **Copilot `light`/`standard`** → `gpt-5.6-luna` at `effort: medium`, the fastest/cheapest validated candidate — luna leads the `priority` list for both the `light` and `standard` classes (see `examples/orchestra.yaml`); `kimi-k2.7-code` and the newly-resolved `mai-code-1-flash-picker` are viable alternatives (see the reference doc for what's been observed about each).
- **Long-context caveat:** Luna has measurably weak long-context recall. Escalate a nominally `light`/`standard`-class task to `deep` if it requires deep traversal of a large repository — the `long_context_escalation` field (`{ class: deep }`) in the example config documents this trigger.
- Across 6 rounds of escalating task difficulty, no accuracy differentiation was observed until task size crossed into genuine multi-file, multi-language feature territory — at which point every cheap tier tested eventually showed at least one real, narrow defect. Treat the adversarial review stage as mandatory beyond a trivially small change, regardless of which model/provider/effort level is implementing the work — this holds for external executors exactly as much as for Claude's own tiers.

## 10. Telemetry (opt-in, anonymized)

Orchestra can optionally emit a small, anonymized telemetry stream so the maintainer can see how the pipeline behaves in practice — class usage, executor fallback rates, dispatch outcomes — without ever seeing what any of it was about.

**Default off.** Telemetry is opt-in via `telemetry.enabled: true` in `orchestra.yaml` (see `examples/orchestra.yaml`). The resolved value surfaces at `doctor`'s `config.values.telemetry.enabled`, same as any other config key (section 9) — check that field rather than re-reading the YAML layers yourself. When disabled (the default, and the as-shipped state), nothing is written, and every `telemetry` subcommand that would record data is a silent no-op. To toggle without hand-editing YAML, use `agent-exec telemetry enable [--scope user|project|local]` or `agent-exec telemetry disable [--scope user|project|local]` — these perform a comment-preserving surgical edit of the scope's orchestra.yaml (creating a stub if absent) and require no manual YAML work.

**Crash-dump-style, allowlist-enforced.** Redaction is not a matter of callers behaving — `agent-exec` itself enforces an ALLOWLIST of field names and enumerated values; only enumerated categorical strings and non-negative integers can ever be stored, and `schema_version`/`ts`/`os` are stamped by `agent-exec` itself, never supplied by the caller. It is structurally impossible to record prompts, task text, file names, paths, task ids, `summary` strings, code, or error-message text: no field accepts free text, and enum fields are checked by exact match, so free text placed in an enum field is silently dropped rather than stored.

Allowed fields:
- `event`: `run_summary` | `dispatch`
- `lane`: `express` | `orchestrated`
- `orchestra_version`: semver
- `executor`: `claude` | `copilot` | `codex`
- `cls`: `light` | `standard` | `deep` | `review`
- `status`: `ok` | `unavailable`
- `reason`: `quota` | `rate-limit` | `credits` | `auth` | `nonzero-exit` | `error`
- `resumed`: boolean
- `run_summary`-only numerics: `task_count`, `pass`, `fail`, `exhausted`, `fallbacks`
- `run_summary`-only histograms (dict): `classes`, `rounds`, `executors_used`, `external_enabled`

**Two emission sources:**
- **AUTO — per dispatch.** `agent-exec run ... --capture` self-logs one `dispatch` record after printing its result (status/reason, and `cls` if `--cls` was passed to tag the dispatch's capability class). This is LLM-independent — it happens inside `agent-exec` regardless of who called it, and is a no-op when telemetry is disabled.
- **RUN SUMMARY — once per orchestration.** At run end, if telemetry is enabled (visible via `doctor`'s `config.values.telemetry.enabled`), the instructor emits exactly one `run_summary` record through a cheap **haiku relay agent** that calls `agent-exec telemetry record --json '...'` — never the instructor directly. This keeps the instructor's own context clean, and `record` only accepts categorical/numeric fields regardless of who calls it. Skip silently when disabled.

Example `run_summary` payload — categorical/numeric fields only, no ids or free text:

```json
{
  "event": "run_summary",
  "lane": "orchestrated",
  "orchestra_version": "0.9.0",
  "task_count": 3,
  "pass": 2,
  "fail": 1,
  "exhausted": 1,
  "fallbacks": 1,
  "classes": { "light": 2, "standard": 1 },
  "rounds": { "1": 2, "3": 1 },
  "executors_used": { "claude": 2, "copilot": 1 },
  "external_enabled": { "copilot": 1 }
}
```

**Storage and CLI.** Records accumulate under `telemetry.dir` (default `~/.claude/orchestra/telemetry`, see `examples/orchestra.yaml`). CLI surface:

- **`agent-exec telemetry record (--json STR | --file F)`** — appends ONE sanitized record; no-op + exit 0 when telemetry is disabled; never echoes the record's content to stdout.
- **`agent-exec telemetry show [--json]`** — inspect what's stored.
- **`agent-exec telemetry archive [--out FILE]`** — bundle stored records into a `.tar.gz`.
- **`agent-exec telemetry clear`** — delete stored records.

`show` / `archive` / `clear` work regardless of `enabled` — disabling telemetry only stops new records from being written, it does not hide or lock what is already on disk. `agent-exec run ... --cls CLASS` optionally tags a dispatch with its capability class for the auto-logged record. The Copilot relay's `answer` is never logged, under any circumstance.
