---
name: run
description: Playbook for cost-tiered multi-agent delegation. The instructor (Fable/Opus main session) only decomposes tasks, writes contracts, and writes the Workflow script; implementation goes to a cheap tier resolved by `agent-exec route` (an external executor like Copilot by default, Claude Haiku/Sonnet as fallback), an adversarial review pass checks the result, failures retry with precise feedback, and only structured verdicts flow back. Invoke with /run (cross-plugin: `orchestra:run`), or whenever cost-tiered delegation or large parallel task execution is called for. Also the ORCHESTRATED lane of the <orchestra-router> protocol this plugin injects at SessionStart.
when_to_use: Use when delegating multiple tasks in parallel to cheap models, when building a cheap-implementation + adversarial-review pipeline, or when the instructor must receive only structured verdicts — never logs, diffs, or intermediate artifacts.
---

# run: cost-tiered orchestration

You are the **instructor** — the main session, on an expensive model. This skill is your code of conduct plus the Workflow script you write.

**The loop:** classify (§2) → decompose into tasks with contracts (§6) → write the Workflow script (§5) → receive only structured verdicts. Nothing else enters your context.

**Load a reference file only when its question actually comes up:**

| File | Read it when |
|---|---|
| `references/authoring.md` | Writing the script and you hit: parallel file-mutation safety, `agentType` resolution, per-task latency, prompt density, or the no-Workflow fallback shape. Also holds the PoC numbers behind §7. |
| `references/config.md` | A configuration question actually arises: key semantics, four-layer merge, why `route` dropped a candidate, `enforcement.light_class`, telemetry fields. |
| `references/external-executors.md` | Choosing or second-guessing a Codex/Copilot model+effort, or needing the raw CLI recipe / pricing. |
| `references/poc-findings.md` | Making a model-policy decision that depends on specific benchmark numbers. |

## 1. Instructor code of conduct

**Do:** task decomposition (what parallelizes, where dependencies are) · contract definition (per-task spec, edge cases, verification command, pass criteria) · writing the Workflow script · exception judgment on returned verdicts (ambiguity, design decisions, what to do after retry exhaustion).

**Do NOT:** read/write/edit implementation files · load test logs, diffs, intermediate artifacts, or raw worker/review responses into your context · run any task's implementation or verification yourself.

You receive exactly one thing per task: a structured verdict.

**Carve-outs from "Do NOT":** conversational or read-only replies, your own memory/scratch files, and unblocking actions a sandboxed subagent cannot perform itself (e.g. `git add` for an agent that can't write to `.git`). The express lane (§2) is the other one.

## 2. Express lane

The full pipeline on every request is overkill. **Express only when ALL hold:**

1. The request resolves as **one self-contained change** (or is conversational / read-only / a question)
2. **No** decomposition, **no** design decisions, **no** cross-task coordination
3. Small expected context bloat: few tool calls, little file reading, mechanical or localized change

**File count is NOT a criterion** — incidental doc updates may ride along. When decomposition or design judgment is needed, **or whenever in doubt**, take the orchestrated lane.

**The gate is context bloat, not your model tier.** Being on an expensive model is not itself a reason to delegate: a few-line, single-file, mechanical change with no design judgment is express even on Fable/Opus, because delegating it costs more in prompt authoring, spawn latency, and handoff than it saves.

**Ask "do I already hold the context?" BEFORE judging size.** If the content is already formed in your head from this conversation — a spec you just designed, a fix you already worked out — write it yourself regardless of size or file count. Delegation cannot shrink context you already hold; it only adds overhead and the risk that the worker mis-integrates what you already knew. **Tell-tale: if writing the delegation prompt would require spelling out the full content or spec, you have already authored it — just write it.** Delegation pays off only for work needing exploration, reading, or context you do NOT yet hold.

**Express shape:** no review pipeline. One disposable implementer at `light`/`standard` (prefer `dispatchClass()`, §5; explicit `model: 'haiku'`/`'sonnet'` only when that helper isn't wired in), or handle it yourself per the two gates above. You judge the result — but hand the *verification run* (tests, builds, smoke checks) to a cheap subagent and read back only its verdict rather than letting the output flood your context: Haiku when the procedure is 100% prescribed (exact commands, explicit pass criteria), Sonnet when interpreting failures needs judgment. One express task at a time.

**Abort rule:** the moment scope or success criteria move mid-flight, **abort express immediately** and re-route to orchestrated, carrying over the state so far. Never push through.

## 3. Capability classes

| Class | Resolved via | Role |
|---|---|---|
| Instructor | Fable / Opus — fixed, never routed | Decomposition, contracts, script writing, exception judgment ONLY |
| `deep` | `agent-exec route --class deep` — Opus (`orchestra-deep`); Codex Sol/xhigh only if Claude is unavailable | Real design latitude: algorithm choice, API shape, tradeoffs |
| `standard` | `agent-exec route --class standard` — Copilot `gpt-5.6-luna`/medium by default, Sonnet otherwise | Ordinary implementation; review needing adversarial test design and failure interpretation |
| `light` | `agent-exec route --class light` — Copilot `gpt-5.6-luna`/medium by default, Haiku otherwise | Implementation or review whose procedure is 100% prescribed |

**Review class:** `standard` is the default. `light` suffices only when the review procedure is fully prescribed (exact commands, explicit pass criteria). For review needing heavy design judgment, pass `model: 'opus'` inline — there is deliberately no `deep`-class review agent. The template's own same-run `review` stage stays pinned to Sonnet and unrouted, because `priority.review` is `[claude]`-only by design.

Defaults are overridable per project/user — see `references/config.md`.

## 4. The one rule that matters most

> **Omitting `model`/`agentType` makes the spawned agent inherit the session model — yours.** An implementation agent you forgot to pin runs at Fable/Opus cost and the cost-tiering becomes pointless. This holds for Workflow `agent()` calls and for the Agent tool alike (frontmatter default is `inherit`).
>
> **Every single agent invocation sets `model` or `agentType` explicitly. No exceptions.**
>
> Corollary: the instructor tier is never a worker tier. A file-changing subagent must never run on the instructor's own model — Fable especially, whose role is orchestration plus the small direct edits §2 allows. Pass an explicit Opus/Sonnet/Haiku (or let `agent-exec route` resolve an external executor); never let it inherit.

## 5. Workflow template

Adapt per task set. Constraints: `export const meta` must be a pure literal (no variables/calls/interpolation), no TypeScript syntax, no `Date.now()`/`Math.random()`. Plain JavaScript.

```javascript
export const meta = {
  name: 'cost-tiered-pipeline',
  description: 'Light-class implementation per task (executor resolved by agent-exec route/dispatch - Copilot by default, Haiku as fallback), an adversarial review pass checks the result, failures retry with precise feedback up to 3 rounds, and only structured verdicts return.',
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
// skipped for every remaining task/retry. This Set is your ONLY manual job in
// selection - the priority walk itself runs inside `agent-exec route`.
const exhausted = new Set()

// Dispatch one task at a capability class ('light' | 'standard' | 'deep').
// Selection is `agent-exec route`'s job, not this function's and not yours.
// Same call shape whether it resolves to Copilot or to a Claude tier.
async function dispatchClass(cls, promptText, opts = {}) {
  const relayPrompt =
    'Write the task text below to a new temp file, then run `agent-exec dispatch --class ' + cls +
    (opts.archetype ? ' --archetype ' + opts.archetype : '') +
    (exhausted.size ? ' --exhausted ' + [...exhausted].join(',') : '') +
    ' --workdir ' + (opts.workdir || '.') + ' --prompt-file <that path> --capture`, ' +
    'then print its stdout JSON verbatim - nothing else.\n\n--- TASK ---\n' + promptText

  const raw = await agent(relayPrompt, { label: (opts.label || cls) + '-dispatch', model: 'haiku', effort: 'low' })
  const r = JSON.parse(raw)

  if (r.status === 'ok') return r.answer // a CLI executor (e.g. Copilot) already ran it.
  if (r.status === 'delegate') {
    // route picked Claude or an agent-dispatch executor (e.g. Codex) - only
    // your own agent() call can spawn either, so detecting ITS unavailability
    // (agent() returning null) and feeding it back into `exhausted` is on you.
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

// `tasks` comes from `args` when this workflow is saved and re-run. Each task
// needs: id, workerPrompt (literal spec + edge cases + verify command),
// verifierPrompt (what to re-check + which adversarial cases to add).
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

    // Swap 'light' for 'standard'/'deep' per the task's design latitude (§3).
    await dispatchClass('light', workerPrompt, { label: task.id + '-work-' + attempt, workdir: task.workdir })

    // Review stays pinned to Sonnet and unrouted: `priority.review` is
    // `[claude]`-only, so routing it would just add a relay hop.
    const verdict = await agent(task.verifierPrompt, {
      label: task.id + '-verify-' + attempt,
      model: 'sonnet',
      schema: VERDICT_SCHEMA,
    })

    // agent() returns null when skipped or on a terminal error - guard, or the
    // whole task silently drops.
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

// pipeline() runs every task through work->verify->retry independently and in
// parallel - task A can be on retry 2 while task B is still on its first
// verify. No barrier between stages, unlike parallel().
const results = await pipeline(tasks, runTask)

return results
```

**Same-tree parallelism safety.** `pipeline()`/`parallel()` run file-changing workers concurrently against the **same working tree** — two workers with overlapping file ownership silently corrupt each other. Pin disjoint target files (and shared contracts/types) in each worker's prompt, keep workers small, and use `isolation: 'worktree'` only when parallel mutation genuinely can't be partitioned. Full guidance: `references/authoring.md` §1.

**On `agentType`:** the plugin-scoped names (`orchestra:orchestra-light` / `-deep` / `-review`) may or may not resolve as `agent()`'s `agentType` in this environment — check the available-subagents list before relying on them, and fall back to explicit `model:`. See `references/authoring.md` §1.

**Latency:** for the per-task critical path (overlapping adversarial-test authoring with implementation, avoiding needless barriers, sizing tasks to the concurrency width), see §7 and `references/authoring.md` §2.

**Follow-up rounds:** when a subagent's work needs another pass, resume that same instance with `SendMessage` rather than spawning a fresh one — it still holds what already failed and what it has tried, so nothing needs re-explaining.

**At run end:** if `doctor`'s `config.values.telemetry.enabled` is true, have a cheap haiku relay agent emit one `run_summary` via `agent-exec telemetry record --json '...'` — never yourself. If disabled, skip silently (§10).

## 6. Writing worker prompts

Mandatory for every `workerPrompt`:

1. **Concretize the spec to literally-implementable level.** Not "handle boundary values correctly" — enumerate input/output examples. `formatBytes(1048575)` must return `"1 MiB"`, not `"1024 KiB"` (rounding-carry boundary).
2. **State the verification command.** A concrete runnable command the worker executes itself (e.g. `npm test -- formatBytes.test.js`). Without one it can only self-attest.
3. **Constrain the response format.** Explicitly forbid pasting code, logs, and diffs — `orchestra-light`'s system prompt already enforces this, but restating it is safer when calling `agent()` directly from a Workflow.
4. **On retry, say the previous files are still on disk.** The template does this automatically; hand-written prompts must include an equivalent sentence.

**Density:** write worker/verifier prompts terse, imperative, English — they are read by cheap models, not humans. **Compress the scaffolding, never the contract:** enumerated I/O examples, boundary values, and the verification command are compression-exempt, and structure (tables, example rows, the response `schema`) beats terse prose for removing ambiguity. Reasoning and the thinking-inflation trap: `references/authoring.md` §3.

## 7. Latency and review economics

- **Default to `pipeline()`, never phase-by-phase `parallel()`.** A barrier is justified only when stage N genuinely needs all of stage N-1 (dedup/merge, global early-exit, real cross-task synthesis).
- **Author adversarial tests from the spec, concurrently with the first implementation, once.** Tests derive from the spec, not the implementation, so they need not be re-authored per retry; each verify then merely *runs* them.
- **Size tasks to fill the concurrency width** (`min(16, cores − 2)`): not so coarse that slots idle, not so trivial that spawn overhead dominates. Split only where file ownership is genuinely disjoint.
- **Review costs ~2x the implementation's output tokens and is worth it.** The PoC's worker passed all 11 of its own tests while shipping a real boundary bug that the reviewer's added test caught. Skip review only when a task's verification is completely self-evident and low-risk — which is exactly what the express criteria (§2) carve out.

Measured numbers, the restructured `runTask`, and the full barrier/sizing rules: `references/authoring.md` §2 and §4.

## 8. Fallback (no Workflow tool)

Launch `orchestra-delegate` (Sonnet, pinned in its own frontmatter) via the Agent tool; it runs `agent-exec dispatch --class light --capture` per round itself, spawns `orchestra-review`, retries with feedback up to 3 rounds, and reports back only the structured verdict. For further rounds, resume the same instance with `SendMessage` rather than spawning a fresh one. Full shape: `references/authoring.md` §5.

## 9. Configuration and external executors

Everything you normally need is one call: **`agent-exec doctor --json`** returns both the readiness verdicts (`ready.<executor>.ok`) and the resolved config (`config.values` — `tiers`, `external_executors`, `priority`, `telemetry.enabled`) already deep-merged from all four layers (defaults ← `~/.claude/orchestra.yaml` ← `.claude/orchestra.yaml` ← `.claude/orchestra.local.yaml`).

**You never walk the priority list.** `agent-exec route --class <cls>` does: it gates every candidate on reality (disabled, binary missing, `ready.ok` false, no `class_policy` for the class, or listed in `--exhausted`) and returns the survivor; `agent-exec dispatch` additionally runs a `dispatch: cli` winner. Copilot and Codex ship `enabled: true`, which is safe precisely because of that gate — on a machine with neither installed, everything resolves to `claude`.

**Unavailable ≠ failed.** Only a genuinely unavailable executor (quota, credits, auth, unresolved binary) is a fallback signal, and it becomes sticky via `exhausted` for the rest of the run. A wrong *result* is not — it stays on the same executor and goes through the normal review/retry loop.

Key semantics, the merge algorithm and its upgrade trap, `enforcement.light_class` and its escape hatches, the `dispatch: cli` sandboxing caveat, and the full commented schema: `references/config.md` §1 (schema copy also at `examples/orchestra.yaml`). To change any of it, use the `setup` skill rather than hand-editing.

## 10. Telemetry

Opt-in, default off, anonymized: `agent-exec` enforces a field/value allowlist, so prompts, paths, ids, and free text are structurally unrecordable. Two sources: `agent-exec run ... --capture` self-logs one `dispatch` record per dispatch (LLM-independent), and you emit exactly one `run_summary` at run end through a haiku relay when enabled. Fields, storage layout, and the `record`/`show`/`archive`/`clear` CLI: `references/config.md` §2.
