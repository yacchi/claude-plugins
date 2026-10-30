# Programme-scale runs: planning, dependency scheduling, rolling integration

Read this when a request decomposes into **dozens of session-sized packages** with ordering between them — an audit-then-refactor, a multi-feature build-out, a migration — rather than a handful of independent tasks. §5's `pipeline(tasks, runTask)` assumes the tasks are independent and integrated once at the end; at programme scale that assumption breaks in three ways: packages depend on each other's *code* (not just a contract), HEAD moves under every package while it runs, and the plan itself has to be produced by agents before any of it can be scheduled.

The shapes below have run end to end at programme scale (hundreds of packages, dozens of hours). At that scale an unpinned agent multiplies into most of the bill, so §1's budget rules come before everything else.

## 1. Budget before anything else

- **Pin every agent.** Implementation goes through `dispatchClass()`/`runTask()` (§5 of the skill, in `workflows/pipeline.js`), review stays on Sonnet, planning runs in §2's stages, each agent pinned by `agent-exec plan next` (planner, spec writers) or by §2's table (scouts, checkers), relays are Haiku. §4 of the skill applies to every line of every script here.
- **Run continuously; fix any budget up front.** Do not chunk the programme. Start it once and let it flow: every package that may start does (`wave run --max-in-flight auto`, the default), and it stops only on `agent-exec wave stop --state ...` or at a budget agreed with the user *before* the start, if they want one (`--budget-usd X` with `--run-id <id>`: once the run's estimated cost, the figure `agent-exec usage --run <id>` prints, reaches X, nothing new starts and the packages in flight finish). Why: in a 123-package run, 8 chunk stops each drained 40-60 min while the slowest in-flight package finished (8.5 h of 18.3 h wall), against 0.9 h of restart gaps in total.
- **Never manage machine load with the width.** Do not lower `--max-in-flight` or cap packages (`--max-packages`) because the machine is small or busy: heavy commands and agent-exec's own worktree checkouts and deletes are admitted machine-wide (`throttle.heavy_slots`; tests, lints and typechecks on `throttle.test_slots`; `config.md` §10), and the runner holds new starts by itself while the OS reports memory pressure (`wave.memory_guard`, `config.md` §9). It also holds all new package starts while the cached Claude 5-hour usage is at the stop level, `max(enforcement.usage_guard, 95)` (`"off"` or `ORCHESTRA_USAGE_GUARD=off` disables), with the same `start-held`/`start-resumed` events, re-checked every 30 s; in-flight packages continue, and a stale or missing reading, or a window already reset, never holds. There is nothing machine-related for you to judge.
- **Relay usage, never stop for it.** Every `wave.usage_every` integrated packages (default 10) the run emits a `usage` event (packages integrated / in flight / pending, and the estimated cost so far when it has a `--run-id`) and runs `--notify-cmd` with `WAVE_EVENT=usage`; pass the numbers on to the user as they come. The notice never pauses anything; `agent-exec usage --run <id>` gives the full breakdown at any time.
- **Plan-usage guard.** The `Agent`/`Task`/`Workflow` hook denies spawns while the Claude plan's five-hour window is at or above `enforcement.usage_guard` (default 95; `"off"` or `ORCHESTRA_USAGE_GUARD=off` disables). Below that, `enforcement.usage_advise` (default 75) only advises: one note per 5-point step, and the runner paces to one new start per round (`usage-advice`). Escape: `[orchestra:allow-usage: <reason>]` in a main-thread Agent call's description or prompt, twice per session; it fails open without a reading. The hook does not cover `wave run`'s hosted workers; the runner's hold below does.
- **Two gates, not three rounds.** The reference run allowed up to three review rounds per package on Opus. `runTask()`'s two-gate discipline (§11) is the cheaper equivalent and already escalates class on its own.

## 2. Planning (read-only, in three stages)

The plan is too large for the instructor to write by hand and too important to take from one cheap agent. Planning runs in three stages, and `agent-exec plan next` (skill §14) decides every step from the files on disk. Stage 1: the instructor spawns one `orchestra:orchestra-planner` at `tiers.planner`'s model and effort (default Opus/xhigh), which writes only the skeleton: `plan.json` with no spec bodies, `<specs dir>/preamble.md`, and the frozen interface files `<specs dir>/interfaces/<id>.md`. Stage 2: the planner has the skeleton reviewed (`agent-exec plan review`, see "The review and the redo gate"). Stage 3: `plan next` issues one short-lived `orchestra:orchestra-spec-writer` per package, which writes that package's spec. Why: one max-effort step that read the code, decomposed, froze interfaces and wrote every spec was slow even for light plans, and its complexity grew with the plan; an xhigh skeleton plus parallel short-lived writers is faster, and the gate keeps max effort for plans that need it (a mechanism, not the instructor's guess). The instructor only runs `plan next` and spawns what it answers. The planner reads the code itself first. Children are the exception, two types only (`orchestra:orchestra-scout`, `orchestra:orchestra-checker`), under the fan-out guard's caps (at most 6 per planner, 2 alive at once); the planner does the synthesis, judging, reconciling and skeleton writing itself. Everything is read-only against the repository; output goes to a specs directory on disk, and the instructor reads only the planner's report.

| Role | Pin (spawned by) | Job |
|---|---|---|
| Scout | `model: 'sonnet'`; `'haiku'` for pure file location | One scoped question; named paths, `file:line` cites, ≤30 lines back, 15 tool calls |
| Checker | `model: 'sonnet'` | Checks only the claims and citations it is given; reads only the cited lines; **default verdict is `refuted`**; 8 tool calls |
| Synthesizer / judge / reconciler / contract writer | the planner itself, never spawned | Turns findings or designs into packages; judges competing designs; writes the skeleton and the interface files |
| Reality check | the planner itself (`git ls-tree`, `ls`, one grep per symbol); a checker only for plans over 20 packages | Re-checks each package's `files_owned` and `depends_on` against the code at the current integration HEAD, immediately before scheduling |
| Codex reviewer | `agent-exec plan review`, class `independent-review` (pi `openai-codex/gpt-6.1-sol`), run by the planner | Reviews only the skeleton: splits, hidden dependencies, interface holes |
| Spec writer | `orchestra:orchestra-spec-writer`, `planning.spec_writers` by cls (light sonnet/medium, standard opus/high, deep opus/max), issued by `plan next` | One package's spec from its skeleton entry; escalates instead of working around a wrong entry |

**Audit → check → synthesize.** The planner audits each area itself, reading the code first and citing `file:line` for every claim; a scout may take a scoped reading question it would otherwise spend its own context on. A checker is spawned only for a claim a decision rests on: it tries to refute the claim against the code and the owner's rules, and a verdict the evidence does not support is `refuted`. There is no per-auditor verifier and no completeness-critic stage by default. The planner then takes the surviving findings and writes the packages. Each package must be self-contained for one engineer session: an implementer gets only its spec plus shared principles. Why: in the 2026-10-26 run, 23 children (12 auditors and 11 verifiers) used 89% of an hour's 388M tokens, the verifiers re-reading 90-95% of their auditor's files.

**Design → critique → revise.** For each feature the planner writes the design and its packages, then a checker attacks it against the code and constraints, and the planner revises: it answers every valid point (and rejects invalid ones in a "Critique responses" section). The Codex review is the default external critique of every skeleton. One scout may test the design against named code. For a decision with two defensible architectures, the planner sketches both, scores them against the code and synthesizes one; the disagreement is information about the problem, the same way §12's competing implementations are information about a spec.

**Reconcile → cross-check.** Independently written designs overlap and disagree. The planner dedupes each track into one package list (keeping ids where a package survives, recording `dropped` with `replaced_by`), then one checker looks across tracks for dangling `depends_on`, missing cross-track dependencies (two packages changing the same type, format, or schema in different files), and `files_owned` gaps.

**Reality check, immediately before scheduling.** `files_owned` is a *prediction*. In the reference run, feature packages were planned while a 7-wave refactor was still landing; by the time they were scheduled, paths had moved and some work had already been done by the refactor. The planner re-reads each package's `files_owned` and `depends_on` against the current integration HEAD (`git ls-tree`, `ls`, one grep per symbol; a checker only for plans over 20 packages), makes the corrected edits in `plan.json`, and appends a "Post-refactor reality check" section to the spec. Skipping this step does not save its cost: every stale spec is paid for later as an ESCALATE, a conflict, or a reviewer rejection.

**The review and the redo gate.** The planner runs `agent-exec plan review --plan P` and repeats it while it reports `running` (each call waits at most 540 s). That is a command the planner runs, not advice: agent-exec builds the prompt, dispatches the skeleton to class `independent-review` in its own isolated tree, parses `{"findings":[{severity: structural|minor, kind: split|dependency|interface|ownership|other, packages, evidence, detail, fix}]}`, writes `<plan stem>.review.json` and applies the redo gate. The gate is configured by `planning.redo_when` (0 disables one): `structural_findings` ≥ 1, `deep_packages` ≥ 3, or an interface listed by `interface_users` ≥ 5 packages. It trips only in round 1 and only when `planning.redo` (default opus/max) is stronger than the round's tier; then `plan next` issues round 2 of the skeleton at `planning.redo`, which is reviewed again and never redone. Review turned off (`planning.review: false`), unavailable, failed or running over 45 min is recorded as `skipped`, and the gate still applies. On `accept`, the planner answers the findings in the skeleton, then runs `agent-exec plan review --resolved`.

**Spec writers.** `plan next` issues one `orchestra:orchestra-spec-writer` per package, at the model and effort `planning.spec_writers` gives its cls, with at most `planning.writers_concurrent` (8) alive at once. Each writer writes its spec (≤ 150 lines, every listed interface file pasted verbatim) and checks it with `agent-exec plan lint --plan P --package ID`. A writer that cannot specify its package writes `<spec stem>.escalate.md`; `plan next` then sends the skeleton back to the planner, at most 2 revisions. A spec is re-issued when its package entry or its interface files change (entry hash); two failed writes of one entry make it stuck. Writers' tool calls are capped by `enforcement.fanout.writer_calls` (40); Write and Edit calls are free.

The skeleton entry the planner writes in `plan.json` (stage 1) and the scheduler consumes; the spec writer fills in the body at `spec` (§6):

```javascript
// one package in plan.json; the spec body is written later, at `spec`
const PACKAGE = {
  type: 'object',
  required: ['id', 'spec', 'cls', 'depends_on', 'files_owned', 'est_seconds', 'verification', 'brief', 'risks', 'interfaces'],
  properties: {
    id: { type: 'string' },
    spec: { type: 'string' },                    // path of the spec file the writer produces
    cls: { type: 'string', enum: ['light', 'standard', 'deep'] },
    depends_on: { type: 'array', items: { type: 'string' } },    // only where one package needs another's CODE
    files_owned: { type: 'array', items: { type: 'string' } },   // tight: over-broad globs serialize the schedule
    est_seconds: { type: 'number', minimum: 1, maximum: 300 }, // larger: split, never allowLarge
    verification: { type: 'string' },            // the exact commands: the touched packages' tests only
    brief: { type: 'string' },                   // what to read, with file:line ranges
    risks: { type: 'array', items: { type: 'string' } },         // likely mistakes
    interfaces: { type: 'array', items: { type: 'string' } },    // ids from the top-level interfaces list
    prio: { type: 'number' },                    // optional; most urgent first
  },
}
// top level of plan.json: interfaces: [{ id, path }], path relative to the plan file
```

## 3. The dependency scheduler with rolling integration

**Rolling base.** Create one integration worktree for the programme before the first run, and mint every package's dispatch token with `--workdir` pointing at it:

```text
agent-exec isolate request start --into prog-<label>   # once; the request's integration tree, cut from its assembly (see --uncommitted)
agent-exec dispatch prepare --isolate always --task <id> --workdir <integration path> --prompt-file <spec> --run-id <run>
```

A package's worktree is created when its dispatch runs, from the integration worktree's HEAD *at that moment* — so every package starts on top of everything integrated so far, and `agent-exec isolate integrate --tasks <id> --into prog-<label>` replays it back onto that same tree. The user's branch is never touched; at the end, merge the integration tree into the request's assembly (`agent-exec isolate merge --task prog-<label> -m "<subject>"`), and offer the assembly to land (F10). Run (and resume) one programme in one session: the integration branch is `orchestra/<session8>/prog-<label>`, so a new session cannot `--into` it by id.

**Package sizing.** Target 1-3 owned files and a few minutes of worker time per package; prefer more, smaller packages over a few large ones. A large package routinely costs tens of minutes on its own, while the fixed cost of an extra package is seconds: the warm reader + fork path (herdr host, plan `warm_read`) lets every package on a model resume from one session that already read the shared files, independent packages run concurrently, and a dependent starts as soon as its dependencies integrate. `agent-exec wave lint` warns about a package owning more than 3 files or a spec longer than 150 lines.

**When may a package start?** All of its `depends_on` are integrated, none of them failed (a failed dependency marks the package `blocked`, transitively), and — by default — no package whose `files_owned` overlaps it is in flight. That last rule is a deliberate exception to §12's "never serialize for file overlap": that rule is about function-sized tasks against a frozen contract. At programme scale the packages are session-sized, two of them owning the same files almost always means one should build on the other's actual code, and resolving a session-sized conflict costs a Sonnet session every time. Set `serializeOverlap: false` to get §12's behaviour back.

**Integration is one serial queue with an integrator.** Approved packages queue up; one integrator at a time takes up to `batch` of them, runs `agent-exec isolate integrate`, resolves any `conflicted` files by reading both packages' specs, runs the targeted verification, and either commits an integration fix or takes one package back out with `git revert` of its `orchestra integrate <id>` commit — reporting per package, so one broken package never un-merges its batchmates. The integrator is the supervising layer (it may commit in the integration worktree; workers never do — staging only the files it resolved or fixed) and is pinned to Sonnet. `isolate integrate` takes an exclusive per-target lock (`--lock wait` by default, up to `--lock-timeout 900`; `--lock fail` does not wait; exit 6 = `locked`), so several workflows that share one integration target — parallel programmes, or a scheduler plus a manual integrate — serialize on it safely without a lock of their own.

**Maintenance every N integrations.** A full verification of the integration tree every `verifyEvery` packages (a failure that one minimal fix cannot clear **pauses scheduling** and returns to you, rather than letting more packages land on a red tree), and a carry-over triage every `triageEvery` packages (§4).

**Never message a workflow's agents.** Neither the instructor nor anything else may `SendMessage` an agent that a running Workflow spawned. In the reference run, a message sent to a package engineer mid-run resumed a *copy* of it, and the copy and the original committed to the same worktree two seconds apart. If a package needs correcting, let its gate reject it, or fix the integration worktree through the integrator. The `worktree_lease` guard (see `references/config.md`) denies the second writer if it happens anyway.

### Template

Paste the helpers from `workflows/pipeline.js` (`VERDICT_SCHEMA` through `runTask`, minus the final `pipeline`/cleanup/`return`) above this block. Copy them from that file rather than re-typing them: the protocol is fixed, and a copy is what keeps the model and effort pins identical to the shipped pipeline. Each package in `args.packages` is a `runTask` task (its `dispatchToken` minted as above) plus `deps`, `files_owned`, `probeDir`, and optional `prio`. Give every package a `probeDir` — a per-package directory under your scratch dir, outside every worktree — rather than `noProbes`: packages are reviewed by the same Sonnet gates, and their probes overlap the package's long implementation for free. The size gate (`sizeGate()` in `workflows/pipeline.js`) is not called here — packages are session-sized by design and run continuously instead — but the ownership pre-check is: a package that changes a path outside its `files_owned` (trailing `/**` globs count as directory prefixes) fails its gate without a review, which is one more reason the reality check above must correct `files_owned` before scheduling.

```javascript
export const meta = {
  name: 'orchestra-programme',
  description: 'Schedule packages by dependency and file overlap, run each through the two-gate runTask pipeline in its own worktree on a rolling integration base, integrate serially with a Sonnet integrator, verify and triage carry-over periodically, and run until done or paused',
  phases: [
    { title: 'Build', detail: 'runTask per package (routed implementer, Sonnet gates)' },
    { title: 'Integrate', detail: 'serial queue onto the integration worktree' },
    { title: 'Maintain', detail: 'full verification and carry-over triage' },
  ],
}

// ... pipeline.js helpers (VERDICT_SCHEMA through runTask) pasted here ...

const P = args
const INTO = P.into                       // e.g. 'prog-features'
const INT_PATH = P.integrationPath        // path of that worktree
const MAX_IN_FLIGHT = P.maxInFlight || Infinity   // every startable package; never lowered for machine load
const MAX_PACKAGES = P.maxPackages || Infinity    // no chunk stop; only a limit the user asked for
const BATCH = P.batch || 3
const VERIFY_EVERY = P.verifyEvery || 6
const TRIAGE_EVERY = P.triageEvery || 6
const SERIALIZE_OVERLAP = P.serializeOverlap !== false

const PKGS = P.packages
const byId = new Map(PKGS.map(p => [p.id, p]))
const state = {}                          // pending | running | approved | merged | failed | blocked | stranded
PKGS.forEach(p => { state[p.id] = P.state?.[p.id] || 'pending' })
const results = {}
let started = 0
let paused = null                         // set when maintenance fails: stop starting packages

// files_owned entries are paths or trailing-glob prefixes ('src/audio/**').
const stem = f => f.replace(/\/?\*\*?.*$/, '')
const touches = (a, b) => { const x = stem(a), y = stem(b); return x === y || x.startsWith(y + '/') || y.startsWith(x + '/') }
const overlaps = (p, q) => p.files_owned.some(a => q.files_owned.some(b => touches(a, b)))

const INTEG_SCHEMA = {
  type: 'object', required: ['packages', 'head', 'checks'],
  properties: {
    packages: { type: 'array', items: { type: 'object', required: ['id', 'merged', 'reason'],
      properties: { id: { type: 'string' }, merged: { type: 'boolean' }, reason: { type: 'string' } } } },
    head: { type: 'string' }, checks: { type: 'string' },
  },
}
const MAINT_SCHEMA = { type: 'object', required: ['ok', 'summary'],
  properties: { ok: { type: 'boolean' }, summary: { type: 'string' } } }

function integratorPrompt(batch) {
  const ids = batch.map(b => b.id)
  return treeLine(INT_PATH) +
    'You integrate approved packages into this integration worktree (branch orchestra/*; you may commit here). ' +
    'Specs: ' + ids.map(id => byId.get(id).workerPromptFile).join(', ') + '.\n' +
    '1. Run `agent-exec isolate integrate --tasks ' + ids.join(',') + ' --into ' + INTO + ' --json` (exit 1 = some task conflicted; that is normal). ' +
    'It holds a per-target lock and waits for another integrate into the same target (`--lock wait`, default timeout 900s); exit 6 = still locked: run it again once, never hand-roll a lock.\n' +
    '2. For each conflicted task: open each listed file, resolve the markers keeping BOTH packages\' intent (read both specs), then `git add -- <the files you resolved> && git commit -qm "orchestra resolve <id>"`.\n' +
    '3. Run the verification of every package in this batch: ' + batch.map(b => b.id + ': ' + byId.get(b.id).verification).join(' | ') + '.\n' +
    '4. A failure caused by the combination: fix it minimally (integration fixes only, no features), stage only the files you changed (`git add -- <paths>`, never `git add -A`) and commit "orchestra fix <id>". ' +
    'A package that is itself broken beyond a few edits: take it out with `git revert --no-edit <sha of its "orchestra integrate <id>" commit>` (newest first), keep the others.\n' +
    'Never use git stash. Never touch any other worktree. Report per package: id, merged, reason (empty when merged); head = full sha of HEAD; checks = one line.'
}

let sinceVerify = 0, sinceTriage = 0
const queue = []
let draining = null

async function maintain() {
  if (sinceVerify >= VERIFY_EVERY && P.fullVerify) {
    sinceVerify = 0
    const r = await agent(treeLine(INT_PATH) +
      'Run the full verification: ' + P.fullVerify + '. On failure, fix it minimally (integration fixes only; a flaky test gets its root cause fixed, not a retry) and stage only the files you changed (`git add -- <paths>`, never `git add -A`) and commit "orchestra fix full-verify", then re-run once. Never use git stash. ok = green at the end.',
      { label: 'verify@' + Object.values(state).filter(s => s === 'merged').length, phase: 'Maintain', model: 'sonnet', schema: MAINT_SCHEMA })
    if (!r || !r.ok) paused = 'full verification failed: ' + (r ? r.summary : 'no result')
  }
  if (sinceTriage >= TRIAGE_EVERY && P.carryDir) {
    sinceTriage = 0
    const pending = PKGS.filter(p => state[p.id] === 'pending').map(p => p.id)
    await agent(treeLine(INT_PATH) +
      'Triage carry-over. Each file ' + P.carryDir + '/<id>.carry.md not yet marked TRIAGED lists things a package could not do because the files were not its own. ' +
      'For each item: drop it if the code at HEAD already does it or a pending spec already plans it; otherwise append it to the ONE pending spec whose files_owned covers it, under "## Carry-over (added by triage)"; ' +
      'if none owns it, append it to ' + P.carryDir + '/unowned.md. Pending packages (only these specs may be edited): ' + (pending.join(', ') || '(none)') + '. ' +
      'Then add a first line "TRIAGED" to each processed carry file. Do not edit repository files.',
      { label: 'triage', phase: 'Maintain', model: 'sonnet', schema: MAINT_SCHEMA })
  }
}

async function drain() {
  while (queue.length) {
    const batch = queue.splice(0, BATCH)
    const r = await agent(integratorPrompt(batch),
      { label: 'integrate:' + batch.map(b => b.id).join('+'), phase: 'Integrate', model: 'sonnet', schema: INTEG_SCHEMA })
    const got = new Map((r?.packages || []).map(x => [x.id, x]))
    for (const b of batch) {
      const x = got.get(b.id)
      if (x?.merged) { state[b.id] = 'merged'; sinceVerify++; sinceTriage++ }
      else { state[b.id] = 'failed'; results[b.id] = { ...results[b.id], pass: false, needsInstructor: true, summary: 'integration: ' + (x ? x.reason : 'integrator returned nothing') } }
    }
    log('integrated ' + batch.filter(b => state[b.id] === 'merged').length + '/' + batch.length + ' -> ' + (r?.head || '?').slice(0, 7))
    await maintain()
    batch.forEach(b => b.done())
  }
  draining = null
}

function enqueue(id) {
  return new Promise(done => { queue.push({ id, done }); if (!draining) draining = drain() })
}

// A RelayFailureError (skill §5) means the RELAY lost the dispatch's result,
// not that the package's work or review failed - in the field the
// implementation had almost always finished. Calling runTask() again would
// re-implement finished work in the same worktree (20+ minutes of a pi worker per
// package, observed), so never do that. Recover in order:
//   (a) resume waiting on the SAME token: `wait` is idempotent and returns
//       the persisted result, however many earlier hops were garbled;
//   (b) if that also fails, ask the package's worktree whether it holds
//       changes: one relay hop runs `agent-exec isolate diff --task <id>
//       --names-only`, whose JSON carries `path` (the worktree root - the
//       same tree the dispatch's `isolation.path` names) and `files`.
//       Non-empty -> rerun runTask with preimplementedPath = that path, so
//       it starts at the review. Empty -> hand it back as needsInstructor.
// The diff check runs only after the result record is unreachable; a
// diff result also marks the tree collected, which is harmless here since
// the review and integration that follow change or consume it anyway.
// A relay failure parks the package as `stranded`, never `failed`: only
// `failed` cascades dependents to `blocked`, and a relay hiccup must never
// block an otherwise-healthy dependency chain - its dependents stay
// `pending` and come back in the run's pending list.
async function recoverPackage(p, err) {
  log('relay failure on ' + p.id + ' (' + err.message + '); resuming wait on ' + err.token)
  if (err.token) {
    try {
      const r = await awaitDispatch(err.token, (err.label || p.id) + '-resume')
      const iso = r.isolation || {}
      const path = iso.isolate ? (iso.workdir || iso.path) : null
      if (r.status === 'ok' && path) return runTask({ ...p, preimplementedPath: path })
    } catch (e) {
      if (!(e instanceof RelayFailureError)) throw e
    }
  }
  const d = await relayHop('agent-exec isolate diff --task ' + p.id + ' --names-only', 120000, p.id + '-diff-check')
  if (d.status === 'ok' && d.path && (d.files || []).length) {
    log(p.id + ': worktree holds ' + d.files.length + ' changed files - reviewing them instead of re-dispatching')
    return runTask({ ...p, preimplementedPath: d.path })
  }
  return { id: p.id, pass: false, needsInstructor: true, relayFailure: true,
           summary: 'relay lost the dispatch result and the worktree has no changes: ' + err.message }
}

async function runPackage(p) {
  state[p.id] = 'running'
  let verdict
  try {
    verdict = await runTask(p)                // two gates, class escalation, isolation: skill §5
  } catch (e) {
    if (!(e instanceof RelayFailureError)) throw e
    try {
      verdict = await recoverPackage(p, e)
    } catch (e2) {
      // A second relay failure inside the recovered run: stop here, still
      // without blaming the package.
      if (!(e2 instanceof RelayFailureError)) throw e2
      verdict = { id: p.id, pass: false, needsInstructor: true, relayFailure: true, summary: 'relay failed twice: ' + e2.message }
    }
  }
  results[p.id] = verdict
  if (!verdict.pass) { state[p.id] = verdict.relayFailure ? 'stranded' : 'failed'; return }
  state[p.id] = 'approved'
  await enqueue(p.id)
}

phase('Build')
const inflight = new Map()
for (;;) {
  let changed = true
  while (changed) {
    changed = false
    for (const p of PKGS) {
      if (state[p.id] === 'pending' && p.deps.some(d => byId.has(d) && (state[d] === 'failed' || state[d] === 'blocked'))) {
        state[p.id] = 'blocked'; changed = true
      }
    }
  }
  if (!paused && started < MAX_PACKAGES) {
    const ready = PKGS.filter(p => state[p.id] === 'pending'
      && p.deps.every(d => !byId.has(d) || state[d] === 'merged')
      && !(SERIALIZE_OVERLAP && PKGS.some(q => (state[q.id] === 'running' || state[q.id] === 'approved') && overlaps(p, q))))
      .sort((a, b) => (b.prio || 0) - (a.prio || 0))
    for (const p of ready) {
      if (inflight.size >= MAX_IN_FLIGHT || started >= MAX_PACKAGES) break
      if (SERIALIZE_OVERLAP && [...inflight.keys()].some(id => overlaps(p, byId.get(id)))) continue
      started++
      inflight.set(p.id, runPackage(p).then(() => p.id, e => { state[p.id] = 'failed'; results[p.id] = { pass: false, summary: String(e) }; return p.id }))
    }
  }
  if (inflight.size === 0) break
  inflight.delete(await Promise.race(inflight.values()))
}
while (draining) await draining

await agent('Run `agent-exec isolate sweep --include-current --text` as ONE foreground Bash call with timeout 120000. Print its stdout verbatim - nothing else.',
  { label: 'cleanup', model: 'haiku', effort: 'low' })

const counts = {}
Object.values(state).forEach(s => { counts[s] = (counts[s] || 0) + 1 })
return {
  counts, paused,
  pending: PKGS.filter(p => state[p.id] === 'pending').map(p => p.id),   // feed back as args.state to resume
  state, results,
}
```

**A lost relay never costs the work twice.** When a dispatch's relay fails (`RelayFailureError`, skill §5), `runPackage` does not call `runTask` again from the start: it resumes waiting on the same token with the skill's `awaitDispatch()`, and if the result is unreachable it asks the package's worktree (`agent-exec isolate diff --task <id> --names-only`, one relay hop) whether the work is there. Changes present → `runTask` with `preimplementedPath` set to the `path` that diff reports, straight to review; no changes → the package comes back `needsInstructor` and is parked as `stranded`, which — unlike `failed` — does not block its dependents (they stay in `pending`). Watch long waits as they happen: `dispatchClass()` logs every wait hop (count, pid, seconds waited).

The run-end sweep keeps the integration tree until it is merged into its assembly, and the assembly until it is landed (a tree with children is never removed). It leaves any package tree whose work never integrated, so a failed package can still be inspected, and it reclaims every package tree that did integrate.

**What the worker prompt must add at programme scale** (on top of the skill's §6): touch only `files_owned`; anything needed outside them goes, one self-contained instruction per line with file paths, into `<carryDir>/<id>.carry.md`, never into the code; work only in the tree named on the WORKING TREE line; set changes aside with `agent-exec shelf push`/`pop`, never `git stash` (the stash stack is shared by every worktree; the guard denies it to Claude workers, but CLI executors run outside the hooks and only the prompt reaches them); read the current code before trusting the spec's paths, since other packages have moved code since it was written.

## 4. Carry-over instead of escalation

A package that needs a change in a file it does not own is not blocked and is not an ESCALATE: the change belongs to *another* package. The worker records it in its carry file and finishes its own scope; the triage step moves each item into the one not-yet-started spec that owns the file, or into `unowned.md` for you to turn into a small extra package. None of it passes through your context, and started packages' specs are never edited (they would not see the change). Package `rescue` is the same tool pointed the other way: a package that failed its gates keeps its worktree, and a new `runTask` with a brief that names the remaining findings continues in it.

## 5. After the programme

The integration branch holds every merged package as `orchestra integrate <id>` commits plus any resolve/fix commits. When the programme is done, finish the integration tree with `agent-exec isolate merge --task <into> -m <paragraph>`: it lands as one commit on the request's assembly and removes the integration tree. Offer the assembly to the user. When they ask to take it, land it with `agent-exec isolate land -m <paragraph>` (`isolation.md` §1a, F10): it squashes the assembly's own changes into one commit under the user's git identity on the checked-out branch, then removes the assembly, and the request ends once its last assembly is landed. Never `git merge` or fast-forward the integration or assembly branch by hand: its commits are authored `orchestra`, and the merge guard denies merging an `orchestra/*` branch outside an orchestra worktree.

## 6. Deterministic runner (`agent-exec wave run`)

`agent-exec wave run` is §3's scheduler as a plain loop with no agent in it: no Workflow script, no integrator session, no model deciding what to start next. Use it instead of the Workflow scheduler when the plan is settled (§2's packages written as `plan.json`: `packages[]` with `id`, `spec` (path to the spec file), `cls`, `depends_on`, `files_owned`, `est_seconds`, `verification`, `brief`, `risks`, `interfaces`, optional `prio`, plus top-level `interfaces` (`{id, path}`, path relative to the plan file) and optional top-level `preamble`, `warm_read` (repo-relative paths a herdr warm reader reads once and every package session on the same model forks from; absolute or `..` paths are rejected, missing files only warn; see `config.md` §8), `external_done` ids, and `rules` entries of `{"if": "<glob>", "require": ["<glob>", ...]}`), the packages are implementable by the cheap tier from their spec alone, and conflicts should come back to you instead of being resolved by an agent. Keep the Workflow scheduler when integration needs judgement (an integrator that resolves conflicts from both specs) or packages need a review gate beyond `agent-exec check`.

Run `agent-exec wave lint --plan plan.json` before scheduling to report non-blocking ownership and sizing warnings. It flags deletion specs that own only literal paths, project rules whose required globs are not owned, packages owning more than 3 files and specs longer than 150 lines (see "Package sizing" in §3); `--json` emits structured warnings for automation. It also flags a spec that does not contain one of its package's interface files verbatim. The planning stages check with `agent-exec plan lint --plan P` (`--package ID` for one spec).

`wave run` refuses to start (an error report of kind `environment`) when the user's HEAD tracks files under any `isolation.exclude_paths` directory. The message names each directory with its file count and size, and the two fixes: take it out of the branch (`git rm -r --cached <dir>`, add it to `.gitignore`, commit) or list it in `isolation.keep_paths`.

`wave run --into` names an integration tree of the calling session's request (F7), cut from the assembly's HEAD when it is first created; an assembly is never accepted as its target. When the request has no assembly yet, the run creates it from the user's checkout HEAD, and a checkout with uncommitted changes refuses before the runner detaches (an error report of kind `uncommitted`, exit 3). Decide that per request with `--uncommitted include|ignore`: `include` when the work builds on those changes (the request refers to code only they contain, or they touch the files the task changes), `ignore` when they are unrelated (scratch, other work in progress), and when unsure ask the user in one line naming the paths. Never commit, stash or discard the user's changes. A run that ends done with no needs merges its integration tree into the assembly (end report key `merge`, event `merge`; a failed merge becomes a `merge` need below).

```text
agent-exec wave run --plan plan.json --state wave/state.json --into prog-<label> \
    --run-id <run> --title '<task title>' --full "<full test cmd>"   # add --budget-usd X only if the user fixed one
agent-exec wave status --state wave/state.json --watch 30     # from another shell
```

**Starting it from Claude Code.** A run lasts far past the Bash tool's 600 s limit. Start a wave by spawning the `orchestra:orchestra-wave` agent with the whole `wave run` command line (absolute `--state`, and `--title '<task title>'`: at most 30 characters, what the user would call this work, the planner's title when one planned it, no quotes) and the description `wave <run-id>: <N> packages`. Never run `wave run` from your own Bash, foreground or `run_in_background`: a Bash-launched run shows nothing in Orca or remote sessions. The watcher starts it with `--detach` (the runner becomes a detached child, recorded in `<state dir>/runner.pid` with its output in `runner.log`; while it lives, another `--detach` for the same state starts nothing and answers `{"status":"running",...}`, exit 2). It then loops `agent-exec wave wait --state <state> --since <seq> --max-wait 560` and prints each hop's `line` (`wave 12/40 · 3 running · 1 needs · ~35m left · ~$4.10`), so its transcript is the run's live progress. It returns the last `wave wait` answer: `needs` as soon as a need appears that was not there at the previous hop (a new need wins over the runner having ended), else `done`, `stopped` or `crashed` once the runner process is gone (`crashed`: it left no `done` or `stopped` end report in `runner.log`, e.g. a plan error or a panic). On `needs`, decide them (the approvals policy, skill §8, and the needs list below), then spawn the watcher again with the same command: it resumes waiting on the live runner, or starts the next run if that one ended. On every `needs` and on `done`/`stopped`/`crashed`, send the user a PushNotification with the answer's `line`: remote viewers see nothing else. Do not poll `wave status` or read `events.jsonl` between the watcher's returns. When you end a programme while the watcher's last answer was `needs` (finished by hand, abandoned, or the remaining needs handled without spawning the watcher again), run `agent-exec status close --state <state> --status stopped` (`--status done` when every package is integrated and the full checks are green); otherwise the status file keeps listing the run as `needs` for 6 hours. The answer's `progress` object (also the `progress` key of `wave status --json`; `wave status --line` prints its `line`) has the run id, counts, the packages in flight with phase and elapsed time, the needs, the next packages to start, the last integrated ones, the run's elapsed time, an ETA (the remaining critical path over `est_seconds`, scaled by the actual/estimated ratio once two estimated packages finished; null when nothing left has an estimate) and the run's estimated cost (`--run-id` needed). `wave wait --until-change` also answers `changed` as soon as an event a viewer sees (a package status, `dispatch-start`/`-end`, an integration, a need, a network denial, a stop) was recorded after `--since`; a new need still answers `needs`, and a gone runner its end status. Every answer also carries `approvals` (the pending items `agent-exec approvals` lists, each `{id, kind: network|path|dialog, pkg, detail}`, `pkg` null for a network host) and `machine` (`load1`, `load5`, `cpus`, `mem_pressure` normal/warn/critical/unknown, `heavy` and `test` slot pools as `{used, size}`, `git_procs`; an unreadable probe is -1 or `unknown` on its own). The orchestra HUD puts the wave's counts and account in the band above the user's prompt (its 詳細 button expands the running packages, needs and next) and toasts each new need. Point the user at `wave status --watch` in their own terminal (or the account's board page, `/orchestra:board`) for package-level detail: herdr-hosted workers run in orchestra's own herdr server and show up in neither Orca nor Claude Code as sessions (their worktrees are not Orca-created, so Orca lists them only where its non-Orca worktree visibility is `show`: `orca repo set --repo path:<repo> --external-worktree-visibility hide` hides them, together with your own non-Orca worktrees). That server stops once no workspace is left open in it, whichever run started it: at the end of a run (with `keep_sessions`, only when no package session is left to resume), or when removing a package tree by any route (`isolate remove`/`sweep`, the SessionEnd hook, `/cleanup`) closed the last workspace — every such removal first closes the hosted session recorded in the tree, so no worker is left running in a deleted directory. A refresh no longer stops it mid-run: the runner closes its herdr session before refreshing a package's tree, and the herdr executor re-checks the server before every workspace create, re-attaching or restarting it when it is gone. Every hosted worker session (herdr or Orca) starts with an `<orchestra-worker>` notice in place of the router, so it does its package instead of orchestrating again. Each hosted Claude session records its own `--session-id` under the run id (`<state dir>/hosted/<run id>.txt`), so `agent-exec usage --run <id>` counts it. Hosted workers run with the advisor tool off (`CLAUDE_CODE_DISABLE_ADVISOR_TOOL=1`); Agent-tool workers and the planner's children are told never to call it, since Claude Code has no per-subagent switch.

**The status file.** The runner, not a model, publishes progress. Every `wave run` keeps its own entry `~/.claude/orchestra/status/runs/<key>.json` current (the key is the first 16 hex characters of the SHA-1 of the real state path): at start, within 5 s of any state change (its own, or a `wave mark` or `approvals decide` from another process), at least every 60 s while it runs, and once at its end (`done`; `needs` when it ended with needs open; `stopped`). Each entry carries `account`: the claude.ai account (organization name, else email) of the profile that started the runner, null when unknown. After each write it rebuilds `~/.claude/orchestra/status.json` under a lock. The planner adds its plan with `agent-exec status plan`. The path is `$HOME`-based, so every ccprofile profile writes the same file. `agent-exec status show` rebuilds and prints it; a rebuild shows a runner that died without its end entry as `crashed` (its pid is gone) or `stale` (no rewrite for 180 s). Ended runs and plans leave the file 6 hours after their last update; their files are deleted after 7 days. orchestra reads nothing back from it. The hooks module mirrors it, without a model, to each account's claude.ai board page once the user has run `/orchestra:board` (`docs/status-file.md` in the plugin describes both); approvals stay in your session. A config layer that still has the removed `dashboard` key makes `config`, `doctor`, `dispatch` and `wave run` fail with a message naming the file: ask the user to delete that key.

Scheduling is rolling (v0.49.0): every package that may start runs at once (`--max-in-flight auto`, the default; an integer caps it, see `config.md` §9), most urgent first: highest `prio`, then the longest chain of packages waiting on it (summed `est_seconds` when every package below each startable one has one, else the package count; never mixed within one pick), then plan order. While the OS reports memory pressure new starts wait, re-checked every 30 s (`start-held` / `start-resumed` events); packages in flight carry on. While a hold lasts, `state.json` carries it as `held` (`{kind, reason, since}`, present only while held), and a non-login hold that has lasted 10 minutes emits one `start-held-long` event (`{kind, reason, held_s}`). The same hold applies at the Claude plan-usage stop level (`max(enforcement.usage_guard, 95)`, §1), and while a `login` need is open (see the needs list below); under the usage stop level, `enforcement.usage_advise` (default 75) only paces starts to one per round and emits `usage-advice`. The moment one finishes it is integrated (one integration at a time, in plan order among those ready) and the freed slot goes to whatever may start now, so a dependent starts as soon as its dependencies are integrated, without waiting for unrelated packages. A wave is one scheduling round that started something. Each start picks what may start (dependencies integrated, no overlapping `files_owned` in flight), refreshes a package's leftover worktree onto the integration HEAD, dispatches each package with preamble + a generated context file (`<state dir>/context/<id>.md`: WORKING TREE, `files_owned`, carry file, shelf-not-stash, ESCALATE, do-not-commit) + spec, verifies the result, then integrates the `ready` packages with `isolate integrate --on-conflict skip [--verify <gate> --bisect]`.

Heavy local commands and the checkout and delete of every orchestra worktree are admitted machine-wide a few at a time (installs, builds and worktree I/O on `throttle.heavy_slots`, tests, lints and typechecks on `throttle.test_slots`; `config.md` §10), whatever the width, so a slow machine is not a reason to lower `--max-in-flight`: workers mostly wait on the model, not the machine, and memory pressure holds new starts on its own. When runs feel slow, check `agent-exec trace report --kind lock` and `agent-exec slot status` first.

How a package is verified depends on `wave.verify_mode` (`config.md` §9). `final` (default): only `checks.items` marked `fast: true` run on the package; a red one is recorded in the package detail (`fast check failed: <names>`, kept after integration, plus a `fast-check` event) and blocks neither integration nor dependents, and there is no default gate (`--gate` still applies when given). When nothing is left to start, the full checks (every check item without `fast`, over the files changed since the base, then `--full`) run once on the integration tree, one full run at a time per process. Red failures are mapped to packages without a model: the failing test files and every path the excerpt names, matched against each integrated package's diff file list, then its `files_owned`. Each matched package gets a `fix-<n>` package (spec `<state dir>/fixups/fix-<n>.md` with the excerpt, the original spec path and the diff file list; `cls` one level up; `files_owned` = the original's plus its diff files); failures nothing matches go to one catch-all fix-up (`deep`, owns `**`). Fix-ups run through the same scheduler (in parallel when their files do not overlap), get integrated, and the full checks run again, up to `wave.converge_rounds` (default 2) rounds; still red, the run stops with a wave-level `post-integration` need holding the remaining failures. Fix-ups are persisted in `<state dir>/fixups.json`, so a resumed run keeps them and their round count. `gated`: the behaviour below — `agent-exec check --task pkg-<id> --baseline` with one correction round (only the correction file when the executor has a resumable session; above `enforcement.resume_max_tokens` the live session is closed and a fresh one gets the full prompt plus the correction, `resume-retired` event; a pi session reports `resumable: false` and is not auto-resumed), the default gate, and `--full` pausing the run on red.

The first implementation dispatch after a refreshed or newly created tree is
always a fresh session; later corrections in that tree use the normal resume
rule. If a refresh carried a patch but the fresh implementation leaves no
diff, the package is recorded as `empty-after-refresh` with the worker's answer
and recovery patch path. ESCALATE details include the package carry file, and
paths outside `files_owned` are reported as `scope`; `wave mark --widen` stores
additional ownership globs for the next dispatch. `wave mark --recheck` batches
existing trees for verification without dispatching them. Checks that pass
only after an isolated failed-test rerun are emitted as `flaky`; recurring
files appear in the end report after the configured threshold.

Flags: `--gate` replaces the default integration gate (`agent-exec check --path . --since <pre-wave HEAD> --baseline`; preexisting failures count as green). In `gated` mode `--full` runs in the integration tree after every `--full-every`-th integration that landed something, and once at the end (no timeout unless `--full-timeout`); red goes through the steps below. `--on-green <cmd>` runs only after a green `--full` in the fixed `<state>/green-tree` worktree, with `{sha}` and `WAVE_GREEN_SHA` set to the integrated SHA; it never stops the loop. Never point it at production. `--resume-on-reset` waits through executor cooldowns in 30-second chunks and retries; `agent-exec wave stop --state ...` drops a `STOP` file; `--budget-usd X` (needs `--run-id`; config `wave.budget_usd`) stops once the run's estimated cost reaches X (reason `budget reached: ~$N of $X`); `--stop-at <epoch>`, `--max-waves` and `--max-packages` also bound the run, but a programme does not use them to chunk itself. Every stop lets the packages in flight finish and integrates what is ready; nothing new starts. An executor pool with no candidate left normally stops the run, with the package back at `pending`. `--notify-cmd` runs on each new need, on each `usage` notice, on stop and on done, with `WAVE_EVENT=need|usage|stopped|done`, `WAVE_STATE`, and the event JSON on stdin (bounded at 30 s each, never waited on by the loop): the hook for agmsg or anything else.

**Red full verification (`gated`).** Every green `--full` records the integration HEAD in `<state dir>/green.json` (`{"sha", "at"}`; the integration base when nothing is recorded yet). Before every full command and bisect probe, including the last-green baseline probe, configured `checks.ensure` entries run against the integration tree. When `--full` is red and `--full-retry "<cmd with {failed}>"` is given, the failing test files parsed from its output are re-run alone; a pass counts as green and is emitted as `flaky` (pkg null). Still red, the runner bisects the `orchestra integrate <task>` commits since the last green SHA with `--full` as the probe (same bisect-probe / revert events as the gate, up to 3 rounds), reverts each culprit, writes `corrections/<id>.md` (failing tests + excerpt) and sets the package `pending` (`post-full revert`); the next wave refreshes its tree and dispatches it in a fresh session with that correction appended to the full prompt, once. A package reverted this way a second time becomes a `post-integration` need instead. If full verification is red at the last green SHA because an ensure prerequisite is failing, the runner records a wave-level `environment` need, emits an `environment` event, and stops without reverting or changing package statuses. Otherwise red at the last green SHA stops the run (`full verification red at the last green SHA`) without reverting; red after the bisect budget stops it (`full verification red`). The integration tree always ends attached and clean.

Every command run in the integration tree is followed by a hard reset and `git clean -fdq`; if this restores tracked or untracked files, the runner first saves them (staged, unstaged and untracked, minus `isolation.exclude_paths`) as `<state dir>/rescue/<timestamp>.patch` and records an `integration-dirty` event whose detail names the `patch` (or a `patch_error`), so a reset is never the only copy of a package's work. Before dispatching a package it also restores a dirty integration tree, preventing generated files from entering a package baseline.

**Integration commits run with hooks off.** Every commit the runner makes in the integration tree (`orchestra integrate <task>`, the bisects' `orchestra revert <task>`, tree baselines) runs with `--no-verify` and `LEFTHOOK=0`/`HUSKY=0`, bounded to 120 s: a repository's pre-commit lint or typecheck never decides what lands — verification is the gate's and `--full`'s job. A commit that still fails is rolled back like a skipped conflict (`isolate integrate` reports the task `commit-failed`, `isolation.md` §2), so the next package integrates from a clean HEAD; the failed package is never `integrated` and becomes an `integrate` need. `empty` packages (changed nothing) are `integrated` as before.

**The needs list** is how the runner hands work back without stopping: `escalate` (the worker's first line was `ESCALATE`), `delegate` (the route resolved to a tier only you can spawn; the detail is JSON with the token, agent type, model, effort and tree), `self-verify` (checks still red after the correction round), `conflict` (refresh or integration conflicted; the package was rolled back), `post-integration` (the gate bisected to this package and reverted it), `dispatch-error`, `integrate` (the package applied but no `orchestra integrate` commit landed for it; the detail is JSON with `excerpt`, the first 20 lines of git's output, and `tree`, the package's tree, which is kept uncollected with its session left open — fix the cause, then `wave mark` it `ready`), `oversized` (integrate refused the package: it adds more new files than `isolation.max_added_files`, usually a dependency/cache dir its tree created; remove that dir from the package's tree, or add it to `isolation.exclude_paths`, or raise/disable (`0`) `max_added_files`, then re-dispatch or `wave mark` it `ready`), `merge` (the run's end-of-run merge of its integration tree into the request's assembly failed; the detail is the merge result's text. On a conflict, merge the assembly's branch into the integration tree, resolve it and commit there, then run `agent-exec isolate merge --task <into> -m "<subject>"` yourself), `permission` (a sandbox path denial), `network` (the worker's first line was `ESCALATE: network <host>[, ...]`; the hosts join the package's `network_denials`; only the list right after `network` counts — the first token that is not a host name with an alphabetic TLD ends it, so prose after it adds nothing), `login` (a hosted Claude worker answered with Claude Code's /login reply: this profile's Claude login expired or is missing. The runner closes that worker's session, keeping its tree, and holds every new start while a `login` need is open (`start-held`, re-checked every 30 s). Ask the user to run /login in a Claude Code session of this profile, then hand each `login` package back with `agent-exec wave mark --state ... --pkg <id> --status pending`; starts resume once no `login` need is left). A hold is not a need. `wave wait` answers `held` (after `needs` and the final statuses, before `changed`) when a `start-held-long` event was recorded after `--since` and the state still holds; `progress.held` (`{kind, reason, since, held_s}`) and the `held: <kind>` part after `N needs` in the line show it while it lasts. Dependents of a package in needs are reported `blocked`. Permission and network needs, permission dialogs (`stalled` with a `dialog:` detail) and every blocked host are **approvals you decide yourself** with the skill's §8 policy: `agent-exec approvals --state <state>` lists them with stable ids (`net:<host>`, `path:<pkg>:<n>`, `dialog:<pkg>`, each with the package's spec), and `agent-exec approvals decide --state <state> <id> allow|deny --reason TEXT [--by instructor|user]` applies one decision — a domain or path grant, Enter/Escape to a herdr dialog (Orca dialogs are still approved in Orca, then `wave mark --await`), or a note telling the worker to finish without it — and puts the package back to `pending` (a dialog to `await-orca`), where the next run resumes it with the decision notes appended. Every decision is audited. `wave run`'s end report (`approvals`) and `wave status` show how many are pending; the runner never decides. After dealing with one (running a `delegate` package yourself in its tree, resolving a conflict), hand it back with `agent-exec wave mark --state ... --pkg <id> --status ready|pending|failed`: the next run integrates a `ready` package without another dispatch and re-dispatches a `pending` one.

**Resume** is rerunning the same command. State lives in `state.json` (with `events.jsonl`, `context/`, `corrections/` and `carry/` next to it), and the `--into` integration worktree is reused (after a run merged it into the assembly, the next run recreates it from the assembly's HEAD). A package a crashed run left `implementing`/`fixing` goes to `verifying` if its tree holds changes and back to `pending` if not; `verifying` is re-checked and `ready` is integrated in the first wave. `wave run` removes a leftover `STOP` file from an earlier `wave stop` when it starts, so rerunning a stopped run needs no manual step. The end report is `{"status", "reason", "waves", "approvals", "state", "integrated", "needs", "network_denials", "pending", "blocked", "flaky_tests"}`; exit 0 done, 1 done with needs, 5 stopped.

**Orca sessions.** When Orca is available (config `orca`, see `config.md` §5), a package the CLI dispatch returns as `delegate` (a Claude tier) runs in an interactive Claude Code session that Orca hosts instead of landing on the needs list — inside the subscription, never `claude -p`. With a host active the runner prepares every dispatch with `--isolate cli` (without one, `always`), so a delegate route gets no orchestra tree; one an earlier attempt left (a CLI route that came back `unavailable`) is dropped. The runner then creates an Orca worktree `wave-<run8>-<id>` on the integration branch (holding one heavy slot for the create; created with `--no-parent`, so Orca sets no lineage, while orchestra records it as a child of the integration tree), adopts it as task `pkg-<id>` (`isolate adopt`, so check/refresh/integrate reach it by task id), opens `claude` there and waits for its prompt box, then sends ONE line naming the same prompt files as the CLI path (the context file's WORKING TREE names the Orca worktree) plus a result file `<state dir>/orca/<id>.result.json`. That file is the only completion signal; `tui-idle` alone never is. The session stays alive: a self-verify correction (only the correction file) and a later re-dispatch go to the SAME terminal (unless its context has reached `enforcement.resume_max_tokens` (Haiku 90k, others 180k; `resume_max_tokens_by_model`): then the terminal is closed and a fresh one gets the full prompt plus the correction, `resume-retired`), a refresh before a dispatch moves the adopted tree in place, and after `integrated` the terminal is closed and the worktree removed. Sessions are recorded in `<state dir>/orca-sessions.json`, so a resumed run reuses a live terminal (a gone terminal is reopened on the same worktree). A permission dialog becomes a `stalled` need with the dialog, terminal, worktree, result path, and exact `wave mark --state <state> --await <id>` recovery command; approve it, mark the package, and the next `wave run` waits for the existing result without resending the prompt. Anything Orca cannot finish becomes a `delegate` need whose JSON detail carries `orca` (`trust`: Claude's folder-trust dialog needs accepting once, or set `auto_trust`; `stalled`: no prompt box within `startup_timeout`, or a permission/choice dialog is waiting; `timeout`: no result within `task_timeout`; `error`), `detail` and the dispatch `token`, so you can still take the package over. Events: `dispatch-start`/`dispatch-end` with `executor: "orca"`, plus `orca-session` (`action`: start, reuse, close, trust).
