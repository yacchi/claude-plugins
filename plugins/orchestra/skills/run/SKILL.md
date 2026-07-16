---
name: run
description: Playbook for cost-tiered multi-agent delegation. An expensive instructor model (Fable/Opus) only decomposes tasks and writes Workflow scripts; execution is delegated to a cheap tier — Haiku workers, then Sonnet verifiers running adversarial checks, then feedback-driven retries on failure — and only structured verdicts flow back to the instructor. Invoke explicitly with /run (or, cross-plugin, `orchestra:run`), or whenever cost-tiered delegation or large parallel task execution is called for. Also invoked as the ORCHESTRATED lane by the <orchestra-router> protocol this plugin injects at SessionStart.
when_to_use: Use when delegating multiple tasks in parallel to cheap models, when building a Haiku-worker + Sonnet-adversarial-verifier pipeline, or when the instructor (main session) must receive only structured verdicts — never logs, diffs, or intermediate artifacts — in its context.
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
- Load test logs, diffs, intermediate artifacts, or raw worker/verifier responses into your own context
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

- No verification pipeline (no orchestra-verifier). Delegate to ONE disposable worker (explicit `model: haiku` or `sonnet`), or handle it directly yourself (subject to your usual direct-edit criteria, e.g. CLAUDE.md rules). The instructor reviews the result itself. At most one express task runs at a time.
- The justification for skipping verification is the same as the conclusion of section 7: skipping adversarial verification is only acceptable for tasks whose verification is completely self-evident and low-risk. The express criteria exist precisely to carve out that subset.

**Abort rule:** the moment scope or success criteria turn out to move mid-flight (decomposition became necessary, a design decision surfaced, the blast radius is wider than assumed), **abort express immediately** and re-route to the orchestrated lane, carrying over the state so far. Never push through on express.

## 3. Model tiers

| Tier | Model | Role |
|---|---|---|
| Instructor | Fable / Opus | Decomposition, contracts, script writing, exception judgment ONLY. Never implements or verifies |
| Design-latitude implementation | Opus (`orchestra-hard-worker`) | Implementation where the spec leaves real design latitude: algorithm choice, API shape, tradeoffs |
| Standard implementation / judgment-based verification | Sonnet | Ordinary implementation, or verification requiring adversarial test design and failure interpretation |
| Mechanical work / fully-scripted verification | Haiku | Implementation or verification whose procedure is 100% prescribed |

**Verifier selection:** if the verification procedure is fully prescribed (exact commands and pass criteria given), Haiku suffices. If failure interpretation, adversarial test design, or spec-ambiguity judgment is needed, use Sonnet. For verification requiring heavy design judgment, pass `model: 'opus'` inline — there is deliberately no separate Opus verifier agent definition.

These defaults can be overridden per project/user via a configuration file — see section 9.

## 4. The one rule that matters most

> **Omitting `model`/`agentType` in a Workflow `agent()` call or in the Agent tool makes the spawned agent inherit the session model — i.e. the instructor's expensive model.** If the instructor runs on Fable/Opus, a worker whose model you forgot to pin runs at Fable/Opus cost, and the entire cost-tiering becomes pointless.
>
> **Every single agent invocation must set `model` or `agentType` explicitly. No exceptions.**

This is stated in the official docs (`/en/workflows`): "Every agent in a workflow uses your session's model unless the script routes a stage to a different one or the `CLAUDE_CODE_SUBAGENT_MODEL` environment variable is set". The same applies on the subagent (Agent tool) side: omitting `model` in frontmatter defaults to `inherit` (= session model).

## 5. Workflow template

Where Dynamic Workflows (`/en/workflows`) are available, adapt this template per task set. Constraints: `export const meta` must be a pure literal object (no variables, no function calls, no template interpolation), no TypeScript syntax, no nondeterministic calls like `Date.now()` / `Math.random()`. Write plain JavaScript.

```javascript
export const meta = {
  name: 'cost-tiered-pipeline',
  description: 'Haiku workers implement each task, Sonnet verifiers adversarially check the result, failures retry with precise feedback up to 3 rounds, and only structured pass/fail verdicts are returned.',
}

// --- Verdict schema: forces the verifier's reply into structured JSON ---
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
// cases to add on top of the worker's own tests).
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

    // Worker tier: cheap, mechanical, literal implementation.
    // ALWAYS pass model or agentType explicitly - see rule #4 above.
    await agent(workerPrompt, {
      label: task.id + '-work-' + attempt,
      model: 'haiku',
      effort: 'low',
    })

    // Verifier tier: adversarial, structured verdict.
    const verdict = await agent(task.verifierPrompt, {
      label: task.id + '-verify-' + attempt,
      model: 'sonnet',
      schema: VERDICT_SCHEMA,
    })

    // agent() returns null if the agent is skipped or dies on a terminal
    // error - guard before dereferencing, or the whole task silently drops.
    if (!verdict) {
      return { id: task.id, pass: false, summary: 'verifier unavailable (skipped or errored)', rounds: attempt }
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

**On `agentType`:** this plugin ships `agents/orchestra-worker.md`, `agents/orchestra-hard-worker.md`, and `agents/orchestra-verifier.md`. Whether the plugin-scoped names (e.g. `orchestra:orchestra-worker`) resolve as the `agentType` option of `agent()` is environment-dependent and unconfirmed. Before relying on it, check the list of available subagents (the @-mention typeahead, or the names visible to the Agent tool); if `orchestra:orchestra-worker` / `orchestra:orchestra-hard-worker` / `orchestra:orchestra-verifier` resolve, pass e.g. `agentType: 'orchestra:orchestra-worker'`. If they don't resolve, or you must run before confirming, fall back to explicit `model: 'haiku'` / `'opus'` / `'sonnet'` (the template's default). Either way, rule #4 stands: exactly one of `model` or `agentType` must always be explicit.

For design-latitude tasks, route the work stage to `orchestra-hard-worker` (Opus) instead of the Haiku worker: `agentType: 'orchestra:orchestra-hard-worker'` or `model: 'opus'`.

## 6. Writing worker prompts

Mandatory requirements when writing each task's `workerPrompt`:

1. **Concretize the spec to literally-implementable level.** Not abstract instructions like "handle boundary values correctly" — enumerate input/output examples. Example: `formatBytes(1048575)` must return `"1 MiB"`, not `"1024 KiB"` (rounding-carry boundary).
2. **State the verification command.** Give a concrete, runnable command the worker can execute itself and confirm fully passes (e.g. `npm test -- formatBytes.test.js`). Without one, the worker can only self-attest, which is not trustworthy.
3. **Constrain the response format.** Explicitly forbid pasting code, logs, and diffs (the `orchestra-worker` agent's system prompt already enforces this, but restating it in the prompt is safer when calling `agent()` directly from a Workflow).
4. **On retry, say that the previous files are still on disk.** The template's `workerPrompt` assembly adds this automatically ("Your previous attempt already wrote files to disk... Read those files first"). When writing prompts by hand, always include an equivalent sentence.

## 7. Findings proven by the PoC

From the measured PoC (3 tasks in parallel, 8 agents, 4 min 23 s, 246k total subagent tokens):

- **4 Haiku workers**: 9,638 output tokens total. **4 Sonnet verifiers**: 19,135 output tokens total. Verifiers consume roughly 2x the workers' output tokens — the main cost is authoring adversarial tests.
- **The instructor consumed zero tokens during execution.** What came back to the instructor was ~2KB of structured JSON.
- **Verification value, demonstrated (formatBytes task):** all 11 of the worker's self-written tests passed, but they didn't cover boundary values. The verifier's added adversarial test caught a rounding-carry bug (`1048575` → returned `"1024 KiB"`; correct is `"1 MiB"`). One feedback-driven retry fixed it and passed.

Conclusion: adversarial verification costs ~2x tokens but catches bugs that the worker's self-attested tests structurally cannot. Skip verification only for tasks whose verification procedure is completely self-evident and low-risk.

## 8. Fallback (environments without the Workflow tool)

Where Dynamic Workflows are unavailable or disabled, fall back to a 3-level nesting: launch the `orchestra-delegate` subagent (Sonnet) via the Agent tool:

```
Instructor (Fable/Opus)
  └─ Agent tool: launch orchestra-delegate (no model needed - pinned to sonnet in its own frontmatter)
       └─ orchestra-delegate internally launches orchestra-worker (haiku) via the Agent tool
       └─ orchestra-delegate internally launches orchestra-verifier (sonnet) via the Agent tool
       └─ on FAIL, orchestra-delegate relaunches orchestra-worker with feedback (cap: 3 rounds)
  └─ instructor receives only the structured verdict from orchestra-delegate
```

When rework needs multiple rounds of back-and-forth, do not spawn a fresh `orchestra-delegate` each time — resume the same instance with `SendMessage` so it keeps its context (previous failures, approaches already tried) without re-explanation.

## 9. Configuration file and external executors

At the start of every orchestration, the instructor resolves configuration from up to three layers, merged in this order (later layers win):

1. **Defaults**: worker: haiku, hard_worker: opus, verifier: sonnet; no external executors.
2. **User**: `~/.claude/orchestra.yaml` (or `.yml`), if present.
3. **Project**: `.claude/orchestra.yaml` (or `.yml`), if present.

This is a **deep merge**, not a first-found-wins lookup: object/mapping keys are merged recursively key-by-key, so a project file only needs to state the keys it actually wants to change — e.g. a project override of just `external_executors.copilot.enabled: true` inherits everything else (codex's whole block, copilot's `model_policy`, etc.) from the user config or defaults. Scalars and arrays are replaced wholesale by the more specific layer, not concatenated or element-merged. An explicit `null`/`~` is a value, not "reset to default" — omit the key entirely to inherit.

The format is YAML, not JSON, specifically so the file can carry comments (JSON can't). `.claude/orchestra.json` / `~/.claude/orchestra.json` (the pre-YAML format) are no longer read — use the `setup` skill (`orchestra:setup`) to convert an old one.

A copy of the schema, fully commented, ships with this plugin at `examples/orchestra.yaml`. Full shape:

```yaml
tiers:
  worker: haiku
  hard_worker: opus
  verifier: sonnet

external_executors:
  codex:
    enabled: true
    dispatch: agent
    agent_type: codex:codex-rescue
    roles: [worker, hard_worker, independent-verifier]
    model_policy:
      worker: { model: gpt-5.6-luna, effort: medium }
      hard_worker: { model: gpt-5.6-sol, effort: xhigh }
      independent-verifier: { model: gpt-5.6-sol, effort: low }
    long_context_escalation:
      when: task requires deep traversal of a large repo (Luna's long-context recall is weak)
      model: gpt-5.6-sol
      effort: xhigh

  copilot:
    enabled: false
    dispatch: cli
    command: >-
      copilot -p {promptfile} --model {model} --effort {effort}
      --allow-all-tools --add-dir {workdir} --output-format json
    resume_command: >-
      copilot --resume={session_id} -p {promptfile} --model {model} --effort {effort}
      --allow-all-tools --add-dir {workdir} --output-format json
    roles: [worker]
    model_policy:
      worker: { model: gpt-5.6-luna, effort: medium }
```

To set this up interactively (detect whether Codex/Copilot are actually available in this environment, choose project vs user scope, edit the file in place without clobbering existing comments), use the `setup` skill instead of hand-editing — see its own SKILL.md.

**`tiers`** overrides the model-tier defaults of section 3. Values are model aliases (or full model IDs) used wherever this skill says haiku/opus/sonnet for the respective role.

**`external_executors`** declares non-Claude executors (Codex, Copilot, etc.) that may be woven into the pipeline. Only entries with `"enabled": true` are used. Two dispatch mechanisms:

- **`dispatch: "agent"`** — the executor is an installed plugin subagent. Pass `agent_type`'s value as `agentType` in Workflow `agent()` calls, or as `subagent_type` in the Agent tool. If the name doesn't resolve in this environment, fall back to the normal model tier for that role.
- **`dispatch: "cli"`** — the executor is a non-interactive CLI. Write the task prompt to a temp file, substitute `{promptfile}` (and, if the config declares them, `{model}`/`{effort}`/`{workdir}`/`{session_id}`) in `command`, and have a **cheap relay agent** (`model: haiku`, with Bash) run the command and return only its final output. The instructor must never run the CLI via Bash itself — the relay exists to keep CLI stdout/stderr out of the instructor's context.

**`roles`** controls where the executor is used:
- `"worker"`: as an implementation worker (in place of, or alongside, the Claude worker tier).
- `"hard_worker"`: as a design-latitude implementation tier (in place of, or alongside, `orchestra-hard-worker`/Opus) — only meaningful for an executor whose `model_policy` names a model strong enough for that tier (see the Codex policy below; Copilot's shipped example intentionally omits this role — see the Copilot section).
- `"independent-verifier"`: as a third-party verification pass in addition to the Claude verifier — useful to avoid single-provider model bias. An independent verifier supplements `orchestra-verifier`; it does not replace the structured-verdict contract, so wrap its output into the same verdict shape.

**`model_policy`** maps each role this executor participates in to a concrete `{ model, effort }` pair to pass to that executor. Without it, the executor runs on its own default model, which defeats the purpose of cost-tiering by external provider just as surely as an unpinned Claude `agent()` call does (rule #4, section 4). Treat this the same way: every external-executor role in `roles` should have a matching `model_policy` entry.

**Safety note:** `cli` dispatch only ever executes command templates that come from the user's own configuration file (`.claude/orchestra.yaml` or `~/.claude/orchestra.yaml`). This plugin ships no CLI commands of its own and must never invent one; if no config file declares a CLI executor, `cli` dispatch is unavailable.

### 9.1 External executor model policy (Codex / Copilot), pricing, and CLI usage

Full detail lives in `references/external-executors.md` (Japanese) — Codex's Sol/Terra/Luna + effort policy and role assignment, Copilot's model catalog and `worker`-role candidates, exact CLI usage recipes (one-shot and session-continuation for retry rounds), and official per-token pricing for Codex/Copilot/Claude. Read it before dispatching to an external executor, and before second-guessing any model/effort choice in the example config.

In brief, as of this plugin's own validation (full reasoning and every round-by-round result: `references/poc-findings.md`):

- **Codex `worker`** → `gpt-5.6-luna` at **`effort: medium`** (not `high` — this plugin's own PoC found `high` produced a real bug on a realistic task that `medium` did not, cheaper and faster besides). **`hard_worker`** → `gpt-5.6-sol`/`xhigh`. **`independent-verifier`** → `gpt-5.6-sol`/`low`. `gpt-5.6-terra` is not in the default policy but hasn't been shown to be dominated either — worth reconsidering for `independent-verifier` if cost is a concern.
- **Copilot `worker`** → `gpt-5.6-luna` at `effort: medium`, the fastest/cheapest validated candidate; `kimi-k2.7-code` and the newly-resolved `mai-code-1-flash-picker` are viable alternatives (see the reference doc for what's been observed about each).
- **Long-context caveat:** Luna has measurably weak long-context recall. Escalate a nominally `worker`-tier task to `hard_worker` if it requires deep traversal of a large repository — the `long_context_escalation` field in the example config documents this trigger.
- Across 6 rounds of escalating task difficulty, no accuracy differentiation was observed until task size crossed into genuine multi-file, multi-language feature territory — at which point every cheap tier tested eventually showed at least one real, narrow defect. Treat the adversarial verifier stage as mandatory beyond a trivially small change, regardless of which model/provider/effort level is implementing the work — this holds for external executors exactly as much as for Claude's own tiers.
