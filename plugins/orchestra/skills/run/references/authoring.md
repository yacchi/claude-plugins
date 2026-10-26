# Authoring reference (orchestra `run`)


Load this file when you are actually setting up a run and hit one of: parallel file-mutation safety, `agentType` resolution, per-task latency, prompt density, or the no-Workflow fallback shape.

Every worker preamble must say: never search outside the working tree. Do not run `find /` or start a glob from `/`; the watchdog will kill that runaway traversal.

## 1. Workflow template notes (`SKILL.md` §5)

**Parallel-with-integration is the default.** `pipeline()` and `parallel()` run their file-changing workers concurrently. For same-tree runs, assign disjoint file ownership up front — pin each worker's target files in its prompt and fix shared contracts/types in the prompts too. When ownership genuinely overlaps, give each worker an orchestra tree (`agent-exec dispatch --isolate always --task <id>`) and have the supervisor integrate afterward with the command below; an `agent()` worker that ran with `isolation: 'worktree'` sits in a Claude Code tree (`worktree-*`) that `isolate integrate` cannot reach — when the user asks for its result, land it with `agent-exec isolate land --path <worktree> -m "<subject>"`, never by merging its branch.

```text
agent-exec isolate integrate --tasks <a,b,c> [--repo <path>] [--onto <ref>] [--into <id>] [--json|--text]
```

`--tasks` is required and takes comma-separated orchestra task IDs in the order to integrate. Same-file edits are not a reason to serialize. The only legitimate reason to serialize is a real dependency in which a later task must consume an earlier task's output or state. Disjoint ownership protects workers from each other; isolation protects the user's uncommitted work.

**The relay runs one command per hop; the script does the waiting.** A command that takes minutes reads to a relay as something to background and poll, and a relay told to poll on its own returned on the first `detached`, copied the worker's long answer instead of the JSON, or summarized it in prose — `JSON.parse` failed and finished work was reported as failed or still running. So `dispatchClass` never asks for that: the start hop's `--detach` returns at once, each wait hop is one `dispatch wait --summary` call that blocks under the relay's Bash limit and returns one short JSON line, and the `while` around them lives in the script (`awaitDispatch`, `workflows/pipeline.js`). A garbled reply is retried as another wait hop — `wait` is idempotent — never as a re-dispatch. If a run still ends in `RelayFailureError`, resume waiting on `err.token`, then check the worktree (`agent-exec isolate diff --task <id> --names-only`) before re-dispatching: the work is usually already there, and `runTask` with `task.preimplementedPath` starts at the review instead of paying for the implementation twice (`references/programme.md` §3 does exactly this).

**On `agentType`:** this plugin ships `agents/orchestra-light.md`, `-deep.md`, `-review.md`, `-relay.md` and `-delegate.md`, and their plugin-scoped names (`orchestra:orchestra-light`, ...) resolve as `agent()`'s `agentType` — `relayHop()` relies on `orchestra:orchestra-relay`. They matter on `dispatchClass`'s fallback branch (`status: 'delegate'` with no `agent_type`, i.e. Claude was the routed candidate) and when writing the express lane / §8 fallback by hand. If one is missing from the available-subagents list, fall back to explicit `model: 'haiku'` / `'opus'` / `'sonnet'`. Either way, rule #4 stands: exactly one of `model` or `agentType` is always explicit — `dispatchClass`'s own branches already guarantee this on the routed path. The `standard` class has no dedicated Claude agent definition — its Claude-side fallback is `model: 'haiku'` inline.

For tasks with modest design latitude, call `dispatchClass('standard', workerPromptFile, opts)` instead of `'light'` — same helper (`workflows/pipeline.js`); `agent-exec route --class standard` resolves it (Claude Haiku first, then pi `openai-codex/gpt-5.6-luna`/high). For tasks with real design latitude, route the work stage to `orchestra-deep` (Opus) instead: `dispatchClass('deep', workerPromptFile, opts)`, or, on the fallback branch, `agentType: 'orchestra:orchestra-deep'` / `model: 'opus'` directly.

## 2. Shortening the critical path

### 5.1 Shorten the per-task critical path — overlap authoring

`pipeline()` already runs tasks concurrently, so the latency you actually feel is not "tasks aren't parallel" — it is (a) the **sequential `work → verify → retry` chain inside a single task**, and (b) **barriers that serialize work with no real dependency** (§5.2). This subsection cuts (a).

**Overlap adversarial-test authoring with implementation, and author once.** §7 measured that the review pass's dominant cost is *authoring* adversarial tests — and those tests derive from the **spec, not the implementation** (the `formatBytes(1048575) → "1 MiB"` boundary test that caught the PoC bug is fully spec-derivable). So author them *concurrently with* the first implementation, and author them **once** — the spec doesn't change across retries, only the implementation does. Each verify step then merely *runs* the pre-authored tests plus a whitebox glance, which is cheap. The pre-authored tests are exactly `orchestra-review`'s probes — one per likely implementation mistake, at least 3 hypotheses — only their authoring moves earlier. They are still probes, not deliverables: the gate that PASSes keeps only those that found a defect and deletes the rest, reporting the survivors in `tests_kept` (`SKILL.md` §6 rule 8).

**The pipeline does this by default.** `workflows/pipeline.js`'s `runTask()` starts `authorProbes()` — a Sonnet `agent()` given only the contract file and the verifier's list of mistakes — in the same breath as the round-1 dispatch, without awaiting it, and awaits it only right before the gate-1 review. It writes throwaway probes to `task.probeDir` (outside every worktree, laid out by repo-relative path) and returns `{files, mistakes}`; `firstGatePrompt()` hands the reviewer that list with "copy them into the tree, run them first, delete them before finishing; keep a probe only if it found a defect" plus a hard 12-invocation budget. Re-gates keep the 6-invocation incremental mode and get no probes. `noProbes: '<reason>'` skips the author; a null or failed author only means gate 1 runs without probes. Why the default: review was 28% of the critical path in measured runs (median 98 s, max 493 s, only 36% of it tool time), and the overlap, documented as an option, was used in 0 of 9 runs. No cache-priming step is needed alongside it: pi already hits its prompt cache ~89% of the time (65-96% on resumed corrections). A task that returns before any review (needs-permission, `ESCALATE` at deep, a FAIL with no findings) leaves its probe author running unawaited — an accepted cost: awaiting it on those paths would put it back on the critical path.

**Note the interaction with §11.2.** Pre-authored tests make the *sweep* cheaper, not optional: the reviewer still classifies each finding by family and checks sibling sites, it just does so against a suite it already has rather than one it writes mid-gate. That is precisely the combination that keeps the whole task inside two gates.

For a change so small that its review is a single obvious assertion, set `noProbes: '<reason>'` and skip the probe author.

### 5.2 Never serialize independent work behind a barrier

The most common reason an orchestration "feels sequential" is a barrier — a `parallel()` between phases, or an `await` — placed where there is no real dependency. The runtime does not add these; the instructor does, by writing phase-by-phase code. Guard against it:

- **Default to `pipeline()`, not phase-by-phase `parallel()`, and integrate rather than serialize overlap.** Writing `const impls = await parallel(tasks.map(work)); const revs = await parallel(impls.map(verify))` forces *every* implementation to finish before *any* verify starts — the single slowest task stalls all reviews. `pipeline(tasks, runTask)` lets each task's verify start the instant *its own* work is done. A barrier between stages is justified ONLY when a later stage has a real dependency on earlier output, such as a global early-exit (`0 findings → skip`) or a synthesis that literally reads every task. Same-file edits are integrated after parallel isolated work; they do not justify serialization.
- **A "final review" is per-task unless it truly reads all tasks.** If a final check only re-validates task A, it belongs *inside* task A's pipeline chain, not in a global barrier that runs after every task finishes. Reserve a single whole-set barrier for a real cross-task synthesis, and scope it to the minimal set of tasks it actually consumes — not "all of them" by reflex.
- **Independent review passes run concurrently, not one after another.** When a task gets both a Claude `review` and an `independent-review` (§9 — a different provider's eyes), the two share no dependency. Dispatch them together — `await parallel([() => claudeReview(...), () => independentReview(...)])` — and merge the two verdicts, rather than `await`-ing one and then the other.

### 5.3 The size gate: split the long pole before dispatch

`pipeline()`/`parallel()` run at most `min(16, cores − 2)` agents at once, but the run's wall clock is its longest single chain. In 7 measured runs the longest task was 89-100% of the wall clock in 5 of them (Opus deep workers, 310-1898 s), only 1-4 of 16 slots were ever used, and in 3 runs the last task ran alone for 50-76% of the run. Splitting that long pole into 2-3 tasks by file ownership would have cut those runs by 48-64%. Per-task fixed overhead on its own chain is ~45-65 s (relay ~16 s, spawn 2-3 s, smallest review 21-47 s) and does not add across parallel tasks, so the target is **120-300 s of worker time per task**.

This is a mechanism, not advice: `sizeGate()` in `workflows/pipeline.js` runs before any dispatch and checks every task's `files_owned` (non-empty array of repo-relative paths, test files included, trailing `/` = directory prefix) and `est_seconds` (integer). Missing either, `est_seconds > 300`, or more than 3 owned paths (`agent-exec wave lint`'s threshold) is a violation, and **any** violation makes the script return `{sizeGate: 'failed', violations: [{id, problems, hint}]}` with nothing dispatched — a failed script costs seconds, a long pole costs the run. The bounded escape hatch is `allowLarge: '<reason>'`: it waives the size checks for that task, is logged, and is echoed in that task's result so the waiver stays visible. A missing `probeDir` (without `noProbes`) is also reported there; `allowLarge` does not waive it.

Splitting helps *only* where file ownership is genuinely disjoint — a split that introduces a cross-task dependency just re-adds the barrier §5.2 told you to avoid. If a task cannot be split that way, say so in `allowLarge`.

### 5.4 The ownership pre-check: a FAIL that needs no reviewer

`files_owned` is also checked after the work. After each work round that returns normally (not `ESCALATE`, `NEEDS-PERMISSION` or `RUNAWAY`), `strayPaths()` spends one haiku relay hop on `agent-exec isolate diff --task <id> --names-only --no-collect` and compares its `files` with `files_owned`: exact match, or a directory prefix for an entry ending in `/` (a trailing `/**` glob reads the same way), so `src/` owns `src/x` but not `src2/x`; paths under `.orchestra/` are ignored. Any path outside ownership skips the reviewer entirely: `ownershipVerdict()` builds a FAIL with one finding per stray path (`family: 'ownership'`, `cited_contract: 'files_owned'`, `expected: 'only files_owned change'`, `actual: <path>`), and it goes through `writeCorrectionPacket()` like any review FAIL, consuming the gate. Since no review ran, the next review is the full first-gate one (with the probes), not a re-gate. The check never blocks: no `files_owned`, a preimplemented tree, or a failed/unparsable hop logs and falls through to the normal review.

**Why `--no-collect`.** A plain `isolate diff` marks the reported content collected. On the common PASS path — the reviewer copies its probes in, finds nothing, deletes them — the tree is then byte-identical to what the pre-check collected, and the run-end `isolate sweep` would remove the worktree and its branch before you ever took the diff. `--no-collect` reports without touching the collected marker, so the sweep keeps the tree.

## 3. Prompt density (`SKILL.md` §6)


### 6.1 Prompt density: compress the scaffolding, never the spec

Worker and verifier prompts are read by cheap models, not humans. They owe nothing to readability, politeness, or a target language — write them terse, imperative, and in **English** (more token-efficient than Japanese for the same content, and the workers need no Japanese). Drop honorifics, hedges, and prose transitions; nominal/telegraphic style is fine. This trims the instructor's authoring output and the worker's input at essentially no risk.

**But this applies only to the scaffolding — never to the contract itself.** The token-compression techniques that circulate for chat replies (caveman/genshijin-style particle-dropping, "essence only") are an *output*-compression trick, and their own authors report the catch: on complex tasks completeness drops and internal thinking tokens balloon (+200–400%), erasing most of the nominal saving. A worker's job *is* the complex-task case, so:

- **Do NOT compress the spec.** The `formatBytes(1048575) → "1 MiB"` boundary example (requirement 1 above) cannot lose a character without losing meaning, and the one example you delete to save tokens is exactly where §7's rounding-carry class of bug hides. Enumerated I/O examples, boundary values, and the verification command are compression-exempt.
- **Reduce variance with structure, not with prose compression.** Terse natural language is *more* ambiguous, not less. When you want to pin down behaviour and kill wording drift, reach for tables, enumerated example rows, and the response `schema` — structure removes ambiguity; dropping particles adds it.
- **Do NOT compress worker/verifier *output*.** It is already lean: the verdict is forced into `VERDICT_SCHEMA` JSON and code/log/diff pasting is already forbidden (§6-3). The review pass's main output is adversarial *test code*, which does not compress. Light-class workers (Claude Haiku first, pi Luna/medium as fallback) think little at their cheapest effort setting, so the thinking-inflation risk is low there — but never ask the Sonnet reviewer to write tersely at the cost of the tests it authors.

Rule of thumb: **strip everything a human would want and a machine does not; keep every concrete fact the worker must reproduce exactly.**

### 6.2 Never rely on "asc"/"desc" for a semantic order

When a spec defines a sort or ranking over a domain ordering (priority, severity, status), do not say "ascending"/"descending" or `order=asc|desc` and leave the reader to infer which end is which. Enumerate the expected order with a concrete example instead: `sort=priority&order=asc` returns `low, medium, high`; `order=desc` returns `high, medium, low`. In the 2026-10-08 re-bench every model that scored 36/38 read `asc` as "most urgent first" and failed exactly those two checks — Claude Haiku 5.5 did so in all 10 of its runs across four harnesses — while gpt-5.6-luna, gpt-6.1-sol and MAI-Code passed the same wording. This matters most for light-class work, which now goes to Claude Haiku first (`poc-findings.md` §11).

## 4. Findings proven by the PoC



From the measured PoC (3 tasks in parallel, 8 agents, 4 min 23 s, 246k total subagent tokens):

- **4 `light`-class (Haiku) implementations**: 9,638 output tokens total. **4 Sonnet `review` passes**: 19,135 output tokens total. Review passes consume roughly 2x the implementation's output tokens — the main cost is authoring adversarial tests.
- **The instructor consumed zero tokens during execution.** What came back to the instructor was ~2KB of structured JSON.
- **Review value, demonstrated (formatBytes task):** all 11 of the implementation's self-written tests passed, but they didn't cover boundary values. The review pass's added adversarial test caught a rounding-carry bug (`1048575` → returned `"1024 KiB"`; correct is `"1 MiB"`). One feedback-driven retry fixed it and passed.

That 2x buys defect detection, not permanent test assets: the PoC reviewer's value was the finding, not the file. Observed: a 3-task run landed 1,050 test lines for 340 implementation lines, and 134 of them were deletable with every suite still green — which is why `SKILL.md` §7 has you read `tests_loc_added / impl_loc_changed` on every PASS.

Conclusion: adversarial review costs ~2x tokens but catches bugs that the implementation's self-attested tests structurally cannot. Skip review only for tasks whose review procedure is completely self-evident and low-risk.

## 5. Fallback (environments without the Workflow tool)



Where Dynamic Workflows are unavailable or disabled, fall back to a 3-level nesting: launch the `orchestra-delegate` subagent (Sonnet) via the Agent tool:

```
Instructor (Fable/Opus)
  └─ Agent tool: launch orchestra-delegate (no model needed - pinned to sonnet in its own frontmatter)
       └─ orchestra-delegate itself dispatches each round detached (`dispatch prepare` → `--detach` → `dispatch wait` hops)
            (no relay needed - it already has Bash) - whatever route resolves (Claude Haiku first, pi as fallback); orchestra-light/haiku
            only on that call's own "delegate" fallback, never launched directly
       └─ orchestra-delegate internally launches orchestra-review (sonnet) via the Agent tool
       └─ on FAIL, orchestra-delegate re-dispatches a self-contained correction packet
            to a FRESH worker and runs one incremental re-gate (cap: 2 gates, §11)
  └─ instructor receives only the structured verdict from orchestra-delegate
```

Two gates without a pass, a re-gate reporting `new_family`, or an `ESCALATE` at `deep` all come back as `needsInstructor` — a request for re-analysis, not a request for more rounds. Answering it by telling the delegate to try again is exactly the loop §11.2 exists to break.

Across those instructor-driven rounds, do not spawn a fresh `orchestra-delegate` each time — resume the same instance with `SendMessage` so it keeps its context (previous failures, approaches already tried) without re-explanation. This is the supervisor-level exception to §11.3's "fresh invocation" rule, which governs the *implementation* worker: the delegate's accumulated context is the thing of value, whereas the failed worker's is a liability.
