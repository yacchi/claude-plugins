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
| `references/gates.md` | A gate rule (§11) is biting and you need the why: a reviewer's rejection looks out of scope, rounds keep closing one sibling at a time, or you must decide whether to spend a third round. |
| `references/isolation.md` | A worker will touch a tree holding uncommitted work (isolation is the default there), or the run needs rollback on failure, a real diff for the reviewer, worktree-isolated parallelism, competing implementations of one contract, or an approval budget for non-interactive workers. |
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

**Ask "do I already hold the context?" BEFORE judging size.** If the content is already formed in your head from this conversation — a spec you just designed, a fix you already worked out — write it yourself regardless of size or file count. Delegation cannot shrink context you already hold; it only adds overhead and the risk that the worker mis-integrates what you already knew. **Tell-tale: if writing the delegation prompt would require spelling out the full content or spec, you have already authored it — just write it.** Delegation pays off only for work needing exploration, reading, or context you do NOT yet hold. This exemption covers content you already hold; it is not a licence for an open-ended build, and it lapses the moment the work outgrows what you held — at which point the abort rule below applies.

**Express shape:** no review pipeline. One disposable implementer at `light`/`standard` (prefer `dispatchClass()`, §5; explicit `model: 'haiku'`/`'sonnet'` only when that helper isn't wired in), or handle it yourself per the two gates above. You judge the result — but hand the *verification run* (tests, builds, smoke checks) to a cheap subagent and read back only its verdict rather than letting the output flood your context: Haiku when the procedure is 100% prescribed (exact commands, explicit pass criteria), Sonnet when interpreting failures needs judgment. One express task at a time.

**Abort rule:** the moment scope or success criteria move mid-flight, **abort express immediately** and re-route to orchestrated, carrying over the state so far. Never push through. Re-judge this *while working*, not only when the user speaks again: turn-level analysis found orchestra invoked in 1 of the 102 turns that went on to hand-edit 10+ files, because the prompts that produce them ("作業を続けて", "実装して", "y") look small at classification time. Finishing the edit already open is fine; starting the next chunk by hand is the tell. A PostToolUse hook (`enforcement.turn_edits`) will say so once if you miss it.

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
  description: 'Light-class implementation per task (executor resolved by agent-exec route/dispatch - Copilot by default, Haiku as fallback), an adversarial review pass checks the result, a rejection gets one correction round with a cited, family-swept packet and an incremental re-gate, and only structured verdicts return.',
}

// --- Verdict schema: forces the review reply into structured JSON ---
// `cited_contract` is required on every finding: a reviewer that cannot point
// at the contract text has an opinion, not a defect (§11.1). `family` drives the
// retry policy (§11.2). Non-contractual improvements go to `optional_hardening`,
// which NEVER makes `pass` false.
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
        required: ['id', 'family', 'case', 'expected', 'actual', 'cited_contract'],
        properties: {
          id: { type: 'string' },
          family: { type: 'string' },      // rounding-carry / input-normalization / error-type / ...
          case: { type: 'string' },
          expected: { type: 'string' },
          actual: { type: 'string' },
          cited_contract: { type: 'string' },
          must_not_change: { type: 'string' },
        },
      },
    },
    optional_hardening: { type: 'array', items: { type: 'string' } },
    new_family: { type: 'boolean' },       // re-gate found a family that predates the correction
  },
}

// One initial gate + one post-correction gate. NOT a blind retry count: see
// §11.2 for why a third round is an instructor decision, not an automatic one.
const MAX_GATES = 2

// Sticky exhaustion for this run: an executor found `unavailable` stays
// skipped for every remaining task/retry. This Set is your ONLY manual job in
// selection - the priority walk itself runs inside `agent-exec route`.
const exhausted = new Set()

// Trailing runtime controls for an agent-dispatch executor. Codex's rescue
// agent strips `--model`/`--effort` out of the task text and forwards them to
// the CLI; anything the route left null is simply omitted.
function routingFlags(r) {
  return (r.model ? '\n\n--model ' + r.model : '') + (r.effort ? ' --effort ' + r.effort : '')
}

// Dispatch one task at a capability class ('light' | 'standard' | 'deep').
// Selection is `agent-exec route`'s job, not this function's and not yours.
// Same call shape whether it resolves to Copilot or to a Claude tier.
async function dispatchClass(cls, promptText, opts = {}) {
  const promptFiles = Array.isArray(promptText) ? promptText : [promptText]
  const relayPrompt =
    'Run `agent-exec dispatch --class ' + cls +
    (opts.archetype ? ' --archetype ' + opts.archetype : '') +
    (exhausted.size ? ' --exhausted ' + [...exhausted].join(',') : '') +
    promptFiles.map(file => ' --prompt-file ' + file).join('') +
    ' --workdir ' + (opts.workdir || '.') + ' --capture`' +
    ' as ONE foreground Bash call with timeout 600000. It routinely takes many minutes: just ' +
    'wait for it. Never background it, never poll it, never start a monitor, never report ' +
    'progress. When it returns, print its stdout JSON verbatim - nothing else.'

  const raw = await agent(relayPrompt, { label: (opts.label || cls) + '-dispatch', model: 'haiku', effort: 'low' })
  const r = JSON.parse(raw)

  if (r.status === 'ok') return r.answer // a CLI executor (e.g. Copilot) already ran it.
  if (r.status === 'delegate') {
    // route picked Claude or an agent-dispatch executor (e.g. Codex) - only
    // your own agent() call can spawn either, so detecting ITS unavailability
    // (agent() returning null) and feeding it back into `exhausted` is on you.
    // For an agent-dispatch executor, `model`/`effort` are NOT agent() options
    // (those pick a Claude tier) - they belong to the executor behind the
    // subagent, which reads them off the prompt text. Codex's rescue agent
    // leaves both unset unless the request names them, so dropping them here
    // silently runs Codex at whatever ~/.codex/config.toml defaults to instead
    // of the class_policy this route just resolved.
    const directPrompt = 'Read the worker prompt from ' + promptFiles.join(' and ') + ' and carry it out.'
    const answer = r.agent_type
      ? await agent(directPrompt + routingFlags(r), { label: opts.label || cls, agentType: r.agent_type })
      : await agent(directPrompt, { label: opts.label || cls, model: r.model, effort: r.effort })
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
// needs: id, cls ('light'|'standard'|'deep'), workerPromptFile (path to the
// literal spec + edge cases + verify command), workerPrompt (same contract text
// for runtime correction packets), verifierPrompt (what to re-check + which
// adversarial cases to add). Optional: workdir, baseline (a snapshot ref - see
// references/isolation.md - so a bad attempt can be rolled back instead of
// patched, and the re-gate can diff against something real).
const tasks = (typeof args !== 'undefined' && args && args.tasks) ? args.tasks : []

const NEXT_CLASS = { light: 'standard', standard: 'deep', deep: 'deep' }

// Self-contained correction packet (§6). It must stand alone: a FRESH worker
// invocation reads it, not the one that failed - see §11.3.
function correctionPacket(task, verdict, gate) {
  return task.workerPrompt +
    '\n\n--- CORRECTION PACKET (gate ' + gate + ') ---\n' +
    'Your predecessor attempted this task and was REJECTED. Its files are still on disk ' +
    'at the paths named in the contract above; read them first, then fix exactly what is ' +
    'listed below and nothing else. Every finding cites the contract line it violates - ' +
    'if a finding contradicts the contract, report ESCALATE instead of guessing.\n' +
    'Honor every `must_not_change` field: those paths/behaviors are already accepted.\n' +
    'Rejection findings:\n' + JSON.stringify(verdict.feedback) +
    '\nThe next review will check ONLY these findings plus regressions they could cause.'
}

// A correction packet is assembled at runtime from the reviewer verdict, so it has no
// file on disk. A correction round therefore bypasses the relay and goes straight to a
// pinned Claude tier via `agent()`.

// A re-gate is incremental, not a fresh audit (§11.2).
function regatePrompt(task, verdict) {
  return task.verifierPrompt +
    '\n\n--- RE-GATE MODE ---\n' +
    'This is an incremental re-gate. Previous verdict summary: ' + verdict.summary + '\n' +
    'Findings that were supposed to be closed: ' + JSON.stringify((verdict.feedback || []).map(f => f.id)) + '\n' +
    'Check ONLY (a) whether each of those is actually closed, and (b) regressions those ' +
    'corrections could plausibly cause. Do NOT restart the full specification audit and do ' +
    'not re-litigate anything the previous verdict accepted. If you find a defect family ' +
    'that predates the correction, set new_family true and say so - do not silently fail it. ' +
    'Cap yourself at 6 read-only shell invocations.'
}

async function runTask(task) {
  let cls = task.cls || 'light'   // per the task's design latitude (§3)
  let prior = null

  for (let gate = 1; gate <= MAX_GATES; gate++) {
    const work = prior
      ? await agent(correctionPacket(task, prior, gate), {
          label: task.id + '-work-' + gate, model: 'sonnet',
        })
      : await dispatchClass(cls, task.workerPromptFile, {
          label: task.id + '-work-' + gate, workdir: task.workdir,
        })

    // A worker that hits a design decision, a boundary crossing, or an
    // unexplained failure reports ESCALATE instead of looping (§11.3). Bump the
    // class and retry WITHOUT consuming a gate - the gate budget is for
    // defects, not for a mis-sized packet.
    if (typeof work === 'string' && work.trim().startsWith('ESCALATE')) {
      // Keep `prior`: if it escalated mid-correction, the stronger class still
      // needs the findings it was escalating over.
      if (NEXT_CLASS[cls] !== cls) { cls = NEXT_CLASS[cls]; gate--; continue }
      return { id: task.id, pass: false, needsInstructor: true, summary: 'escalated at deep class: ' + work.slice(0, 300) }
    }

    // Review stays pinned to Sonnet and unrouted: `priority.review` is
    // `[claude]`-only, so routing it would just add a relay hop.
    const verdict = await agent(prior ? regatePrompt(task, prior) : task.verifierPrompt, {
      label: task.id + '-verify-' + gate,
      model: 'sonnet',
      schema: VERDICT_SCHEMA,
    })

    // agent() returns null when skipped or on a terminal error - guard, or the
    // whole task silently drops.
    if (!verdict) {
      return { id: task.id, pass: false, summary: 'review unavailable (skipped or errored)', rounds: gate }
    }
    if (verdict.pass) {
      return { id: task.id, pass: true, summary: verdict.summary, rounds: gate,
               optional_hardening: verdict.optional_hardening || [] }
    }

    // A FAIL with an empty `feedback` array is unactionable: the packet would
    // interpolate `undefined` and the next worker can only guess or ESCALATE.
    // Reviewers do prose the findings into `summary` instead of the field, so
    // guard rather than trust the schema. Hand it back with the summary intact
    // - you can re-derive the findings from it far cheaper than a wasted round.
    if (!(verdict.feedback || []).length) {
      return { id: task.id, pass: false, needsInstructor: true, rounds: gate,
               summary: 'FAIL with no structured findings: ' + verdict.summary }
    }

    // A re-gate that surfaces a defect family the correction did not introduce
    // means the FIRST review under-swept. Another blind round would just find
    // the next sibling - hand it back for re-analysis instead (§11.2).
    if (verdict.new_family) {
      return { id: task.id, pass: false, needsInstructor: true, rounds: gate,
               summary: 'new defect family at re-gate: ' + verdict.summary, feedback: verdict.feedback }
    }
    // Same family failing twice = the packet is under-specified for this class,
    // not the worker being careless. Escalate rather than repeat.
    if (prior && (verdict.feedback || []).some(f => (prior.feedback || []).some(p => p.family === f.family))) {
      cls = NEXT_CLASS[cls]
    }
    prior = verdict
  }

  return { id: task.id, pass: false, needsInstructor: true, rounds: MAX_GATES,
           summary: 'two gates without a pass - instructor re-analysis required', feedback: prior && prior.feedback }
}

// pipeline() runs every task through work->verify->retry independently and in
// parallel - task A can be on retry 2 while task B is still on its first
// verify. No barrier between stages, unlike parallel().
const results = await pipeline(tasks, runTask)

return results
```

The relay command should carry `--run-id <the workflow run id>` so Copilot/Codex cost lands in the same ledger the instructor later queries.

**Same-tree parallelism safety.** `pipeline()`/`parallel()` run file-changing workers concurrently against the **same working tree** — two workers with overlapping file ownership silently corrupt each other. Pin disjoint target files (and shared contracts/types) in each worker's prompt, and keep workers small. Full guidance: `references/authoring.md` §1.

**Fan-out variant.** When several tasks must touch the same file but their interfaces can be frozen up front — exact key names, function signatures, CLI flags, JSON shapes, written once and pasted verbatim into every contract — give each worker its own worktree, run them in parallel, then `agent-exec isolate integrate --tasks <a,b,c>` and run one mandatory integration gate on the merged tree.

**Decision rule: serialize only for a real ordering dependency** — one task must read another task's *output* to do its own work. Two tasks editing the same file is not that; it is a merge, and merges are mechanized now. If the interface between the tasks can be written down before either starts, the dependency is on the contract, not on the code, and the tasks run in parallel.

**Isolate whenever the tree is dirty.** Disjoint ownership does not protect the *user's* uncommitted work: transcripts show workers of every model tier running `git checkout --`/`restore`/`reset --hard` over changes they did not author, because a diff a worker did not write reads as contamination whatever its prompt says. So when the tree has uncommitted work, give file-changing workers their own tree — `agent-exec dispatch --isolate auto --task <id>` (the default; it isolates exactly when the tree is dirty) for CLI executors, `isolation: 'worktree'` for `agent()` calls. Collect with `agent-exec isolate diff --task <id>`. Full guidance: `references/isolation.md`.

**On `agentType`:** the plugin-scoped names (`orchestra:orchestra-light` / `-deep` / `-review`) may or may not resolve as `agent()`'s `agentType` in this environment — check the available-subagents list before relying on them, and fall back to explicit `model:`. See `references/authoring.md` §1.

**Latency:** for the per-task critical path (overlapping adversarial-test authoring with implementation, avoiding needless barriers, sizing tasks to the concurrency width), see §7 and `references/authoring.md` §2.

**Follow-up rounds:** when a subagent's work needs another pass, resume that same instance with `SendMessage` rather than spawning a fresh one — it still holds what already failed and what it has tried, so nothing needs re-explaining.

**At run end:** if `doctor`'s `config.values.telemetry.enabled` is true, have a cheap haiku relay agent emit one `run_summary` via `agent-exec telemetry record --json '...'` — never yourself. If disabled, skip silently (§10).

## 6. Writing worker prompts

Mandatory for every `workerPrompt`:

1. **Concretize the spec to literally-implementable level.** Not "handle boundary values correctly" — enumerate input/output examples. `formatBytes(1048575)` must return `"1 MiB"`, not `"1024 KiB"` (rounding-carry boundary).
2. **State the verification command.** A concrete runnable command the worker executes itself (e.g. `npm test -- formatBytes.test.js`). Without one it can only self-attest.
3. **Constrain the response format.** Explicitly forbid pasting code, logs, and diffs — `orchestra-light`'s system prompt already enforces this, but restating it is safer when calling `agent()` directly from a Workflow.
4. **Name the files the worker owns** — and, when other workers run concurrently, say that everything else is off-limits (§5's same-tree safety rule, or give it a worktree per `references/isolation.md`).
5. **Permit escalation explicitly.** State that a decision the packet doesn't settle, a change crossing the ownership boundary, or a failure it can't explain locally should come back as `ESCALATE` rather than a guess (§11.3).
6. **Keep relay-dispatched worker prompts on disk.** Write the prompt to a FILE and give the relay only its path. Never put task text, a base64 blob, or any other payload in the relay's prompt: it is a model and will act on the text or fail to reproduce it verbatim. Split shared preamble and per-task contract into separate files and pass `--prompt-file` twice rather than duplicating the preamble.
7. **A correction packet must stand alone.** It goes to a *fresh* worker, so it carries: the original contract in full, the rejection findings with their `cited_contract`, the `must_not_change` paths, the fact that the previous attempt's files are still on disk, and what the next gate will check. `correctionPacket()` in §5 does this; hand-written prompts must include all six.

**Density:** write worker/verifier prompts terse, imperative, English — they are read by cheap models, not humans. **Compress the scaffolding, never the contract:** enumerated I/O examples, boundary values, and the verification command are compression-exempt, and structure (tables, example rows, the response `schema`) beats terse prose for removing ambiguity. Reasoning and the thinking-inflation trap: `references/authoring.md` §3.

## 7. Latency and review economics

- **Default to parallel work followed by integration, never serialization for file overlap.** A barrier is justified only when a later stage has a real dependency on earlier output, such as a global early-exit or synthesis that literally reads every task. Same-file edits are handled with isolated worktrees and supervisor-run integration, not by making otherwise independent tasks wait.
- **Author adversarial tests from the spec, concurrently with the first implementation, once.** Tests derive from the spec, not the implementation, so they need not be re-authored per retry; each verify then merely *runs* them.
- **Size tasks to fill the concurrency width** (`min(16, cores − 2)`): not so coarse that slots idle, not so trivial that spawn overhead dominates. Split only where file ownership is genuinely disjoint.
- **Review costs ~2x the implementation's output tokens and is worth it.** The PoC's worker passed all 11 of its own tests while shipping a real boundary bug that the reviewer's added test caught. Skip review only when a task's verification is completely self-evident and low-risk — which is exactly what the express criteria (§2) carve out.

Measured numbers, the restructured `runTask`, and the full barrier/sizing rules: `references/authoring.md` §2 and §4.

## 8. Fallback (no Workflow tool)

Launch `orchestra-delegate` (Sonnet, pinned in its own frontmatter) via the Agent tool; it runs `agent-exec dispatch --class light --capture` per round itself, spawns `orchestra-review`, applies the same two-gate discipline (§11) — one correction round with a cited, family-swept packet to a fresh worker, then an incremental re-gate — and reports back only the structured verdict. When it returns `needsInstructor`, that is your re-analysis cue, not a signal to tell it to try again. Full shape: `references/authoring.md` §5.

## 9. Configuration and external executors

Everything you normally need is one call: **`agent-exec doctor --json`** returns both the readiness verdicts (`ready.<executor>.ok`) and the resolved config (`config.values` — `tiers`, `external_executors`, `priority`, `telemetry.enabled`) already deep-merged from all four layers (defaults ← `~/.claude/orchestra.yaml` ← `.claude/orchestra.yaml` ← `.claude/orchestra.local.yaml`).

**You never walk the priority list.** `agent-exec route --class <cls>` does: it gates every candidate on reality (disabled, binary missing, `ready.ok` false, no `class_policy` for the class, or listed in `--exhausted`) and returns the survivor; `agent-exec dispatch` additionally runs a `dispatch: cli` winner. Copilot and Codex ship `enabled: true`, which is safe precisely because of that gate — on a machine with neither installed, everything resolves to `claude`.

**Unavailable ≠ failed.** Only a genuinely unavailable executor (quota, credits, auth, unresolved binary) is a fallback signal, and it becomes sticky via `exhausted` for the rest of the run. A wrong *result* is not — it stays on the same executor and goes through the normal review/retry loop.

Key semantics, the merge algorithm and its upgrade trap, `enforcement.light_class` and its escape hatches, the `dispatch: cli` sandboxing caveat, and the full commented schema: `references/config.md` §1 (schema copy also at `examples/orchestra.yaml`). To change any of it, use the `setup` skill rather than hand-editing.

## 10. Telemetry

Opt-in, default off, anonymized: `agent-exec` enforces a field/value allowlist, so prompts, paths, ids, and free text are structurally unrecordable. Two sources: `agent-exec run ... --capture` self-logs one `dispatch` record per dispatch (LLM-independent), and you emit exactly one `run_summary` at run end through a haiku relay when enabled. Fields, storage layout, and the `record`/`show`/`archive`/`clear` CLI: `references/config.md` §2.

Measure one workflow run with `agent-exec usage --run <workflow-run-id>`; this is exact rather than a time window. External-executor dispatches must carry `--run-id` for their costs to be attributable to that run.

## 11. Gate discipline

Rounds are the pipeline's real cost. Slow runs are usually not slow because a model was weak, but because rounds were spent badly. `MAX_GATES = 2` replaces the old blind 3-retry count; these rules are what make two gates enough.

- **A rejection needs a citation.** FAIL only on contract text the work violates; `cited_contract` is required on every finding. Non-contractual improvements go to `optional_hardening` and never block. Applies to your own correction packets too — your mid-run inference is not a requirement.
- **Findings carry a defect `family`; the reviewer sweeps siblings before reporting.** One round closes the class, not one instance.
- **Gate 2 is a re-gate, not a re-audit** (`regatePrompt()`, §5). If it reports `new_family`, the loop stops and hands back to you: the first sweep was wrong, so another automatic round just finds the next sibling.
- **Escalate the class on `ESCALATE` or a twice-failing family** — that doesn't consume a gate, since a mis-sized packet is not a defect. Start auth/session/concurrency/security work at `standard`/`deep` rather than proving it through a cheap failure. Escalation never widens scope.
- **Corrections go to a *fresh* invocation**, never back to the rejected worker, which is anchored on the reasoning that produced the defect. Resume (`SendMessage`) only when a long investigation would be expensive to reconstruct.
- **Budget invocations:** ~12 shell calls per worker and per review, 6 per re-gate. An interrupted or budget-exhausted review is a FAIL, never a PASS.

Rationale, failure modes, and the family taxonomy: `references/gates.md`.

## 12. Rollback, diffs, and aggressive isolation

VCS is effectively free, and a run that can return to a known-good state can afford to be aggressive. Take a baseline before the first worker (`git init` if the tree isn't a repo) and snapshot between attempts: the reviewer then diffs against something real instead of inferring the worker's footprint from prose, and a bad attempt is rolled back instead of patched over.

**Parallel-with-integration is the default.** Run independent tasks concurrently. When their paths overlap, isolate them in worktrees rather than serializing them; after they finish, the supervisor mechanizes integration with:

```text
agent-exec isolate integrate --tasks <a,b,c> [--repo <path>] [--onto <ref>] [--into <id>] [--json|--text]
```

`--tasks` is required and takes comma-separated orchestra task IDs in the order to integrate. Same-file edits are not a reason to serialize. The only legitimate reason to serialize is a real dependency in which a later task must consume an earlier task's output or state.

Isolation is still the default whenever the tree is dirty, not a special-case optimization. `agent-exec dispatch` isolates on its own (`--isolate auto`, needs `--task <id>`), covering CLI executors that `agent()`'s `isolation` option cannot reach; pass `--isolate always` to isolate a clean tree too, `never` to opt out. It carries the user's uncommitted work in, copies gitignored dependency dirs (`node_modules`, `.venv`, …) by CoW clone so no worker re-runs an install, and reuses one worktree per `--task` across retry rounds. Where git-worktree-runner is installed it goes through `gtr`, so the user's own copy patterns and postCreate hooks apply; orchestra never writes gtr config.

Beyond safety, isolation also lifts the disjoint-file-ownership constraint (§5) — use it to parallelize genuinely tangled work, make exploratory failures free, and run **competing implementations of one contract**, where variant disagreement is a defect report about your *spec*. N× tokens, so spend that variant-fanout on the risky core.

Workers never touch VCS state; snapshots, merges, and cleanup are the supervisor's. Descendant agent branches are disposable and unrestricted; the PR branch is built from the accepted diff and stays clean. Never commit to, rebase, or push the user's branch without an explicit request. Full guidance: `references/isolation.md`.
