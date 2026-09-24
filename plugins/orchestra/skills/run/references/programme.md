# Programme-scale runs: planning, dependency scheduling, rolling integration

Read this when a request decomposes into **dozens of session-sized packages** with ordering between them — an audit-then-refactor, a multi-feature build-out, a migration — rather than a handful of independent tasks. §5's `pipeline(tasks, runTask)` assumes the tasks are independent and integrated once at the end; at programme scale that assumption breaks in three ways: packages depend on each other's *code* (not just a contract), HEAD moves under every package while it runs, and the plan itself has to be produced by agents before any of it can be scheduled.

Everything below comes from one real ultracode session that ran this shape end to end: 13 workflow runs, 499 agents, 115.9M tokens, 32.5 hours, ~70 of ~220 packages integrated. The shapes worked. The cost did not: every agent inherited the session's Opus model, and the run exhausted the user's weekly quota with six days left on it. Treat §1 as mandatory, not advice.

## 1. Budget before anything else

- **Pin every agent.** That run pinned a model on exactly one of 13 scripts. Implementation goes through `dispatchClass()`/`runTask()` (§5 of the skill), review stays on Sonnet, planning roles are pinned per §2's table, relays are Haiku. §4 of the skill applies to every line of every script here.
- **Cap the chunk, not just the concurrency.** Pass `maxPackages` (default 12) and `maxInFlight` (default 4). The scheduler stops *starting* packages once `maxPackages` have started, lets in-flight ones finish, and returns `{ pending, state, head }` so the next chunk resumes exactly there. The reference run used 10 in flight and integrated ~12 packages/hour — at a burn rate nobody chose on purpose.
- **Measure between chunks.** `agent-exec usage --run <workflow-run-id>` after every chunk; show the user the number before launching the next. A programme is a sequence of chunks the user can stop, never one run that ends when the quota does.
- **Two gates, not three rounds.** The reference run allowed up to three review rounds per package on Opus. `runTask()`'s two-gate discipline (§11) is the cheaper equivalent and already escalates class on its own.

## 2. Planning workflows (read-only)

The plan is too large for the instructor to write by hand and too important to take from one agent. All planning roles are read-only against the repository and write their output to a specs directory on disk; the instructor reads only the returned summaries and package lists (ids, `files_owned`, `depends_on`, `cls`).

| Role | Pin | Job |
|---|---|---|
| Scoped auditor / designer | `model: 'sonnet'` | One per area or feature; reads code, cites `file:line` for every claim |
| Adversarial verifier / critic | `model: 'sonnet'` | Tries to **refute** each finding or design point against the code and the owner's rules; **default verdict when the evidence does not hold up is `refuted`** |
| Completeness critic | `model: 'sonnet'` | Given only the *titles* of kept findings, finds what nobody covered: cross-module drift, lifecycle gaps, flows that break at the seams |
| Synthesizer / judge / reconciler | `model: 'opus'` | Turns verified findings or designs into packages; judges competing designs |
| Reality checker | `model: 'sonnet'` | Right before scheduling, re-checks each spec against the code at the current integration HEAD |

**Audit → verify → critic → synthesize.** Pipeline each auditor straight into its own verifier (no barrier: auditor B is still reading while auditor A's findings are being refuted). Merge verdicts into the findings (a verifier's `corrected_proposal` and severity replace the auditor's), drop `refuted`, then run the critic on the titles of what survived and verify *its* findings the same way. The synthesizer gets every surviving finding plus each auditor's short area map and returns packages. Each package must be self-contained for one engineer session: an implementer gets only its spec plus shared principles.

**Design → critique → revise.** For each feature, a designer writes the design and its packages, a critic attacks it against the code and constraints, a reviser answers every valid point (and rejects invalid ones in a "Critique responses" section). For a decision with two defensible architectures, run two designers with opposing briefs in `parallel()` and an Opus judge that scores both and synthesizes one — the disagreement is information about the problem, the same way §12's competing implementations are information about a spec.

**Reconcile → cross-check.** Independently written designs overlap and disagree. One reconciler per track dedupes into one package list (keeping ids where a package survives, recording `dropped` with `replaced_by`), then one cross-checker looks across tracks for dangling `depends_on`, missing cross-track dependencies (two packages changing the same type, format, or schema in different files), and `files_owned` gaps.

**Reality check, immediately before scheduling.** `files_owned` is a *prediction*. In the reference run, feature packages were planned while a 7-wave refactor was still landing; by the time they were scheduled, paths had moved and some work had already been done by the refactor. One checker per group of packages re-reads each spec against the current integration HEAD, returns corrected `files_owned` and `depends_on` edits, and appends a "Post-refactor reality check" section to the spec. Skipping this step does not save its cost: every stale spec is paid for later as an ESCALATE, a conflict, or a reviewer rejection.

The package schema the synthesizer and reconcilers return, and the scheduler consumes:

```javascript
const PACKAGE = {
  type: 'object',
  required: ['id', 'title', 'instructions', 'files_owned', 'depends_on', 'cls', 'verification'],
  properties: {
    id: { type: 'string' }, title: { type: 'string' },
    instructions: { type: 'string' },            // self-contained; cites evidence locations
    files_owned: { type: 'array', items: { type: 'string' } },   // tight: over-broad globs serialize the schedule
    depends_on: { type: 'array', items: { type: 'string' } },    // only where one package needs another's CODE
    cls: { type: 'string', enum: ['light', 'standard', 'deep'] },
    verification: { type: 'string' },            // the exact commands
  },
}
```

## 3. The dependency scheduler with rolling integration

**Rolling base.** Create one integration worktree for the programme before the first chunk, and mint every package's dispatch token with `--workdir` pointing at it:

```text
agent-exec isolate create --task prog-<label> --backend git          # once; carries the user's uncommitted state in as its baseline
agent-exec dispatch prepare --isolate always --task <id> --workdir <integration path> --prompt-file <spec> --run-id <run>
```

A package's worktree is created when its dispatch runs, from the integration worktree's HEAD *at that moment* — so every package starts on top of everything integrated so far, and `agent-exec isolate integrate --tasks <id> --into prog-<label>` replays it back onto that same tree. The user's branch is never touched; at the end, the integration branch is what you offer to land. Run the chunks of one programme in one session: the integration branch is `orchestra/<session8>/prog-<label>`, so a new session cannot `--into` it by id.

**When may a package start?** All of its `depends_on` are integrated, none of them failed (a failed dependency marks the package `blocked`, transitively), and — by default — no package whose `files_owned` overlaps it is in flight. That last rule is a deliberate exception to §12's "never serialize for file overlap": that rule is about function-sized tasks against a frozen contract. At programme scale the packages are session-sized, two of them owning the same files almost always means one should build on the other's actual code, and resolving a session-sized conflict costs a Sonnet session every time. Set `serializeOverlap: false` to get §12's behaviour back.

**Integration is one serial queue with an integrator.** Approved packages queue up; one integrator at a time takes up to `batch` of them, runs `agent-exec isolate integrate`, resolves any `conflicted` files by reading both packages' specs, runs the targeted verification, and either commits an integration fix or takes one package back out with `git revert` of its `orchestra integrate <id>` commit — reporting per package, so one broken package never un-merges its batchmates. The integrator is the supervising layer (it may commit in the integration worktree; workers never do) and is pinned to Sonnet.

**Maintenance every N integrations.** A full verification of the integration tree every `verifyEvery` packages (a failure that one minimal fix cannot clear **pauses scheduling** and returns to you, rather than letting more packages land on a red tree), and a carry-over triage every `triageEvery` packages (§4).

**Never message a workflow's agents.** Neither the instructor nor anything else may `SendMessage` an agent that a running Workflow spawned. In the reference run, a message sent to a package engineer mid-run resumed a *copy* of it, and the copy and the original committed to the same worktree two seconds apart. If a package needs correcting, let its gate reject it, or fix the integration worktree through the integrator. The `worktree_lease` guard (see `references/config.md`) denies the second writer if it happens anyway.

### Template

Paste the skill's §5 helpers (`VERDICT_SCHEMA` through `runTask`, minus the final `pipeline`/cleanup/`return`) above this block. Each package in `args.packages` is a `runTask` task (its `dispatchToken` minted as above) plus `deps`, `files_owned`, and optional `prio`.

```javascript
export const meta = {
  name: 'orchestra-programme',
  description: 'Schedule packages by dependency and file overlap, run each through the two-gate runTask pipeline in its own worktree on a rolling integration base, integrate serially with a Sonnet integrator, verify and triage carry-over periodically, and stop at the chunk budget',
  phases: [
    { title: 'Build', detail: 'runTask per package (routed implementer, Sonnet gates)' },
    { title: 'Integrate', detail: 'serial queue onto the integration worktree' },
    { title: 'Maintain', detail: 'full verification and carry-over triage' },
  ],
}

// ... §5 helpers pasted here ...

const P = args
const INTO = P.into                       // e.g. 'prog-features'
const INT_PATH = P.integrationPath        // path of that worktree
const MAX_IN_FLIGHT = P.maxInFlight || 4
const MAX_PACKAGES = P.maxPackages || 12
const BATCH = P.batch || 3
const VERIFY_EVERY = P.verifyEvery || 6
const TRIAGE_EVERY = P.triageEvery || 6
const SERIALIZE_OVERLAP = P.serializeOverlap !== false

const PKGS = P.packages
const byId = new Map(PKGS.map(p => [p.id, p]))
const state = {}                          // pending | running | approved | merged | failed | blocked
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
    '1. Run `agent-exec isolate integrate --tasks ' + ids.join(',') + ' --into ' + INTO + ' --json` (exit 1 = some task conflicted; that is normal).\n' +
    '2. For each conflicted task: open each listed file, resolve the markers keeping BOTH packages\' intent (read both specs), then `git add -A && git commit -qm "orchestra resolve <id>"`.\n' +
    '3. Run the verification of every package in this batch: ' + batch.map(b => b.id + ': ' + byId.get(b.id).verification).join(' | ') + '.\n' +
    '4. A failure caused by the combination: fix it minimally (integration fixes only, no features) and commit "orchestra fix <id>". ' +
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
      'Run the full verification: ' + P.fullVerify + '. On failure, fix it minimally (integration fixes only; a flaky test gets its root cause fixed, not a retry) and commit "orchestra fix full-verify", then re-run once. Never use git stash. ok = green at the end.',
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

async function runPackage(p) {
  state[p.id] = 'running'
  const verdict = await runTask(p)            // two gates, class escalation, isolation: skill §5
  results[p.id] = verdict
  if (!verdict.pass) { state[p.id] = 'failed'; return }
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
  pending: PKGS.filter(p => state[p.id] === 'pending').map(p => p.id),   // feed back as args.state for the next chunk
  state, results,
}
```

The run-end sweep leaves the integration worktree alone (nothing ever collects it, so its work counts as uncollected) and leaves any package tree whose work never integrated, so a failed package can still be inspected. It reclaims every package tree that did integrate.

**What the worker prompt must add at programme scale** (on top of the skill's §6): touch only `files_owned`; anything needed outside them goes, one self-contained instruction per line with file paths, into `<carryDir>/<id>.carry.md`, never into the code; work only in the tree named on the WORKING TREE line; set changes aside with `agent-exec shelf push`/`pop`, never `git stash` (the stash stack is shared by every worktree; the guard denies it to Claude workers, but CLI executors run outside the hooks and only the prompt reaches them); read the current code before trusting the spec's paths, since other packages have moved code since it was written.

## 4. Carry-over instead of escalation

A package that needs a change in a file it does not own is not blocked and is not an ESCALATE: the change belongs to *another* package. The worker records it in its carry file and finishes its own scope; the triage step moves each item into the one not-yet-started spec that owns the file, or into `unowned.md` for you to turn into a small extra package. None of it passes through your context, and started packages' specs are never edited (they would not see the change). Package `rescue` is the same tool pointed the other way: a package that failed its gates keeps its worktree, and a new `runTask` with a brief that names the remaining findings continues in it.

## 5. After the programme

The integration branch holds every merged package as `orchestra integrate <id>` commits plus any resolve/fix commits. Offer it to the user as it is, or build the PR branch from its accepted diff (skill §12); never fast-forward the user's branch without an explicit request. Remove the integration worktree only after the user has taken the branch: `agent-exec isolate remove --task <into> --force`.
