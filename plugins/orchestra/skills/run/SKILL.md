---
name: run
description: Playbook for cost-tiered multi-agent delegation. The instructor (Fable/Opus main session) only decomposes tasks, writes contracts, and writes the Workflow script; implementation goes to a cheap tier resolved by `agent-exec route` (Claude Haiku for light work, pi `openai-codex/gpt-5.6-luna` for standard work, Sonnet as fallback), an adversarial review pass checks the result, failures retry with precise feedback, and only structured verdicts flow back. Invoke with /run (cross-plugin: `orchestra:run`), or whenever cost-tiered delegation or large parallel task execution is called for. Also the ORCHESTRATED lane of the <orchestra-router> protocol this plugin injects at SessionStart.
when_to_use: Use when delegating multiple tasks in parallel to cheap models, when building a cheap-implementation + adversarial-review pipeline, or when the instructor must receive only structured verdicts — never logs, diffs, or intermediate artifacts.
---

# run: cost-tiered orchestration

You are the **instructor** — the main session, on an expensive model. This skill is your code of conduct plus the Workflow script you write.

**The loop:** classify (§2) → decompose into tasks with contracts (§6) → write the Workflow script (§5) → receive only structured verdicts. Nothing else enters your context.

**Load a reference file only when its question actually comes up:**

| File | Read it when |
|---|---|
| [`references/workflow-template.md`](references/workflow-template.md) | **Always, before writing or adapting a Workflow script** (§5): the full ready-to-copy script, the run-id rule, and the relay protocol. |
| [`references/authoring.md`](references/authoring.md) | Writing the script and you hit: parallel file-mutation safety, `agentType` resolution, per-task latency, prompt density, or the no-Workflow fallback shape. Also holds the PoC numbers behind §7. |
| [`references/gates.md`](references/gates.md) | A gate rule (§11) is biting and you need the why: a reviewer's rejection looks out of scope, rounds keep closing one sibling at a time, you must decide whether to spend a third round, or you need why corrections resume a session. |
| [`references/isolation.md`](references/isolation.md) | A worker will touch a tree holding uncommitted work (isolation is the default there), or the run needs rollback on failure, a real diff for the reviewer, worktree-isolated parallelism and `isolate integrate`'s flags, competing implementations of one contract, the shared-stash rule, or an approval budget for non-interactive workers. |
| [`references/config.md`](references/config.md) | A configuration question actually arises: key semantics, four-layer merge, why `route` dropped a candidate, `enforcement.light_class`, telemetry fields. |
| [`references/external-executors.md`](references/external-executors.md) | Choosing or second-guessing a pi model+effort, or needing the raw CLI recipe / pricing. pi reports per-run usage and a list-price cost estimate (a real charge only on metered providers such as `github-copilot/*`). |
| [`references/poc-findings.md`](references/poc-findings.md) | Making a model-policy decision that depends on specific benchmark numbers. |
| [`references/programme.md`](references/programme.md) | The work is dozens of session-sized packages with ordering between them (an audit-then-refactor, a multi-feature build-out): agent-produced plans, dependency scheduling, a rolling integration base, carry-over, chunk budgets. §13. |

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
| `deep` | `agent-exec route --class deep` — Opus (`orchestra-deep`) first, then pi `openai-codex/gpt-6.1-sol`/high | Real design latitude: algorithm choice, API shape, tradeoffs |
| `standard` | `agent-exec route --class standard` — pi `openai-codex/gpt-5.6-luna`/medium by default, then Sonnet | Ordinary implementation; review needing adversarial test design and failure interpretation |
| `light` | `agent-exec route --class light` — Claude Haiku first, then pi `openai-codex/gpt-5.6-luna`/medium | Implementation or review whose procedure is 100% prescribed |

**Review class:** `standard` is the default. `light` suffices only when the review procedure is fully prescribed (exact commands, explicit pass criteria). For review needing heavy design judgment, pass `model: 'opus'` inline — there is deliberately no `deep`-class review agent. The template's own same-run `review` stage stays pinned to Sonnet and unrouted, because `priority.review` is `[claude]`-only by design.

Defaults are overridable per project/user — see `references/config.md`.

## 4. The one rule that matters most

> **Omitting `model`/`agentType` makes the spawned agent inherit the session model — yours.** An implementation agent you forgot to pin runs at Fable/Opus cost and the cost-tiering becomes pointless. This holds for Workflow `agent()` calls and for the Agent tool alike (frontmatter default is `inherit`).
>
> **Every single agent invocation sets `model` or `agentType` explicitly. No exceptions.**
>
> Corollary: the instructor tier is never a worker tier. A file-changing subagent must never run on the instructor's own model — Fable especially, whose role is orchestration plus the small direct edits §2 allows. Pass an explicit Opus/Sonnet/Haiku (or let `agent-exec route` resolve an external executor); never let it inherit.

## 5. Workflow template

The full script — `VERDICT_SCHEMA`, the relay helpers (`relayHop()`, `awaitDispatch()`), `dispatchClass()`, the correction-packet helpers, `runTask()`, the `pipeline()` call, and the run-end sweep — lives in [`references/workflow-template.md`](references/workflow-template.md). Read it in full and copy from it whenever you write a Workflow script; do not re-derive the helpers.

**What it does.** `dispatchClass()` sends each task at its capability class through a relay that runs `agent-exec dispatch --token … --capture --detach` and then `agent-exec dispatch wait` hops until the status is final; `agent-exec route` does the executor selection, and the script only keeps the sticky `exhausted` set. `runTask()` gates the work with a Sonnet review forced into `VERDICT_SCHEMA` JSON — at most `MAX_GATES = 2`: one initial gate, then one incremental re-gate after a cited, family-swept correction packet that the script itself writes — escalates the class instead of spending a gate on `ESCALATE`, a twice-failing family, or a second runaway, and returns one structured verdict per task. `pipeline(tasks, runTask)` runs every task's chain independently, with no barrier between stages.

**Hard rules for every script:**

- `export const meta` is a pure literal (no variables/calls/interpolation); no TypeScript syntax; no `Date.now()`/`Math.random()`. Plain JavaScript.
- Every `agent()` call sets `model` or `agentType` (§4) — relays, reviewers, fallbacks, and the cleanup relay included.
- Every `agent()` call that edits or reviews files starts with `treeLine()` for the task's isolated tree (`isolation.workdir`, falling back to `isolation.path`), or gets `isolation: 'worktree'` when dispatch could not isolate. The reviewer and the correction round land in that same tree.
- Relay prompts carry one command, never a payload. Worker prompts are FILES baked into the dispatch token at `agent-exec dispatch prepare` time (§6 rule 6), and so is `--run-id`; the relay gets only the token. Every loop and wait lives in the script, not in a relay.
- End every run with the `agent-exec isolate sweep --include-current --text` relay (below).

**Same-tree parallelism safety.** `pipeline()`/`parallel()` run file-changing workers concurrently against the **same working tree** — two workers with overlapping file ownership silently corrupt each other. Pin disjoint target files (and shared contracts/types) in each worker's prompt, and keep workers small. Full guidance: `references/authoring.md` §1.

**Fan-out variant.** When several tasks must touch the same file but their interfaces can be frozen up front — exact key names, function signatures, CLI flags, JSON shapes, written once and pasted verbatim into every contract — give each worker its own worktree, run them in parallel, then `agent-exec isolate integrate --tasks <a,b,c>` and run one mandatory integration gate on the merged tree.

**Decision rule: serialize only for a real ordering dependency** — one task must read another task's *output* to do its own work. Two tasks editing the same file is not that; it is a merge, and merges are mechanized now. If the interface between the tasks can be written down before either starts, the dependency is on the contract, not on the code, and the tasks run in parallel.

**Isolate every file-changing worker, dirty tree or not.** A prompt that names the files a worker owns does not constrain the worker — only a separate tree does: a documentation-only worker told it owned three `.md` files truncated an implementation file to zero bytes, and workers of every model tier run `git checkout --`/`restore`/`reset --hard` over changes they did not author. So give every file-changing worker its own tree — `agent-exec dispatch --isolate always --task <id>` for CLI executors, `isolation: 'worktree'` for `agent()` calls; `--isolate auto` only covers the dirty-tree case. **The `status: "delegate"` path needs this said out loud:** dispatch does not run that worker, *you* do, so an `agent()` call that omits both the worktree and the working-directory line puts Claude-routed workers straight into the user's tree while CLI-routed ones stay isolated. dispatch has already created the task's tree by then: point the worker at `isolation.workdir` (falling back to `isolation.path`, the worktree root that `agent-exec isolate diff/remove --task <id>` operate on) rather than opening a second one, and carry the same path into the correction round. Collect with `agent-exec isolate diff --task <id>`; if you take the result out by any other route (an integration worktree, above all), run `agent-exec isolate collect --task <id>` so the SessionEnd hook can reclaim it too. Full guidance: `references/isolation.md` §0 and §2.

**On `agentType`:** the plugin-scoped names (`orchestra:orchestra-light` / `-deep` / `-review`) may or may not resolve as `agent()`'s `agentType` in this environment — check the available-subagents list before relying on them, and fall back to explicit `model:`. See `references/authoring.md` §1.

**Latency:** for the per-task critical path (overlapping adversarial-test authoring with implementation, avoiding needless barriers, sizing tasks to the concurrency width), see §7 and `references/authoring.md` §2.

**Follow-up rounds:** when a subagent *you spawned with the Agent tool* needs another pass after it has returned, resume that same instance with `SendMessage` rather than spawning a fresh one — it still holds what already failed and what it has tried, so nothing needs re-explaining. **Never `SendMessage` an agent that a running Workflow spawned**, nor one that has not returned yet: observed, a message to a running workflow engineer resumed a *copy* of it, and the copy and the original committed into the same worktree seconds apart. Corrections inside a Workflow go through its own gates (§11); the `worktree_lease` guard denies a second writer in a worktree if it happens anyway.

**At run end, always:** have a cheap haiku relay agent run `agent-exec isolate sweep --include-current --text` — never yourself, and never conditionally. This is the *primary* worktree cleanup, not a nicety: the SessionEnd hook cannot be relied on (a killed or crashed session never fires it, and a slow removal gets cut off by the teardown timeout), whereas run end is a moment you control and every diff has already been collected by then. The sweep keeps anything still holding uncollected work, so it cannot destroy a verdict you have not read. It also keeps an integration worktree (`--into`) until its branch has landed on a non-orchestra local branch (merged, or squashed so the integrated files match), so the always-sweep never takes the tree a programme still verifies, deploys or squashes from; once it has landed, the next sweep or `isolate remove --task <into>` reclaims it. Start the sweep only after the work in surviving trees has finished: it reports a tree `busy` while a subagent holds its writer lease or activity stamp or `isolate integrate` holds its lock, but a background process or a CLI executor running outside the hooks leaves no stamp. If `doctor`'s `config.values.telemetry.enabled` is true, have that *same* relay also emit one `run_summary` via `agent-exec telemetry record --json '...'`; if disabled, skip that half silently (§10). If you ran without a Workflow (§8), run the sweep yourself once the delegate returns.

**Then report what the run cost, every run.** Have the same relay also run `agent-exec usage --run <workflow-run-id> --text` and return it verbatim, then tell the user, per source: input split into fresh / cache write / cache read, output, and the estimated cost with its `~$` figure. Never report only input and output: cache reads are the bulk of the volume and the cache writes are priced above plain input, so in/out alone neither shows the real volume nor supports a cost. Quote `PARTIAL: unpriced …` when it is present — it means a model with no list price is *not* in the figure — and say the figure is an estimate from list prices (the report carries the price-list date), not an invoice. Without a run id, use `--session <session-id>`; use a `--since` window only as a last resort, since a window also counts unrelated sessions.

## 6. Writing worker prompts

Mandatory for every `workerPrompt`:

1. **Concretize the spec to literally-implementable level.** Not "handle boundary values correctly" — enumerate input/output examples. `formatBytes(1048575)` must return `"1 MiB"`, not `"1024 KiB"` (rounding-carry boundary).
2. **State the verification command.** A concrete runnable command the worker executes itself (e.g. `npm test -- formatBytes.test.js`). Without one it can only self-attest.
3. **Constrain the response format.** Explicitly forbid pasting code, logs, and diffs — `orchestra-light`'s system prompt already enforces this, but restating it is safer when calling `agent()` directly from a Workflow.
4. **Name the files the worker owns** — and, when other workers run concurrently, say that everything else is off-limits (§5's same-tree safety rule, or give it a worktree per `references/isolation.md`).
   When choosing a replacement identifier or name, grep the project for collisions first.
5. **Permit escalation explicitly.** State that a decision the packet doesn't settle, a change crossing the ownership boundary, or a failure it can't explain locally should come back as `ESCALATE` rather than a guess (§11.3).
6. **Keep relay-dispatched worker prompts on disk.** Write the prompt to a FILE and give the relay only its path. Never put task text, a base64 blob, or any other payload in the relay's prompt: it is a model and will act on the text or fail to reproduce it verbatim. The one sanctioned exception is `writeCorrectionPacket()` (§5): the packet rides inside a quoted heredoc to `agent-exec write-file --expect-chars N`, so the relay can only paste it, and a mangled paste is refused instead of written. Split shared preamble and per-task contract into separate files and pass `--prompt-file` twice rather than duplicating the preamble.
7. **A correction packet has two shapes, chosen at runtime.** When the round-1 executor's session can be resumed (§11), the packet is a *delta*: just the rejection findings with their `cited_contract`, the `must_not_change` paths, and what the next gate checks — the session already holds the contract. When there is no session to resume (a Claude tier, or a task with no correction tokens prepared), the packet must stand alone: the original contract in full, plus the same findings/`must_not_change`/next-gate text, plus the fact that the previous attempt's files are still on disk. `correctionPacketBody()` in §5 builds both shapes from one source so they never drift; `correctionPacket()` wraps the full shape for the no-session fallback path.

8. **Name the implementation mistakes to detect, not a case table to satisfy.** Rule 1's enumeration is for *spec clarity*; this rule governs which of those examples become *tests*. A case table handed over as "tests to write" reads as a floor: the worker satisfies it and then adds more. Instead state what is likely to go wrong — "`current.version || -1` silently maps 0 to -1", "guregu's `Update.If` ANDs successive calls, so a three-way OR must be one `If`". A test that detects an anticipated mistake earns its maintenance cost; a test that proves the specification does not.
   **Do NOT require tests for:** a constant expression's exact value (a jitter formula, a default timeout); behavior the change does not touch; the proposition "this behavior is unchanged" (the existing suite is that guard); elapsed wall-clock time.
   **When the contract says a behavior must not change, say explicitly: "do not add tests to prove it."** Otherwise the worker returns the proof as a deliverable.
   **Carry the probe discipline into every `verifierPrompt` verbatim.** §5's review is `agent()` with `model: 'sonnet'`, not the `orchestra-review` agent, so it never loads that agent's system prompt — paste its step 4, the "never keep" list, the structure-pinning test, and the `git diff --numstat` measurement paragraph from `agents/orchestra-review.md`, and name the mistakes to probe for. A reviewer told nothing writes tests, keeps them all, and leaves `tests_kept` empty.

**Density:** write worker/verifier prompts terse, imperative, English — they are read by cheap models, not humans. **Compress the scaffolding, never the contract:** enumerated I/O examples, boundary values, and the verification command are compression-exempt, and structure (tables, example rows, the response `schema`) beats terse prose for removing ambiguity. Reasoning and the thinking-inflation trap: `references/authoring.md` §3.

## 7. Latency and review economics

- **Default to parallel work followed by integration, never serialization for file overlap.** A barrier is justified only when a later stage has a real dependency on earlier output, such as a global early-exit or synthesis that literally reads every task. Same-file edits are handled with isolated worktrees and supervisor-run integration, not by making otherwise independent tasks wait.
- **Author adversarial probes from the spec, concurrently with the first implementation, once.** Probes derive from the spec, not the implementation, so they need not be re-authored per retry; each verify then merely *runs* them. Which probes stay as permanent tests is decided at the gate that PASSes — the ones that found nothing are deleted there.
- **Size tasks to fill the concurrency width** (`min(16, cores − 2)`): not so coarse that slots idle, not so trivial that spawn overhead dominates. Split only where file ownership is genuinely disjoint.
- **Review costs ~2x the implementation's output tokens and is worth it** — the PoC's reviewer caught a real boundary bug the worker's own passing tests missed (`references/authoring.md` §4). Skip review only when a task's verification is completely self-evident and low-risk — which is exactly what the express criteria (§2) carve out.
  That 2x buys **defect detection**, not permanent test assets: keep the test that guards a found defect; delete the probes that found nothing. Read `tests_loc_added / impl_loc_changed` on every PASS — above ~3, open `tests_kept` and strip entries whose `mistake_detected` is a specification restatement (a constant's value, unchanged behavior, wall-clock time) before landing the diff.

Measured numbers, the restructured `runTask`, and the full barrier/sizing rules: `references/authoring.md` §2 and §4.

## 8. Fallback (no Workflow tool)

Launch `orchestra-delegate` (Sonnet, pinned in its own frontmatter) via the Agent tool; it runs `agent-exec dispatch --class light --capture` per round itself, spawns `orchestra-review`, applies the same two-gate discipline (§11) — one correction round with a cited, family-swept packet to a fresh worker, then an incremental re-gate — and reports back only the structured verdict. When it returns `needsInstructor`, that is your re-analysis cue, not a signal to tell it to try again. Full shape: `references/authoring.md` §5.

When a verdict has `needsPermission: true` (or a wave has pending `agent-exec approvals`), **you decide** each denial yourself against the package's spec, using this policy:

- **Allow without asking:** package registries and mirrors of the project's toolchain; official docs/spec/standards hosts the package plausibly reads; toolchain caches and config dirs a build tool writes; dialogs for actions confined to the package's worktree that match its spec.
- **Deny with a reason, no user question,** when the subject is unrelated to the package's spec — the package continues without it.
- **Ask the user (`AskUserQuestion`) only for:** credentials and secret stores; writes outside the repo and toolchain caches (other repos, home config); hosts that accept uploads or messages (paste/file sharing, webhooks, chat/issue APIs, git push targets); cloud, internal, corporate or production endpoints; anything that deploys, publishes or pushes; and whenever the relation to the spec is unclear.
- Always give `--reason`. In wave runs prefer `agent-exec approvals decide --state <state> <item> allow|deny --reason TEXT` over raw `sandbox allow`/`allow-domain`: it applies the grant, resumes the package and audits the decision. Outside wave runs use `agent-exec sandbox allow <path> --reason TEXT` / `allow-domain <host> --reason TEXT`, then re-dispatch the **same task id**: the session store resumes the same executor session and keeps its learned cache state. A denied path goes into the correction packet as off-limits to the worker. The hard refusals (`/`, `$HOME`, `~/.claude`, `deny_read`, tamper paths; `*`, `*.tld`, IPs, localhost) hold for every decider.

A sandbox denial is not executor exhaustion and must not consume a gate.

## 9. Configuration and external executors

Everything you normally need is one call: **`agent-exec doctor --json`** returns both the readiness verdicts (`ready.<executor>.ok`) and the resolved config (`config.values` — `tiers`, `external_executors`, `priority`, `telemetry.enabled`) already deep-merged from all four layers (defaults ← `~/.claude/orchestra.yaml` ← `.claude/orchestra.yaml` ← `.claude/orchestra.local.yaml`).

**Run this `doctor --json` check yourself, once, before writing the Workflow script — not delegated to a relay.** If its `shim.installed` is false, STOP and tell the user to run `agent-exec install` (or point them at the `setup` skill) before proceeding. A missing shim is not a hard error anywhere downstream — `route`/`dispatch` just gate every external executor on `ready.<x>.ok` and fall through to `claude`, exactly as designed for a machine with no pi — but that silent, correct-looking fallback means every task in the run bills the user's OWN Claude quota instead of the external executor they configured, with no error to notice until it is gone. Observed: a run with the shim missing exhausted a session's entire weekly quota on work that should have gone to pi. If `ready.pi.missing` contains `executor-binary-unavailable` and doctor gives a `hint`, relay that hint to the user; do not install silently mid-run.

**You never walk the priority list.** `agent-exec route --class <cls>` does: it gates every candidate on reality (disabled, binary missing, `ready.ok` false, no `class_policy` for the class, or listed in `--exhausted`) and returns the survivor; `agent-exec dispatch` additionally runs a `dispatch: cli` winner. pi ships `enabled: true`, which is safe precisely because of that gate — on a machine without it, everything resolves to `claude`.

**Unavailable ≠ failed.** Only a genuinely unavailable executor (quota, credits, auth, unresolved binary) is a fallback signal, and it becomes sticky via `exhausted` for the rest of the run. A wrong *result* is not — it stays on the same executor and goes through the normal review/retry loop.

Key semantics, the merge algorithm and its upgrade trap, `enforcement.light_class` and its escape hatches, the `dispatch: cli` sandboxing caveat, and the full commented schema: `references/config.md` §1 (schema copy also at `examples/orchestra.yaml`). To change any of it, use the `setup` skill rather than hand-editing.

## 10. Telemetry

Opt-in, default off, anonymized: `agent-exec` enforces a field/value allowlist, so prompts, paths, ids, and free text are structurally unrecordable. Two sources: `agent-exec dispatch` self-logs one `dispatch` record per dispatch (LLM-independent), and you emit exactly one `run_summary` at run end through a haiku relay when enabled. Fields, storage layout, and the `record`/`show`/`archive`/`clear` CLI: `references/config.md` §2.

Measure one workflow run with `agent-exec usage --run <workflow-run-id>`; this is exact rather than a time window. `--run-id` sharpens the ledger's session key to a run key and should still always be passed — at `dispatch prepare` time, never on the relay's `--token` call — but omitting it now costs precision rather than the measurement itself.

## 11. Gate discipline

Rounds are the pipeline's real cost. Slow runs are usually not slow because a model was weak, but because rounds were spent badly. `MAX_GATES = 2` replaces the old blind 3-retry count; these rules are what make two gates enough.

- **A rejection needs a citation.** FAIL only on contract text the work violates; `cited_contract` is required on every finding. Non-contractual improvements go to `optional_hardening` and never block. Applies to your own correction packets too — your mid-run inference is not a requirement.
- **Findings carry a defect `family`; the reviewer sweeps siblings before reporting.** One round closes the class, not one instance.
- **Gate 2 is a re-gate, not a re-audit** (`regatePrompt()`, §5). If it reports `new_family`, the loop stops and hands back to you: the first sweep was wrong, so another automatic round just finds the next sibling.
- **Escalate the class on `ESCALATE` or a twice-failing family** — that doesn't consume a gate, since a mis-sized packet is not a defect. Start auth/session/concurrency/security work at `standard`/`deep` rather than proving it through a cheap failure. Escalation never widens scope.
- **Corrections resume the round-1 executor's session when one exists.** `runTask()` sends a correction round back through `dispatchClass()` with a per-task correction token, so agent-exec's session store picks the SAME executor back up with a delta packet. A class escalation always forces a full, non-resuming token. Only a Claude-tier round 1, or a task with no correction tokens prepared, falls back to a fresh pinned-Sonnet `agent()` call with the full packet. Why: `references/gates.md` §5.
- **Budget invocations:** ~12 shell calls per worker and per review, 6 per re-gate. An interrupted or budget-exhausted review is a FAIL, never a PASS.
- **Sandbox and watchdog outcomes are control signals, not ordinary defects.** `needs-permission` returns immediately without exhausting an executor or consuming a gate. The first `runaway` gets one synthesized `runaway` finding and a correction packet; a second one escalates the class. After a review FAIL, any non-auto sandbox denial returns `needsInstructor`/`needsPermission` rather than spending a correction round.

Rationale, failure modes, and the family taxonomy: `references/gates.md`.

## 12. Rollback, diffs, and aggressive isolation

VCS is effectively free, and a run that can return to a known-good state can afford to be aggressive.

- **Baseline, then snapshot between attempts.** Take a baseline before the first worker (`git init` if the tree isn't a repo) and snapshot between attempts: the reviewer then diffs against something real instead of inferring the worker's footprint from prose, and a bad attempt is rolled back instead of patched over.
- **Parallel-with-integration is the default.** Run independent tasks concurrently. When their paths overlap, isolate them in worktrees rather than serializing them, and have the supervisor integrate afterwards with `agent-exec isolate integrate --tasks <a,b,c>` (task IDs in integration order; integrations into one `--into` target serialize on a per-target lock, exit 6 = `locked`). Same-file edits are not a reason to serialize; the only legitimate one is a later task consuming an earlier task's output or state.
- **Isolation is the default whenever the tree is dirty.** `agent-exec dispatch` isolates on its own (`--isolate auto`, needs `--task <id>`), covering CLI executors that `agent()`'s `isolation` option cannot reach; `--isolate always` isolates a clean tree too, `never` opts out. One worktree per `--task` is reused across retry rounds. The OS sandbox is a second boundary: isolation decides where work lands, the sandbox decides what the worker can touch.
- **Isolation lifts the disjoint-file-ownership constraint** (§5): use it to parallelize genuinely tangled work, make exploratory failures free, and run **competing implementations of one contract**, where variant disagreement is a defect report about your *spec*. N× tokens, so spend that variant-fanout on the risky core.
- **Workers never touch VCS state;** snapshots, merges, and cleanup are the supervisor's — and cleanup has a deadline: `agent-exec isolate sweep --include-current` at run end (§5), because the SessionEnd hook is insurance that a killed session never collects. Never commit to, rebase, or push the user's branch without an explicit request.
- **The stash stack is shared** by every worktree of a repository, so workers must not `git stash`; give them `agent-exec shelf push [--keep-index] [-- PATH...]` / `shelf pop` instead, and say so in every worker prompt — CLI executors (pi) run outside Claude's hooks, so the `hooks/guards/worker-vcs.ts` guard cannot stop them.

Integrate flags and lock semantics, what a worktree carries in (uncommitted work, CoW-cloned dependency dirs, gtr), sweep's live window, branch hygiene, and the stash incident: `references/isolation.md`.

## 13. Programme scale

When the request is dozens of session-sized packages with dependencies between them, §5's `pipeline(tasks, runTask)` is the wrong outer loop: packages need each other's *code*, HEAD moves under every package while it runs, and the plan itself must be produced by agents first. Read `references/programme.md`: planning workflows, a dependency scheduler, a rolling integration base, and carry-over instead of escalation, all reusing §5's `runTask` unchanged per package. **Budget first**: chunk the programme (`maxPackages`, default 12; `maxInFlight`, default 4), return the pending set, and show `agent-exec usage --run` between chunks.
