---
name: run
description: Playbook for cost-tiered multi-agent delegation. An expensive instructor model (Fable/Opus) only decomposes tasks and writes Workflow scripts; execution is delegated to a cheap tier — light-class implementation (resolved by `agent-exec route`, which prefers a ready external executor like Copilot over Claude Haiku), then a Sonnet review pass running adversarial checks, then feedback-driven retries on failure — and only structured verdicts flow back to the instructor. Invoke explicitly with /run (or, cross-plugin, `orchestra:run`), or whenever cost-tiered delegation or large parallel task execution is called for. Also invoked as the ORCHESTRATED lane by the <orchestra-router> protocol this plugin injects at SessionStart.
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

- No verification pipeline (no orchestra-review). Delegate to ONE disposable implementer at the `light` or `standard` class — prefer resolving the executor via `agent-exec route`/`dispatch` (§5's `dispatchClass()`) over assuming a Claude model; fall back to explicit `model: 'haiku'`/`'sonnet'` only when that helper isn't wired into this Workflow. Or handle it directly yourself (subject to your usual direct-edit criteria, e.g. CLAUDE.md rules). The instructor reviews the result itself. At most one express task runs at a time.
- The justification for skipping verification is the same as the conclusion of section 7: skipping adversarial review is only acceptable for tasks whose verification is completely self-evident and low-risk. The express criteria exist precisely to carve out that subset.

**Abort rule:** the moment scope or success criteria turn out to move mid-flight (decomposition became necessary, a design decision surfaced, the blast radius is wider than assumed), **abort express immediately** and re-route to the orchestrated lane, carrying over the state so far. Never push through on express.

## 3. Model tiers

| Tier | Resolved via | Role |
|---|---|---|
| Instructor | Fable / Opus — fixed, never routed | Decomposition, contracts, script writing, exception judgment ONLY. Never implements or reviews |
| `deep` — design-latitude implementation | `agent-exec route --class deep` — Opus (`orchestra-deep`) by design; Codex Sol/xhigh only if Claude itself is unavailable | Implementation where the spec leaves real design latitude: algorithm choice, API shape, tradeoffs |
| `standard` implementation / judgment-based `review` | `agent-exec route --class standard` — Copilot `gpt-5.6-luna`/medium by default, Sonnet if unavailable | Ordinary implementation, or review requiring adversarial test design and failure interpretation |
| `light` implementation / fully-scripted `review` | `agent-exec route --class light` — Copilot `gpt-5.6-luna`/medium by default, Haiku if unavailable | Implementation or review whose procedure is 100% prescribed |

**Review class selection:** if the review procedure is fully prescribed (exact commands and pass criteria given), the `light` class suffices. If failure interpretation, adversarial test design, or spec-ambiguity judgment is needed, use the `standard` class — this is the default review class. For review requiring heavy design judgment, use the `deep` class (pass `model: 'opus'` inline) — there is deliberately no separate `deep`-class review agent definition. The one exception left permanently un-routed is the Workflow template's own same-run adversarial `review` *stage* (§5): `priority.review` is `[claude]`-only by design, so it stays pinned to Sonnet rather than calling through `agent-exec route` — see §5's note on why.

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
  description: 'Light-class implementation for each task (executor resolved by agent-exec route/dispatch - Copilot by default, Haiku as fallback), a Sonnet review pass adversarially checks the result, failures retry with precise feedback up to 3 rounds, and only structured pass/fail verdicts are returned.',
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

// Sticky exhaustion for this run: an executor found `unavailable` stays
// skipped for every remaining task/retry (§9). This Set is the instructor's
// ONLY manual job in selection - the walk itself runs inside `agent-exec
// route`/`dispatch`, never in this script's own logic.
const exhausted = new Set()

// Dispatch one task at a capability class ('light' | 'standard' | 'deep').
// Selection is made by `agent-exec route`, NOT by this function and NOT by
// the instructor. On a Copilot-equipped machine this resolves to
// gpt-5.6-luna/medium; on a plain Claude Code install it resolves to the
// matching Claude tier (haiku/sonnet/opus) automatically - same call shape
// either way, so nothing here needs to know which one it got.
async function dispatchClass(cls, promptText, opts = {}) {
  const relayPrompt =
    'Write the task text below to a new temp file, then run `agent-exec dispatch --class ' + cls +
    (opts.archetype ? ' --archetype ' + opts.archetype : '') +
    (exhausted.size ? ' --exhausted ' + [...exhausted].join(',') : '') +
    ' --workdir ' + (opts.workdir || '.') + ' --prompt-file <that path> --capture`, ' +
    'then print its stdout JSON verbatim - nothing else.\n\n--- TASK ---\n' + promptText

  const raw = await agent(relayPrompt, { label: (opts.label || cls) + '-dispatch', model: 'haiku', effort: 'low' })
  const r = JSON.parse(raw)

  if (r.status === 'ok') return r.answer // a CLI executor (e.g. Copilot) already ran the task.
  if (r.status === 'delegate') {
    // route picked Claude, or an agent-dispatch executor (e.g. Codex) - only
    // the instructor's own agent() call can actually spawn either one, so ITS
    // unavailability (agent() returning null - §9's Claude/Codex signal) is on
    // us to detect and feed back into `exhausted`, same as the branch below.
    const answer = r.agent_type
      ? await agent(promptText, { label: opts.label || cls, agentType: r.agent_type })
      : await agent(promptText, { label: opts.label || cls, model: r.model, effort: r.effort })
    if (answer === null) {
      exhausted.add(r.executor)
      return dispatchClass(cls, promptText, opts)
    }
    return answer
  }
  if (r.status === 'unavailable') {
    exhausted.add(r.executor) // sticky for the rest of THIS run - never re-probed
    return dispatchClass(cls, promptText, opts)
  }
  throw new Error('agent-exec dispatch: unroutable for class ' + cls + ' - ' + JSON.stringify(r.route))
}

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

    // Light tier: cheap, mechanical, literal implementation. Executor choice
    // is `agent-exec route`'s job (see dispatchClass above), never this
    // line's - swap 'light' for 'standard' here for tasks with modest
    // design latitude (see the note below the template for 'deep').
    await dispatchClass('light', workerPrompt, { label: task.id + '-work-' + attempt, workdir: task.workdir })

    // Review tier: adversarial, structured verdict. Stays pinned to Sonnet,
    // unrouted - `priority.review` is `[claude]`-only by design (§9), so
    // sending it through dispatchClass would add a relay hop for a class
    // that only ever resolves to Claude anyway.
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

**On `agentType`:** this plugin ships `agents/orchestra-light.md`, `agents/orchestra-deep.md`, and `agents/orchestra-review.md`. These matter only on `dispatchClass`'s fallback branch (`status: 'delegate'` with no `agent_type`, i.e. Claude was the routed candidate) or when writing the express lane / §8 fallback pattern by hand — whether the plugin-scoped names (e.g. `orchestra:orchestra-light`) resolve as the `agentType` option of `agent()` is environment-dependent and unconfirmed. Before relying on it, check the list of available subagents (the @-mention typeahead, or the names visible to the Agent tool); if `orchestra:orchestra-light` / `orchestra:orchestra-deep` / `orchestra:orchestra-review` resolve, pass e.g. `agentType: 'orchestra:orchestra-light'`. If they don't resolve, fall back further to explicit `model: 'haiku'` / `'opus'` / `'sonnet'`. Either way, rule #4 stands: exactly one of `model` or `agentType` must always be explicit — `dispatchClass`'s own branches already guarantee this on the routed path. The `standard` class has no dedicated Claude agent definition — its Claude-side fallback is `model: 'sonnet'` inline instead of an `agentType`.

For tasks with modest design latitude, call `dispatchClass('standard', workerPrompt, opts)` instead of `'light'` — same helper; `agent-exec route --class standard` resolves it (Copilot Luna by default, Sonnet otherwise). For tasks with real design latitude, route the work stage to `orchestra-deep` (Opus) instead: `dispatchClass('deep', workerPrompt, opts)`, or, on the fallback branch, `agentType: 'orchestra:orchestra-deep'` / `model: 'opus'` directly.

### 5.1 Shorten the per-task critical path — overlap authoring

`pipeline()` already runs tasks concurrently, so the latency you actually feel is not "tasks aren't parallel" — it is (a) the **sequential `work → verify → retry` chain inside a single task**, and (b) **barriers that serialize work with no real dependency** (§5.2). This subsection cuts (a).

**Overlap adversarial-test authoring with implementation, and author once.** §7 measured that the review pass's dominant cost is *authoring* adversarial tests — and those tests derive from the **spec, not the implementation** (the `formatBytes(1048575) → "1 MiB"` boundary test that caught the PoC bug is fully spec-derivable). So author them *concurrently with* the first implementation, and author them **once** — the spec doesn't change across retries, only the implementation does. Each verify step then merely *runs* the pre-authored tests plus a whitebox glance, which is cheap. The pre-authored tests are exactly `orchestra-review`'s "≥3 additional adversarial tests"; only their authoring moves earlier.

This restructures `runTask` (author-tests worker owns the `tests/` paths, impl worker owns the `src/` paths — disjoint, per the same-tree safety rule above):

```javascript
// (dispatchClass() and the module-level `exhausted` Set are defined in §5 above -
// this restructuring only changes runTask, not the rest of the script.)
async function runTask(task) {
  // Author adversarial tests from the SPEC, concurrently with the first
  // implementation. Disjoint paths (tests/ vs src/), so no write conflict.
  // The test-authoring side stays pinned to Sonnet, same reasoning as the
  // review stage below (`priority.review` is `[claude]`-only, §9).
  await parallel([
    () => dispatchClass('light', task.workerPrompt, { label: task.id + '-work-1', workdir: task.workdir }),
    () => agent(task.authorTestsPrompt, { label: task.id + '-authtests', model: 'sonnet' }),
  ])

  let feedback = null
  for (let attempt = 1; attempt <= MAX_RETRIES; attempt++) {
    if (feedback) {
      // Re-implement only. Tests are already on disk from the concurrent
      // author step and are NOT re-written across retries.
      await dispatchClass(
        'light',
        task.workerPrompt +
          '\n\nThis is retry ' + attempt + ' of ' + MAX_RETRIES + '. Your previous ' +
          'attempt already wrote source files at the paths you used before. Read them ' +
          'first, then apply this feedback exactly, changing only what it names:\n' +
          JSON.stringify(feedback),
        { label: task.id + '-work-' + attempt, workdir: task.workdir },
      )
    }
    // Verify = RUN the pre-authored tests + whitebox glance. No re-authoring.
    // Unrouted, same as §5's review stage and for the same reason.
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
- **Do NOT compress worker/verifier *output*.** It is already lean: the verdict is forced into `VERDICT_SCHEMA` JSON and code/log/diff pasting is already forbidden (§6-3). The review pass's main output is adversarial *test code*, which does not compress. Light-class workers (Copilot Luna/medium, or Haiku when routed there as fallback) think little at their cheapest effort setting, so the thinking-inflation risk is low there — but never ask the Sonnet reviewer to write tersely at the cost of the tests it authors.

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
       └─ orchestra-delegate itself runs `agent-exec dispatch --class light --capture` per round
            (no relay needed - it already has Bash) - Copilot by default, orchestra-light/haiku
            only on that call's own "delegate" fallback, never launched directly
       └─ orchestra-delegate internally launches orchestra-review (sonnet) via the Agent tool
       └─ on FAIL, orchestra-delegate re-dispatches with feedback (cap: 3 rounds)
  └─ instructor receives only the structured verdict from orchestra-delegate
```

When rework needs multiple rounds of back-and-forth, do not spawn a fresh `orchestra-delegate` each time — resume the same instance with `SendMessage` so it keeps its context (previous failures, approaches already tried) without re-explanation.

## 9. Configuration file and external executors

At the start of every orchestration, the instructor resolves configuration from up to four layers, merged in this order (later layers win):

1. **Defaults**: `tiers` — light: haiku, standard: sonnet, deep: opus, review: sonnet (unchanged — these remain the Claude-side fallback models `agent-exec route` resolves to). `external_executors.copilot` and `external_executors.codex` now ship **`enabled: true`** out of the box, each with its `class_policy` (Copilot: `gpt-5.6-luna`/medium for `light`+`standard`; Codex: `gpt-5.6-luna`/medium `standard`, `gpt-5.6-sol`/xhigh `deep`, `gpt-5.6-sol`/low `review`) and a built-in `priority` (`light: [copilot, claude]`, `standard: [copilot, claude, codex]`, `deep: [claude, codex]`, `review: [claude]`, `independent-review: [codex]`), plus `enforcement.light_class: off`. Shipping external executors enabled by default is safe because `agent-exec route` gates every candidate on actual availability (below) — on a machine with neither CLI installed, every call still resolves to `claude`.
2. **User**: `~/.claude/orchestra.yaml` (or `.yml`), if present.
3. **Project**: `.claude/orchestra.yaml` (or `.yml`), if present — checked into git, shared with the team.
4. **Project-local**: `.claude/orchestra.local.yaml` (or `.yml`), if present — this developer's personal override for this one project. Mirrors Claude Code's own `settings.json` / `settings.local.json` split; never commit this file (see `setup` SKILL.md §1).

This is a **deep merge**, not a first-found-wins lookup: object/mapping keys are merged recursively key-by-key, so a project file only needs to state the keys it actually wants to change — e.g. a project override of just `external_executors.copilot.enabled: true` inherits everything else (codex's whole block, copilot's `class_policy`, etc.) from the user config or defaults. Scalars and arrays are replaced wholesale by the more specific layer, not concatenated or element-merged. An explicit `null`/`~` is a value, not "reset to default" — omit the key entirely to inherit.

**Upgrade trap for existing configs:** the key-by-key merge means a *partial* `priority` override does not insulate the untouched classes from new built-in defaults. A config written before v0.11.0 that overrides only, say, `priority.deep` (to add Codex there) will, after upgrading, silently inherit the *entire new* `priority.light`/`priority.standard` maps from the v0.11.0 defaults — i.e. the new copilot-first ordering — because those keys were never stated in the user's own file and so were never "theirs" to keep frozen. If you want to keep pre-v0.11.0 behavior for a class you didn't mean to touch, state that class's `priority` list explicitly in your own config, or set `external_executors.copilot.enabled: false` to opt out of external executors altogether.

The format is YAML, not JSON, specifically so the file can carry comments (JSON can't). `.claude/orchestra.json` / `~/.claude/orchestra.json` (the pre-YAML format) are no longer read — use the `setup` skill (`orchestra:setup`) to convert an old one.

Instead of merging these four layers in-context, the instructor can obtain the already-resolved configuration deterministically via **`agent-exec config [--json]`** — it deep-merges the same four layers with the same precedence/merge rules described above and additionally reports, per `external_executors.<name>` with `enabled: true` and `dispatch: cli`, whether its executable resolves on `PATH` (`"available": true/false`, or `null` if the name has no built-in profile). This keeps the merge logic out of the instructor's own context. **Prefer a single startup call, though:** `agent-exec doctor --json` (needed anyway for the `dispatch: cli` pre-flight below) now embeds this exact resolved config under `config.values` alongside its readiness report, so one `doctor` call yields both the `ready.<executor>.ok` verdicts *and* the resolved `tiers` / `external_executors` / `priority` — a standalone `config` call is only worth it when you want the config and nothing else. Likewise, Copilot's `dispatch: cli` invocation may use **`agent-exec run copilot --model M --effort E --workdir W --prompt-file F [--resume SID] [--capture]`** as a normalized entry point that centralizes the `--disable-builtin-mcps`/`--add-dir`/`--output-format` conventions instead of assembling the raw `agent-exec copilot ...` command by hand each time; with `--capture`, it also does what the relay used to hand-parse: it runs copilot as a subprocess and prints one normalized `{ status, answer, session_id, reason, exit_code }` JSON object to stdout instead of handing off via `execvpe`, so the relay just reads that JSON — see `references/external-executors.md` §5 for details.

**`agent-exec route` / `agent-exec dispatch`: the walk itself is code, not instructor judgment.** `agent-exec route --class <light|standard|deep|review|independent-review> [--archetype default|investigation] [--exhausted a,b] [--json|--text]` runs the entire `priority` walk described below in one call: it deep-merges the four config layers, then hard-gates each candidate on reality — dropping it if `enabled: false`, its binary/agent doesn't resolve, `doctor`'s own `ready.<x>.ok` is false, it has no `class_policy` entry for the class, or the caller listed it in `--exhausted` — and returns the surviving top candidate (`executor`, `dispatch`, `model`, `effort`, `agent_type`) plus the full `candidates`/`remaining`/`skipped` trail. The pre-flight `doctor` check described above is folded into this call; it is no longer a separate step the instructor performs before building a `priority` candidate list. `agent-exec dispatch --class <cls> [--archetype A] [--exhausted a,b] --prompt-file F --workdir W [--resume SID] [--capture]` goes one step further: it calls `route` internally, and if the winning candidate is a `dispatch: cli` executor it also runs it, returning `{status: 'ok'|'unavailable', answer, session_id, reason, exit_code, executor, model, effort, route}`; if the winner is Claude or a `dispatch: agent` executor (Codex), it returns `{status: 'delegate', executor, model, effort, agent_type, route}` instead, since only the instructor's own `agent()`/Agent-tool call can spawn a Claude or plugin subagent. With nothing viable it returns `{status: 'unroutable', route}`. This is exactly what §5's `dispatchClass()` helper wraps: **the instructor never decides which executor runs a `light`/`standard` task — it asks, via one relay agent, and branches on the answer.** This availability gate is exactly what makes shipping external executors `enabled: true` by default safe (point 1 above).

As of v0.4.0, the configuration vocabulary itself was renamed from role names to capability/performance classes: `tiers.worker/hard_worker/verifier` → `tiers.light/standard/deep` + `review`, `external_executors.*.roles` → `classes`, `model_policy` → `class_policy`, `role_priority` → `priority`, and `long_context_escalation.{model, effort}` → `long_context_escalation.{class}`. Pre-0.4 keys using the old role vocabulary are no longer read — use the `setup` skill (`orchestra:setup`) to convert an old config to the new vocabulary, the same way it converts legacy JSON to YAML.

A copy of the schema, fully commented, ships with this plugin at `examples/orchestra.yaml`. Full shape:

```yaml
tiers:
  # implementation capability classes (cheapest/fastest -> hardest). These
  # are the CLAUDE-side fallback models `agent-exec route` resolves to when
  # no external executor is enabled/ready for the class - see `priority`
  # below for what actually gets tried first.
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
    enabled: true  # ships true by default; gated on binary+doctor readiness, see §9 below
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

# copilot ships `enabled: true` above, so this list actually prefers it
# when ready - `agent-exec route`/`dispatch` execute the walk, not the
# instructor (see the route/dispatch paragraph above).
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

# Opt-in nudge, never a hard wall - see the `enforcement.light_class`
# paragraph below for the escape hatches. Ships "off". NOTE: quote the value -
# YAML 1.1 parses a bareword `off` as boolean False, not the string "off".
enforcement:
  light_class: "off"
```

To set this up interactively (detect whether Codex/Copilot are actually available in this environment, choose project vs user scope, edit the file in place without clobbering existing comments), use the `setup` skill instead of hand-editing — see its own SKILL.md.

**`tiers`** overrides the model-class defaults of section 3. Values are model aliases (or full model IDs) used whenever this skill — or `agent-exec route` — resolves a class/role to a Claude model: the `claude` candidate in `priority`, or any class/role with no external executor configured for it.

**`external_executors`** declares non-Claude executors (Codex, Copilot, etc.) that may be woven into the pipeline. Only entries with `"enabled": true` are used. Two dispatch mechanisms:

- **`dispatch: "agent"`** — the executor is an installed plugin subagent. Pass `agent_type`'s value as `agentType` in Workflow `agent()` calls, or as `subagent_type` in the Agent tool. If the name doesn't resolve in this environment, fall back to the normal model tier for that class/role.
- **`dispatch: "cli"`** — the executor is a non-interactive CLI (Copilot today). The instructor does not assemble the raw command, write a temp file by hand, or read `settings.json`/run `which` itself: **one call** — `dispatchClass()` (§5), which spawns **one cheap relay agent** (`model: 'haiku'`, with Bash) whose entire job is to run `agent-exec dispatch --class <cls> ... --prompt-file F --workdir W --capture` and hand back its JSON verbatim. `dispatch` calls `route` internally, so the `ready.<executor>.ok` gate — backed by `permission.found`, `shim.installed`, `shim.on_path`, and the executor's own `enabled`/`available` state (see `references/external-executors.md` §4 for the report shape) — has already been applied before the CLI ever runs; an unauthorized/unavailable candidate is simply never selected. The CLI command must still be allowlisted in Claude Code's Bash permissions — a single `Bash(agent-exec:*)` rule (or `Bash(copilot:*)` for the no-wrapper manual path) — since an un-allowlisted call is denied outright before it can even reach `route`'s gating; `agent-exec install` (or the `setup` skill) sets this up. See `references/external-executors.md` §2/§5 for the exact recipe, session-resume flow, and Copilot-specific setup notes.
  **Not a security boundary (M2):** `copilot -p` is not reliably confinable by its own tool-permission flags — excluding a named tool (e.g. `--excluded-tools=bash`) has been observed to be routed around via the `task` tool rather than actually blocked. `--allow-tool`/`--deny-tool`/`--excluded-tools` are defense-in-depth at best, never treat them as a containment guarantee; real containment needs an external OS sandbox (container, `sandbox-exec`, restricted user, network egress control) or a disposable worktree, which this plugin does not implement — treat any `dispatch: cli` Copilot task as an autonomous, directory-scoped agent, and prefer a disposable workdir (a scratch clone or worktree it can freely write/execute in) over the user's primary working tree when the task doesn't need to persist there. A `--deny-tool` list may still be set by the user as an optional, explicitly-advisory knob, but document it to them as narrowing the *named* surface only, not as a boundary.

**`classes`** controls where the executor is used:
- `"light"`: as a light-class implementer (in place of, or alongside, the Claude `light` tier).
- `"standard"`: as a standard-class implementer (in place of, or alongside, the Claude `standard` tier / `model: 'sonnet'`).
- `"deep"`: as a design-latitude implementation class (in place of, or alongside, `orchestra-deep`/Opus) — only meaningful for an executor whose `class_policy` names a model strong enough for that class (see the Codex policy below; Copilot's shipped example intentionally omits this class — see the Copilot section).
- `"review"`: as the same-run adversarial review pass (in place of, or alongside, the Claude `review` tier / `orchestra-review`).
- `"independent-review"`: as a third-party review pass in addition to the Claude review — useful to avoid single-provider model bias. An independent review supplements `orchestra-review`; it does not replace the structured-verdict contract, so wrap its output into the same verdict shape. Because an external, CLI-backed reviewer may ignore the Workflow `schema:` option, force JSON in the prompt and normalize the reply with a tolerant parser — see `references/external-executors.md` §4 (`parseExternalVerdict`).

**`class_policy`** maps each class this executor participates in to a concrete `{ model, effort }` pair to pass to that executor. Without it, the executor runs on its own default model, which defeats the purpose of cost-tiering by external provider just as surely as an unpinned Claude `agent()` call does (rule #4, section 4). Treat this the same way: every external-executor class in `classes` should have a matching `class_policy` entry.

**`priority`** declares, per class/role (and, for `light`, per task archetype — `investigation` vs `default`), an ordered candidate list to try — this list is still the single source of truth for preference order. What changed is *who walks it*: **`agent-exec route`/`dispatch` execute the walk**, not the instructor. `route` tries candidates left-to-right and drops one only on a **reactive fallback signal** — the executor is *unavailable*: `enabled: false`, its binary/agent doesn't resolve, `doctor`'s `ready.<x>.ok` is false, it has no `class_policy` entry for the class, or the caller passed it in `--exhausted`. This is the crucial **unavailable-vs-failed** distinction: a task that runs but returns a wrong result is NOT a fallback signal — it stays on the same executor and goes through the normal review/retry loop; only a genuinely unavailable executor (rate-limit/usage-window cap, credit exhaustion, auth failure) makes `route` drop to the next candidate. Once `dispatch` reports an executor `unavailable` for a task, the instructor's one remaining manual job is to **carry that executor forward in `--exhausted` for every subsequent `route`/`dispatch` call in the same run** (§5's `dispatchClass()` does this automatically via its module-level `exhausted` Set) — this is sticky exhaustion, and it is the instructor's only bookkeeping in the whole selection process. `priority` **supersedes `classes` for ordering** when present for a class/role; `classes` remains the legacy fallback when `priority` is absent (`route`/`dispatch` apply this same fallback internally). Operational detail — per-executor unavailable signals and the priority-walk logic `route` implements — lives in `references/external-executors.md`; read it before second-guessing a fallback decision.

**`enforcement.light_class`** (`"off"` default | `"block"` — quote the value; YAML 1.1 parses a bareword `off` as boolean `False`, not the string) is a **nudge with a guaranteed escape, not a hard wall**. When set to `"block"`, a `PreToolUse` hook (`hooks/enforce-router.sh`) fires only when `agent-exec route --class light` reports a non-`claude` executor ready — i.e. only when there's genuinely something better to redirect to — and the call would spawn a **generic** Claude implementer: no `subagent_type` at all, or `subagent_type` in `general-purpose`/`claude`, at a Haiku-class model. **Naming any other specific `subagent_type` is itself the carve-out**: the Agent tool has no per-call tool-restriction parameters — its schema is only `description`/`isolation`/`model`/`prompt`/`run_in_background`/`subagent_type` — so a named agent is how tool access and a specialized system prompt actually get pinned, and Copilot can substitute for neither; `orchestra:orchestra-light` itself stays a deny target, since redirecting it to `agent-exec dispatch` is the whole point. Escape hatches:

1. **Hard cap: one deny per session, full stop.** Not a per-task or per-prompt cap — after the first nudge the hook goes inert for the rest of the session regardless of what's asked next. (A prompt-keyed fingerprint doesn't work here: this plugin's own retry convention appends new feedback text to the prompt every round (§5), which would re-deny every round of the same task — so the bound is session-wide instead.)
2. **Explicit escape marker.** `[orchestra:allow-claude: <reason>]` anywhere in the prompt/description allows immediately, no deny recorded — for a deliberate choice to stay on Claude.
3. **Named-subagent carve-out.** Any call naming a specific `subagent_type` other than `general-purpose`/`claude` is always allowed (see above) — `Explore`, `Plan`, `statusline-setup`, `claude-code-guide`, `orchestra-review`, and any other installed agent all qualify; only a generic/anonymous Haiku-class call (or `orchestra:orchestra-light` itself) is a deny candidate.
4. **Kill switch + fail-open.** `ORCHESTRA_ENFORCEMENT=off` short-circuits to allow, and every uncertainty (`agent-exec` missing, `config` failing/slow, no `python3`, unparseable stdin, anything unexpected) fails open — it never denies on doubt.

Ships **`"off"`** — turning it on is a deliberate opt-in via the `setup` skill or by hand-editing `orchestra.yaml`, not a default behavior change.

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
  "orchestra_version": "0.11.0",
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
