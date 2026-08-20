# Authoring reference (orchestra `run`)

Detail split out of `SKILL.md` in v0.12.0 so the playbook itself stays short. The text is unchanged; `SKILL.md`'s section numbering was kept stable across that split, so every `§N` / "section N" reference below still resolves — except the old `§7` (PoC findings), which is now section 4 of this file.

Load this file when you are actually writing a Workflow script and hit one of: parallel file-mutation safety, `agentType` resolution, per-task latency, prompt density, or the no-Workflow fallback shape.

## 1. Workflow template notes (`SKILL.md` §5)

**Parallel-with-integration is the default.** `pipeline()` and `parallel()` run their file-changing workers concurrently. For same-tree runs, assign disjoint file ownership up front — pin each worker's target files in its prompt and fix shared contracts/types in the prompts too. When ownership genuinely overlaps, use `isolation: 'worktree'` and have the supervisor integrate afterward with:

```text
agent-exec isolate integrate --tasks <a,b,c> [--repo <path>] [--onto <ref>] [--into <id>] [--json|--text]
```

`--tasks` is required and takes comma-separated orchestra task IDs in the order to integrate. Same-file edits are not a reason to serialize. The only legitimate reason to serialize is a real dependency in which a later task must consume an earlier task's output or state. Disjoint ownership protects workers from each other; isolation protects the user's uncommitted work.

**The relay must block.** `dispatchClass`'s relay gets only a prepared token and one shell command, and a command that takes minutes reads to it as something to background and poll — at which point it answers with a progress sentence instead of the JSON, `JSON.parse` throws, and the task fails even though the executor behind it ran to completion and left its work on disk. So the relay prompt states the call is foreground, single, long, and not to be monitored. If a relay does return prose, check the worktree before re-dispatching: the work is usually already there, and only the handshake was lost — re-run from the verify stage rather than paying for the implementation twice.

**On `agentType`:** this plugin ships `agents/orchestra-light.md`, `agents/orchestra-deep.md`, and `agents/orchestra-review.md`. These matter only on `dispatchClass`'s fallback branch (`status: 'delegate'` with no `agent_type`, i.e. Claude was the routed candidate) or when writing the express lane / §8 fallback pattern by hand — whether the plugin-scoped names (e.g. `orchestra:orchestra-light`) resolve as the `agentType` option of `agent()` is environment-dependent and unconfirmed. Before relying on it, check the list of available subagents (the @-mention typeahead, or the names visible to the Agent tool); if `orchestra:orchestra-light` / `orchestra:orchestra-deep` / `orchestra:orchestra-review` resolve, pass e.g. `agentType: 'orchestra:orchestra-light'`. If they don't resolve, fall back further to explicit `model: 'haiku'` / `'opus'` / `'sonnet'`. Either way, rule #4 stands: exactly one of `model` or `agentType` must always be explicit — `dispatchClass`'s own branches already guarantee this on the routed path. The `standard` class has no dedicated Claude agent definition — its Claude-side fallback is `model: 'sonnet'` inline instead of an `agentType`.

For tasks with modest design latitude, call `dispatchClass('standard', workerPromptFile, opts)` instead of `'light'` — same helper; `agent-exec route --class standard` resolves it (Copilot Luna by default, Sonnet otherwise). For tasks with real design latitude, route the work stage to `orchestra-deep` (Opus) instead: `dispatchClass('deep', workerPromptFile, opts)`, or, on the fallback branch, `agentType: 'orchestra:orchestra-deep'` / `model: 'opus'` directly.

## 2. Shortening the critical path

### 5.1 Shorten the per-task critical path — overlap authoring

`pipeline()` already runs tasks concurrently, so the latency you actually feel is not "tasks aren't parallel" — it is (a) the **sequential `work → verify → retry` chain inside a single task**, and (b) **barriers that serialize work with no real dependency** (§5.2). This subsection cuts (a).

**Overlap adversarial-test authoring with implementation, and author once.** §7 measured that the review pass's dominant cost is *authoring* adversarial tests — and those tests derive from the **spec, not the implementation** (the `formatBytes(1048575) → "1 MiB"` boundary test that caught the PoC bug is fully spec-derivable). So author them *concurrently with* the first implementation, and author them **once** — the spec doesn't change across retries, only the implementation does. Each verify step then merely *runs* the pre-authored tests plus a whitebox glance, which is cheap. The pre-authored tests are exactly `orchestra-review`'s "≥3 additional adversarial tests"; only their authoring moves earlier.

This restructures `runTask` (author-tests worker owns the `tests/` paths, impl worker owns the `src/` paths — disjoint, per the same-tree safety rule above):

```javascript
// (dispatchClass(), correctionPacket(), regatePrompt(), NEXT_CLASS, MAX_GATES and
// the module-level `exhausted` Set are defined in §5/§11 above - this
// restructuring only changes runTask, not the rest of the script.)
async function runTask(task) {
  // Author adversarial tests from the SPEC, concurrently with the first
  // implementation. Disjoint paths (tests/ vs src/), so no write conflict.
  // The test-authoring side stays pinned to Sonnet, same reasoning as the
  // review stage below (`priority.review` is `[claude]`-only, §9).
  await parallel([
    () => dispatchClass(task.cls || 'light', task.workerPromptFile, { label: task.id + '-work-1', workdir: task.workdir }),
    () => agent(task.authorTestsPrompt, { label: task.id + '-authtests', model: 'sonnet' }),
  ])

  let cls = task.cls || 'light'
  let prior = null
  for (let gate = 1; gate <= MAX_GATES; gate++) {
    if (prior) {
      // Re-implement only, via a self-contained correction packet to a FRESH
      // worker (§11.3). Tests are already on disk from the concurrent author
      // step and are NOT re-written across rounds.
      const work = await dispatchClass(cls, correctionPacket(task, prior, gate),
        { label: task.id + '-work-' + gate, workdir: task.workdir })
      if (typeof work === 'string' && work.trim().startsWith('ESCALATE')) {
        if (NEXT_CLASS[cls] === cls) return { id: task.id, pass: false, needsInstructor: true, summary: 'escalated at deep class' }
        cls = NEXT_CLASS[cls]; gate--; continue
      }
    }
    // Verify = RUN the pre-authored tests + whitebox glance. No re-authoring.
    // Gate 2 is incremental (§11.2). Unrouted, same as §5's review stage.
    const verdict = await agent(prior ? regatePrompt(task, prior) : task.runTestsPrompt, {
      label: task.id + '-verify-' + gate, model: 'sonnet', schema: VERDICT_SCHEMA,
    })
    if (!verdict) return { id: task.id, pass: false, summary: 'review unavailable (skipped or errored)', rounds: gate }
    if (verdict.pass) return { id: task.id, pass: true, summary: verdict.summary, rounds: gate }
    if (verdict.new_family) {
      return { id: task.id, pass: false, needsInstructor: true, rounds: gate,
               summary: 'new defect family at re-gate: ' + verdict.summary, feedback: verdict.feedback }
    }
    if (prior && (verdict.feedback || []).some(f => (prior.feedback || []).some(p => p.family === f.family))) {
      cls = NEXT_CLASS[cls]
    }
    prior = verdict
  }
  return { id: task.id, pass: false, needsInstructor: true, rounds: MAX_GATES,
           summary: 'two gates without a pass - instructor re-analysis required', feedback: prior && prior.feedback }
}
```

**Note the interaction with §11.2.** Pre-authored tests make the *sweep* cheaper, not optional: the reviewer still classifies each finding by family and checks sibling sites, it just does so against a suite it already has rather than one it writes mid-gate. That is precisely the combination that keeps the whole task inside two gates.

**Trade-off:** this adds one agent (the test author) and thus one spawn's overhead, so it pays off when authoring latency exceeds spawn overhead — true whenever the reviewer writes non-trivial tests, and the win *grows with retry depth* because authoring no longer repeats per round. For a change so small its review is a single obvious assertion, keep the plain §5 `runTask` instead. Split `task.verifierPrompt` into `task.authorTestsPrompt` (write spec-derived adversarial tests to `tests/`, do not run them) and `task.runTestsPrompt` (run the worker's tests **and** the pre-authored `tests/`, whitebox-inspect the diff, return `VERDICT_SCHEMA`).

### 5.2 Never serialize independent work behind a barrier

The most common reason an orchestration "feels sequential" is a barrier — a `parallel()` between phases, or an `await` — placed where there is no real dependency. The runtime does not add these; the instructor does, by writing phase-by-phase code. Guard against it:

- **Default to `pipeline()`, not phase-by-phase `parallel()`, and integrate rather than serialize overlap.** Writing `const impls = await parallel(tasks.map(work)); const revs = await parallel(impls.map(verify))` forces *every* implementation to finish before *any* verify starts — the single slowest task stalls all reviews. `pipeline(tasks, runTask)` lets each task's verify start the instant *its own* work is done. A barrier between stages is justified ONLY when a later stage has a real dependency on earlier output, such as a global early-exit (`0 findings → skip`) or a synthesis that literally reads every task. Same-file edits are integrated after parallel isolated work; they do not justify serialization.
- **A "final review" is per-task unless it truly reads all tasks.** If a final check only re-validates task A, it belongs *inside* task A's pipeline chain, not in a global barrier that runs after every task finishes. Reserve a single whole-set barrier for a real cross-task synthesis, and scope it to the minimal set of tasks it actually consumes — not "all of them" by reflex.
- **Independent review passes run concurrently, not one after another.** When a task gets both a Claude `review` and an `independent-review` (§9 — a different provider's eyes), the two share no dependency. Dispatch them together — `await parallel([() => claudeReview(...), () => independentReview(...)])` — and merge the two verdicts, rather than `await`-ing one and then the other.

### 5.3 Size tasks to fill the concurrency width

`pipeline()`/`parallel()` run at most `min(16, cores − 2)` agents at once. Two failure modes waste that width: **too few, too coarse tasks** (three 10-minute tasks can occupy at most three slots — split independent sub-parts into separate pipeline items *when their file ownership is disjoint*), and **too many trivial tasks** (fixed spawn overhead then dominates useful work). Aim for tasks large enough to amortize a spawn yet numerous and independent enough to keep the slots full. Splitting helps *only* when the parts are genuinely independent and touch disjoint paths — a split that introduces a cross-task dependency just re-adds the barrier §5.2 told you to avoid.

## 3. Prompt density (`SKILL.md` §6)


### 6.1 Prompt density: compress the scaffolding, never the spec

Worker and verifier prompts are read by cheap models, not humans. They owe nothing to readability, politeness, or a target language — write them terse, imperative, and in **English** (more token-efficient than Japanese for the same content, and the workers need no Japanese). Drop honorifics, hedges, and prose transitions; nominal/telegraphic style is fine. This trims the instructor's authoring output and the worker's input at essentially no risk.

**But this applies only to the scaffolding — never to the contract itself.** The token-compression techniques that circulate for chat replies (caveman/genshijin-style particle-dropping, "essence only") are an *output*-compression trick, and their own authors report the catch: on complex tasks completeness drops and internal thinking tokens balloon (+200–400%), erasing most of the nominal saving. A worker's job *is* the complex-task case, so:

- **Do NOT compress the spec.** The `formatBytes(1048575) → "1 MiB"` boundary example (requirement 1 above) cannot lose a character without losing meaning, and the one example you delete to save tokens is exactly where §7's rounding-carry class of bug hides. Enumerated I/O examples, boundary values, and the verification command are compression-exempt.
- **Reduce variance with structure, not with prose compression.** Terse natural language is *more* ambiguous, not less. When you want to pin down behaviour and kill wording drift, reach for tables, enumerated example rows, and the response `schema` — structure removes ambiguity; dropping particles adds it.
- **Do NOT compress worker/verifier *output*.** It is already lean: the verdict is forced into `VERDICT_SCHEMA` JSON and code/log/diff pasting is already forbidden (§6-3). The review pass's main output is adversarial *test code*, which does not compress. Light-class workers (Copilot Luna/medium, or Haiku when routed there as fallback) think little at their cheapest effort setting, so the thinking-inflation risk is low there — but never ask the Sonnet reviewer to write tersely at the cost of the tests it authors.

Rule of thumb: **strip everything a human would want and a machine does not; keep every concrete fact the worker must reproduce exactly.**

## 4. Findings proven by the PoC



From the measured PoC (3 tasks in parallel, 8 agents, 4 min 23 s, 246k total subagent tokens):

- **4 `light`-class (Haiku) implementations**: 9,638 output tokens total. **4 Sonnet `review` passes**: 19,135 output tokens total. Review passes consume roughly 2x the implementation's output tokens — the main cost is authoring adversarial tests.
- **The instructor consumed zero tokens during execution.** What came back to the instructor was ~2KB of structured JSON.
- **Review value, demonstrated (formatBytes task):** all 11 of the implementation's self-written tests passed, but they didn't cover boundary values. The review pass's added adversarial test caught a rounding-carry bug (`1048575` → returned `"1024 KiB"`; correct is `"1 MiB"`). One feedback-driven retry fixed it and passed.

Conclusion: adversarial review costs ~2x tokens but catches bugs that the implementation's self-attested tests structurally cannot. Skip review only for tasks whose review procedure is completely self-evident and low-risk.

## 5. Fallback (environments without the Workflow tool)



Where Dynamic Workflows are unavailable or disabled, fall back to a 3-level nesting: launch the `orchestra-delegate` subagent (Sonnet) via the Agent tool:

```
Instructor (Fable/Opus)
  └─ Agent tool: launch orchestra-delegate (no model needed - pinned to sonnet in its own frontmatter)
       └─ orchestra-delegate itself runs `agent-exec dispatch --class light --capture` per round
            (no relay needed - it already has Bash) - Copilot by default, orchestra-light/haiku
            only on that call's own "delegate" fallback, never launched directly
       └─ orchestra-delegate internally launches orchestra-review (sonnet) via the Agent tool
       └─ on FAIL, orchestra-delegate re-dispatches a self-contained correction packet
            to a FRESH worker and runs one incremental re-gate (cap: 2 gates, §11)
  └─ instructor receives only the structured verdict from orchestra-delegate
```

Two gates without a pass, a re-gate reporting `new_family`, or an `ESCALATE` at `deep` all come back as `needsInstructor` — a request for re-analysis, not a request for more rounds. Answering it by telling the delegate to try again is exactly the loop §11.2 exists to break.

Across those instructor-driven rounds, do not spawn a fresh `orchestra-delegate` each time — resume the same instance with `SendMessage` so it keeps its context (previous failures, approaches already tried) without re-explanation. This is the supervisor-level exception to §11.3's "fresh invocation" rule, which governs the *implementation* worker: the delegate's accumulated context is the thing of value, whereas the failed worker's is a liability.
