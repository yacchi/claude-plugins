export const meta = {
  name: 'pipeline',
  description: 'Light-class implementation per task (executor resolved by agent-exec route/dispatch - Claude Haiku first, pi as fallback), an adversarial review pass checks the result, a rejection gets one correction round with a cited, family-swept packet and an incremental re-gate, and only structured verdicts return.',
  whenToUse: 'Run a batch of orchestra tasks (args: { runId, tasks }) through the cost-tiered implement-verify-retry pipeline and get back structured verdicts.',
  phases: [
    { title: 'Size gate', detail: 'refuse oversized or unowned tasks before any dispatch' },
    { title: 'Implement and verify', detail: 'routed light-class worker, probe author, ownership pre-check, adversarial review, one correction round and re-gate' },
    { title: 'Cleanup', detail: 'sweep the run worktrees and report usage' },
  ],
}

// --- Verdict schema: forces the review reply into structured JSON ---
// `cited_contract` is required on every finding: a reviewer that cannot point
// at the contract text has an opinion, not a defect (§11.1). `family` drives the
// retry policy (§11.2). Non-contractual improvements go to `optional_hardening`,
// which NEVER makes `pass` false. `tests_kept` / `tests_loc_added` /
// `impl_loc_changed` make test inflation visible at the verdict instead of
// after it lands: the reviewer's probes are throwaway, and every test it keeps
// must name the implementation mistake it detects (§6 rule 8).
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
    tests_kept: {
      type: 'array',
      items: {
        type: 'object',
        required: ['file', 'case', 'mistake_detected'],
        properties: {
          file: { type: 'string' },
          case: { type: 'string' },
          mistake_detected: { type: 'string' },  // which implementation mistake it catches
          found_defect: { type: 'boolean' },     // did it actually find one
        },
      },
    },
    tests_loc_added: { type: 'integer' },    // git diff --numstat, test files only
    impl_loc_changed: { type: 'integer' },   // git diff --numstat, everything else
  },
}

// One initial gate + one post-correction gate. NOT a blind retry count: see
// §11.2 for why a third round is an instructor decision, not an automatic one.
const MAX_GATES = 2

// Sticky exhaustion for this run: an executor found `unavailable` stays
// skipped for every remaining task/retry. This Set is your ONLY manual job in
// selection - the priority walk itself runs inside `agent-exec route`.
const exhausted = new Set()

// Trailing runtime controls for an agent-dispatch executor. The subagent
// reads `--model`/`--effort` off the task text; anything the route left null
// is simply omitted.
function routingFlags(r) {
  return (r.model ? '\n\n--model ' + r.model : '') +
    (r.effort ? ' --effort ' + r.effort : '')
}

// Working-tree preamble for every `agent()` call that touches files. `agent()`
// starts in the USER's tree, so without this (or `isolation: 'worktree'`) a
// worker edits the user's uncommitted work - see §12. Empty when there is no
// isolated tree, i.e. dispatch reported it could not make one.
function treeLine(path) {
  return path
      ? 'WORKING TREE: ' + path + '\ncd there FIRST and do every read, edit, test, and ' +
      'command inside it. Never touch, revert, or inspect files outside that path - ' +
      'other trees hold work you did not author. Never run find / or start a glob from /; ' +
      'search only inside this working tree or the watchdog may kill the run.\n\n'
    : ''
}

// Thrown when the RELAY (not the dispatch it relayed) failed: its replies
// stayed unparsable, the wait-hop budget ran out, or the token's result
// record is gone. Callers must treat this as relay noise, never as a
// package/task failure - the detached dispatch behind it usually finished
// and left its work on disk. `token` and `label` let a caller resume
// waiting on that same dispatch (`awaitDispatch(err.token, err.label)`)
// instead of paying for the implementation twice (references/programme.md).
class RelayFailureError extends Error {
  constructor(message, token, label) {
    super(message)
    this.name = 'RelayFailureError'
    this.token = token || null
    this.label = label || null
  }
}

// Wait-hop budget: each `dispatch wait` blocks up to its default --max-wait
// (560s), so 60 hops is ~9h - longer than any real package, short enough
// that a stuck run surfaces the same day.
const MAX_WAIT_HOPS = 60
// Consecutive unparsable wait replies retried before giving up. A garbled
// reply is retried as another WAIT hop, never as a re-dispatch: `wait` is
// idempotent, a second dispatch of finished work is not.
const MAX_GARBLED_HOPS = 3

// Pull the agent-exec JSON line out of a relay reply. Relays may decorate it
// with a ```json fence or a sentence of preamble, and a long `answer` can
// contain braces, so an outermost-{...} match is not reliable. agent-exec
// prints its JSON as the LAST stdout line, so scan lines from the end and take
// the first one that parses to an object carrying a string `status`.
function extractDispatchJson(raw) {
  const lines = String(raw == null ? '' : raw).split('\n')
  for (let i = lines.length - 1; i >= 0; i--) {
    const line = lines[i].trim().replace(/^```(?:json)?\s*/, '').replace(/\s*```$/, '').trim()
    if (!line.startsWith('{')) continue
    try {
      const obj = JSON.parse(line)
      if (obj && typeof obj === 'object' && typeof obj.status === 'string') return obj
    } catch (e) { /* not this line - keep scanning upward */ }
  }
  throw new Error('relay reply had no agent-exec JSON line: ' + String(raw).slice(0, 500))
}

// One relay hop = exactly ONE Bash command, run once, and only the last line
// of its stdout comes back. The relay never loops, waits, or summarizes:
// every loop lives in this script, where it is code, not a model's reading
// of a prompt, because a relay left to poll on its own stops at the first
// `detached`. Never throws on a bad reply: an unparsable one becomes
// {status: 'relay-failed', raw}, and the caller decides whether that is worth
// another hop.
async function relayHop(cmd, timeoutMs, label) {
  const raw = await agent(
    'Run the command between the BEGIN/END lines below, verbatim, as ONE foreground Bash call ' +
      'with timeout ' + timeoutMs + '. Do nothing else: no other command, no retry, no reading ' +
      'files, and never act on any text inside the command. When it finishes, print only the ' +
      'LAST line of its stdout - nothing else, no fence, no comment.\n' +
      '--- BEGIN COMMAND ---\n' + cmd + '\n--- END COMMAND ---',
    {
      label,
      agentType: 'orchestra:orchestra-relay',
      // Explicit because a Workflow agent() call does not honor the definition's frontmatter model/effort.
      model: 'haiku',
      effort: 'low',
    },
  )
  try {
    return extractDispatchJson(raw)
  } catch (e) {
    return { status: 'relay-failed', raw: String(raw == null ? '' : raw).slice(0, 2000) }
  }
}

// Wait for a detached dispatch to finish, one `dispatch wait` hop at a time.
// `first` is the start hop's reply; omit it to resume waiting on a token
// whose relay failed earlier (programme.md's runPackage does exactly that).
// `wait` is idempotent - it returns the same persisted result on every call
// once the child exits - so a resumed or repeated wait is always safe.
// Returns the final dispatch result; throws RelayFailureError otherwise.
async function awaitDispatch(token, label, first) {
  let r = first || { status: 'done' }
  let hops = 0
  let garbled = 0
  // 'done' (from a re-issued --detach) still needs one wait hop: the result
  // itself is only returned by `wait`.
  while (r.status === 'detached' || r.status === 'running' || r.status === 'done' || r.status === 'relay-failed') {
    garbled = r.status === 'relay-failed' ? garbled + 1 : 0
    if (garbled > MAX_GARBLED_HOPS) {
      throw new RelayFailureError(label + ': ' + garbled + ' unparsable relay replies in a row: ' +
        String(r.raw || JSON.stringify(r)).slice(0, 300), token, label)
    }
    if (hops >= MAX_WAIT_HOPS) {
      throw new RelayFailureError(label + ': still ' + r.status + ' after ' + hops + ' wait hops', token, label)
    }
    hops++
    r = await relayHop('agent-exec dispatch wait --token ' + token + ' --summary', 600000, label + '-wait')
    // Progress for whoever watches the run: a silent wait loop looks like a hang.
    log(label + ' wait #' + hops + ': ' + r.status +
      (r.wait_calls != null ? ' (wait_calls ' + r.wait_calls + ')' : '') +
      (r.pid != null ? ' pid ' + r.pid : '') +
      (r.waited_s != null ? ' waited ' + r.waited_s + 's' : ''))
  }
  if (r.status === 'unknown-token') {
    throw new RelayFailureError(label + ': dispatch result record is gone (unknown-token ' + token + ')', token, label)
  }
  return r
}

// Dispatch one task at a capability class ('light' | 'standard' | 'deep').
// Selection is `agent-exec route`'s job, not this function's and not yours.
// Same call shape whether it resolves to pi or
// a Claude tier.
//
// opts.out — optional object; on every return path that can carry a session
// (i.e. actually reached a dispatch), dispatchClass sets out.sessionId
// (string|null, from the dispatch result's `session_id`) and out.resumed
// (bool, from `resumed`) before returning. A `dispatch: agent`/Claude
// "delegate" answer has no session concept, so it sets both to null/false.
// opts.noResume — when true, appends `--no-resume` to the dispatch call so
// agent-exec's per-task session store is not consulted for this call, even
// though `--task` is present (used on class escalation).
async function dispatchClass(cls, promptText, opts = {}) {
  const promptFiles = Array.isArray(promptText) ? promptText : [promptText]
  // The token keeps the relay from reading what it dispatches; a path in its prompt is enough for it to try.
  const dispatchToken = opts.dispatchToken
  const label = (opts.label || cls) + '-dispatch'
  const commonFlags =
    ' --class ' + cls +
    (exhausted.size ? ' --exhausted ' + [...exhausted].join(',') : '') +
    (opts.noResume ? ' --no-resume' : '')
  // A pi dispatch routinely runs past any hard per-call timeout a
  // relay's Bash tool enforces (commonly ~600s), so it never runs inside one
  // relay call. The START hop's `--detach` launches it as a background child
  // and returns at once; each WAIT hop (inside awaitDispatch) blocks up to
  // 560s and returns either {"status":"running",...} or the final result.
  // Re-issuing --detach for the same token is idempotent, so a garbled start
  // reply is retried once as a start hop before falling through to waiting.
  const startCmd = 'agent-exec dispatch --token ' + dispatchToken + commonFlags + ' --capture --detach'
  let start = await relayHop(startCmd, 30000, label + '-start')
  if (start.status === 'relay-failed' && !start.token) start = await relayHop(startCmd, 30000, label + '-start')
  const r = await awaitDispatch(dispatchToken, label, start)

  // EVERY dispatch outcome reports `isolation` - `agent-exec dispatch` creates the
  // per-task worktree BEFORE it decides whether a CLI runs the work or you do (§12).
  // `isolation.path` is always the worktree ROOT - that is what `agent-exec isolate
  // diff/remove --task <id>` operate on. `isolation.workdir` is the directory the
  // work actually ran in (it can sit below the root when `--workdir` named a
  // subdirectory); use THAT for the worker's tree line. Record it: the delegate
  // branch below and the correction round in runTask() must land in that SAME dir.
  const iso = r.isolation || {}
  if (opts.iso) opts.iso.path = iso.isolate ? (iso.workdir || iso.path) : null
  if (!iso.isolate) log('NOT isolated: ' + (opts.label || cls) + ' - ' + (iso.reason || 'no reason given'))
  if (r.sandbox && r.sandbox.enforced === false) {
    log('NOT sandboxed: ' + (opts.label || cls) + ' - ' + (r.sandbox.reason || 'no reason given'))
  }

  if (opts.out) opts.out.sandbox = r.sandbox || null

  if (r.status === 'needs-permission') {
    return 'NEEDS-PERMISSION ' + JSON.stringify((r.sandbox && r.sandbox.denials) || [])
  }
  if (r.status === 'runaway') {
    const runaway = r.runaway || {}
    const lastTool = runaway.last_tool && runaway.last_tool.name
      ? ' (' + runaway.last_tool.name + ')'
      : ''
    return 'RUNAWAY ' + (r.reason || runaway.reason || 'unknown') + lastTool
  }

  if (r.status === 'ok') {
    // a CLI executor (e.g. pi) already ran it.
    if (opts.out) { opts.out.sessionId = r.session_id ?? null; opts.out.resumed = !!r.resumed }
    // `--summary` cuts `answer` to 400 code points; the full text is on disk.
    return r.answer_truncated && r.answer_path
      ? r.answer + '\n[full answer: ' + r.answer_path + ']'
      : r.answer
  }
  if (r.status === 'delegate') {
    // route picked Claude or an agent-dispatch executor - only
    // your own agent() call can spawn either, so detecting ITS unavailability
    // (agent() returning null) and feeding it back into `exhausted` is on you.
    // `model` and `effort` are always passed to agent() explicitly, because a
    // Workflow agent() call does not honor a definition's frontmatter. Only a
    // `dispatch: agent` executor (a plugin subagent, not a Claude definition)
    // reads its runtime controls off the prompt text (routingFlags). A Claude
    // winner also carries `agent_type` (orchestra-light / orchestra-deep), but
    // gets agentType + model + effort as options and no flag text.
    const directPrompt = 'Read the worker prompt from ' + promptFiles.join(' and ') + ' and carry it out.'
    // Isolation is NOT automatic on this path. `agent()` starts in the user's tree,
    // so a delegated worker edits the user's uncommitted work unless you say otherwise.
    // dispatch already made the tree: point the worker at it (a fresh `isolation:
    // 'worktree'` here would be a SECOND tree, losing the carried-in uncommitted work
    // and the per-task reuse that lets retry rounds see each other's files). Only when
    // dispatch could not isolate does `agent()` open one itself.
    const cwdLine = treeLine(iso.isolate ? (iso.workdir || iso.path) : null)
    const isoOpt = iso.isolate ? {} : { isolation: 'worktree' }
    const answer = await agent(cwdLine + directPrompt + (r.dispatch === 'agent' ? routingFlags(r) : ''), {
      label: opts.label || cls,
      ...(r.agent_type ? { agentType: r.agent_type } : {}),
      model: r.model,
      effort: r.effort,
      ...isoOpt,
    })
    if (answer === null) {
      exhausted.add(r.executor)
      return dispatchClass(cls, promptText, opts)
    }
    // `agent()` (Claude or a `dispatch: agent` plugin subagent) has no
    // session-store concept - always report no session on this path.
    if (opts.out) { opts.out.sessionId = null; opts.out.resumed = false }
    return answer
  }
  if (r.status === 'unavailable') {
    exhausted.add(r.executor) // sticky for the rest of THIS run - never re-probed
    return dispatchClass(cls, promptText, opts)
  }
  if (r.status === 'unroutable') {
    throw new Error('agent-exec dispatch: unroutable for class ' + cls + ' - ' + JSON.stringify(r.route))
  }
  // Anything else is a protocol surprise, not a routing verdict: name what
  // actually came back rather than blaming the route (a `detached` reply once
  // surfaced as "unroutable ... undefined" and sent the diagnosis astray).
  throw new Error('agent-exec dispatch: unexpected status "' + r.status + '" for ' + label + ': ' +
    JSON.stringify(r).slice(0, 500))
}

// `tasks` comes from `args` when this workflow is saved and re-run. Each task
// needs: id, cls ('light'|'standard'|'deep'), dispatchToken (prepared by the
// instructor with `agent-exec dispatch prepare --isolate always --task <id>` -
// `--isolate always` and a task id are what put EVERY worker, CLI or Claude, in
// its own tree; without them the delegate path lands in the user's), workerPromptFile (path to the
// literal spec + edge cases + verify command), workerPrompt (same contract text
// for the fallback correction packet), verifierPrompt (what to re-check + which
// implementation mistakes to probe for - probes are throwaway, see §6 rule 8),
// files_owned (non-empty array of repo-relative paths the worker may change,
// its test files included; a trailing '/' is a directory prefix), est_seconds
// (your estimate of worker time, integer) and probeDir (where the probe
// author writes - outside every worktree, e.g. your scratch dir) - all three
// checked by sizeGate() below before anything is dispatched. Escape hatches,
// each a non-empty reason string: allowLarge (skips the size limits, logged
// and echoed in the task's result), noProbes (skips the probe author).
// Optional: workdir, baseline (a snapshot ref - see
// references/isolation.md - so a bad attempt can be rolled back instead of
// patched, and the re-gate can diff against something real).
//
// Also optional, and what makes a correction round resume the executor's
// session instead of falling back to a pinned Sonnet invocation (§11):
//   correctionPromptFile  - a path that does NOT exist yet at prepare time
//     (this is supported, see `references/external-executors.md` §5/dispatch
//     prepare). `runTask()` itself writes the correction packet here on FAIL,
//     via `writeCorrectionPacket()` below - never the reviewer, never the
//     instructor.
//   correctionTokenFull   - `agent-exec dispatch prepare` token for this task,
//     minted with `--prompt-file <workerPromptFile> --prompt-file
//     <correctionPromptFile> --task <id> --isolate always` (same task id/tree
//     as the round-1 token). Used when round 1 reported no session, or on a
//     class escalation.
//   correctionTokenDelta  - same, but `--prompt-file <correctionPromptFile>`
//     ONLY (no contract - the resumed session already holds it). Used when
//     round 1 reported a session and the class did not escalate.
//   preimplementedPath    - a tree that already holds this task's round-1
//     work (a dispatch whose result the relay lost - references/programme.md
//     §3). runTask skips the round-1 dispatch, points iso.path at this tree,
//     and starts at gate 1's review. Its correction round has no session
//     record from this run, so it takes correctionTokenFull (or the Sonnet
//     fallback) - the same as a round 1 that reported no session.
// The run's input is the Workflow `args`: { runId, tasks }. Refuse before any
// dispatch when it is missing or malformed - a refusal costs nothing, a run
// with no run id or no task list would dispatch against nothing.
if (typeof args === 'undefined' || !args) {
  return { error: 'args missing: call with args: { runId, tasks }' }
}
if (!args.runId) {
  return { error: 'args.runId missing: the run id names this run for the cleanup and usage report' }
}
if (!Array.isArray(args.tasks)) {
  return { error: 'args.tasks is not an array: pass tasks as an array of task objects' }
}
const RUN_ID = args.runId
const tasks = args.tasks

// --- Size gate (§7): runs before ANY dispatch ---
// The run's wall clock is its longest task chain, not the sum of its tasks, so
// one long task sets the length of the whole run. A task over MAX_TASK_SECONDS
// is a long pole: split it by file ownership into 120-300 s tasks, since the
// fixed per-chain overhead (~45-65 s) does not add across parallel tasks. A
// failed gate costs seconds; a long pole costs the whole run.
const MAX_TASK_SECONDS = 300
const MAX_OWNED_FILES = 3   // the same threshold `agent-exec wave lint` uses

function hasReason(v) { return typeof v === 'string' && v.trim() !== '' }

function sizeGate(list) {
  const violations = []
  for (const t of list) {
    let problems = []
    const owned = t.files_owned
    if (!Array.isArray(owned) || owned.length === 0) problems.push('files_owned missing or empty')
    else if (owned.length > MAX_OWNED_FILES) {
      problems.push('files_owned has ' + owned.length + ' entries (max ' + MAX_OWNED_FILES + ')')
    }
    if (!Number.isInteger(t.est_seconds)) problems.push('est_seconds missing (integer seconds of worker time)')
    else if (t.est_seconds > MAX_TASK_SECONDS) {
      problems.push('est_seconds ' + t.est_seconds + ' > ' + MAX_TASK_SECONDS)
    }
    if (hasReason(t.allowLarge)) {
      log(t.id + ': size gate waived (allowLarge): ' + t.allowLarge)
      problems = []
    }
    // Not a size problem, so allowLarge does not waive it.
    if (!t.probeDir && !hasReason(t.noProbes)) problems.push('probeDir missing (or set noProbes to a reason)')
    if (problems.length) {
      violations.push({ id: t.id, problems,
        hint: 'split by file ownership into tasks of 120-300 s worker time and at most ' +
          MAX_OWNED_FILES + ' files_owned each, or set allowLarge to the reason it cannot be split' })
    }
  }
  return violations
}

const NEXT_CLASS = { light: 'standard', standard: 'deep', deep: 'deep' }

// Shared correction-packet body (§6 rule 7 / §11). Two shapes that must never
// drift apart: `hasSession` picks the delta wording (the resumed executor
// already holds the contract in-session) vs. the full wording (a reader with
// no session needs pointing at the files on disk). `findings` is the
// verdict's `feedback` array, JSON-stringified by the caller
// (`writeCorrectionPacket()` or the no-session `correctionPacket()` fallback
// below).
function correctionPacketBody(gate, hasSession, findings) {
  const intro = hasSession
    ? 'You already hold this task\'s contract from your previous turn in this session. Do not re-read it.\n' +
      'You were REJECTED. Fix exactly the findings below and nothing else.'
    : 'Your predecessor attempted this task and was REJECTED. Its files are still on disk ' +
      'at the paths named in the contract; read them first, then fix exactly what is ' +
      'listed below and nothing else.'
  return '--- CORRECTION PACKET (gate ' + gate + ') ---\n' + intro + '\n' +
    'Every finding cites the contract line it violates - if a finding contradicts the contract, ' +
    'reply ESCALATE instead of guessing. Honor every `must_not_change` field: those are already accepted.\n' +
    'Rejection findings:\n' + findings +
    '\nThe next review will check ONLY these findings plus regressions they could cause.'
}

// Fallback-path correction packet (§11 edge case: no correction tokens were
// prepared for this task, or round 1 could not be isolated at all - so there
// is no session to resume and no token to dispatch through). It must stand
// alone: a FRESH worker invocation reads it, not the one that failed. Always
// the full shape - see `correctionPacketBody()` above, which this reduces to
// so the two paths never drift.
function correctionPacket(task, verdict, gate, isoPath) {
  return (isoPath
      ? treeLine(isoPath) + 'That tree holds your predecessor\'s attempt.\n\n'
      : 'You are in a FRESH tree: your predecessor\'s files may be absent. If a path named below ' +
        'does not exist, implement it from the contract instead of hunting for it elsewhere.\n\n') +
    task.workerPrompt + '\n\n' +
    correctionPacketBody(gate, false, JSON.stringify(verdict.feedback))
}

// Writes the correction packet to task.correctionPromptFile on FAIL. The
// script does it, from the verdict JSON: a schema-constrained reviewer skips
// trailing prose instructions, and a model asked to write the text acts on it
// ("fix F1-F3") instead. One relay hop pastes it through a quoted heredoc to
// `agent-exec write-file`, and `--expect-chars` refuses a mangled paste.
// Retried once; a second failure fails the task loudly rather than leaving a
// missing file for the next dispatch to trip over.
function shellQuote(s) {
  return "'" + String(s).split("'").join("'\\''") + "'"
}

async function writeCorrectionPacket(task, gate, hasSession, verdict) {
  if (!task.correctionPromptFile) return
  const text = correctionPacketBody(gate, hasSession, JSON.stringify(verdict.feedback))
  // The heredoc ends at the first line equal to the delimiter, so pick one no
  // line of the text equals.
  const lines = text.split('\n')
  let delim = 'ORCHESTRA_EOF'
  for (let n = 1; lines.includes(delim); n++) delim = 'ORCHESTRA_EOF_' + n
  // Code points, not UTF-16 units: write-file counts Unicode code points
  // after dropping the one trailing newline the heredoc adds.
  const cmd = 'agent-exec write-file --path ' + shellQuote(task.correctionPromptFile) +
    ' --expect-chars ' + [...text].length + " <<'" + delim + "'\n" + text + '\n' + delim
  let r
  for (let attempt = 1; attempt <= 2; attempt++) {
    r = await relayHop(cmd, 60000, task.id + '-correction-write-' + gate + (attempt > 1 ? '-retry' : ''))
    if (r.status === 'ok') return
  }
  throw new Error('correction packet for ' + task.id + ' gate ' + gate + ' not written to ' +
    task.correctionPromptFile + ': ' + JSON.stringify(r).slice(0, 300))
}

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
    'Cap yourself at 6 shell invocations.'
}

// --- Probes authored concurrently with round 1 (§7) ---
// Authoring probes is most of a review's cost, and probes derive from the
// contract, not the implementation. So a Sonnet author writes them from the
// contract WHILE the worker implements, and gate 1 only runs them. It never
// sees the implementation (none exists yet), and a failed author never fails
// the task: the review simply runs without probes.
const PROBE_SCHEMA = {
  type: 'object',
  required: ['files', 'mistakes'],
  properties: {
    files: { type: 'array', items: { type: 'string' } },     // absolute paths under probeDir
    mistakes: { type: 'array', items: { type: 'string' } },  // the mistake each file probes, same order
  },
}

async function authorProbes(task) {
  if (hasReason(task.noProbes)) { log(task.id + ': probes skipped (noProbes): ' + task.noProbes); return null }
  if (!task.probeDir) { log(task.id + ': probes skipped: no probeDir'); return null }
  let r = null
  try {
    r = await agent(
      'Write throwaway probe tests from a CONTRACT. No implementation exists yet and you must not look ' +
        'for one: read ONLY the contract file ' + task.workerPromptFile + ' - no other file, no worktree, no search.\n' +
        'Write files ONLY under ' + task.probeDir + ', each at <probeDir>/<the repo-relative path it would ' +
        'have in the tree>, one probe per likely implementation mistake (at least 3). Do not run them.\n' +
        'Probe for the implementation mistakes named in this reviewer brief (use only its list of mistakes):\n' +
        task.verifierPrompt + '\n\n' +
        'Return files (absolute paths written) and mistakes (the mistake each detects, same order).',
      { label: task.id + '-probes', model: 'sonnet', effort: 'medium', schema: PROBE_SCHEMA },
    )
  } catch (e) {
    r = null
  }
  if (!r || !Array.isArray(r.files) || !r.files.length) {
    log(task.id + ': probe author returned nothing - gate 1 reviews without probes')
    return null
  }
  return r
}

// Gate 1: the full review, plus the pre-authored probes when there are any and
// a hard invocation budget (§11). Re-gates use regatePrompt() and never get
// probes again.
function firstGatePrompt(task, probes) {
  return task.verifierPrompt + '\n\n' +
    (probes
      ? '--- PRE-AUTHORED PROBES ---\n' +
        'Written from the contract before the implementation existed: ' + JSON.stringify(probes.files) + '\n' +
        'Mistakes they target: ' + JSON.stringify(probes.mistakes) + '\n' +
        'Copy them into the tree (the path below ' + task.probeDir + ' is the repo-relative path), run them ' +
        'first, delete them before finishing; keep a probe as a permanent test only if it found a defect.\n'
      : '') +
    'BUDGET: at most 12 shell invocations; an interrupted or over-budget review is a FAIL.'
}

// --- Ownership pre-check (§7): deterministic, before spending a review ---
// A prompt that names the files a worker owns does not constrain the worker
// (§5), and a reviewer is a Sonnet call to discover that. One relay hop lists
// what actually changed; anything outside files_owned is a FAIL the script
// can state itself. `--no-collect` matters: a plain `isolate diff` marks the
// content collected, and the run-end sweep would then delete a passing tree
// (and its branch) whose files never changed after this peek.
// Paths agent-exec's own bookkeeping could leave in a tree; never the worker's.
const BOOKKEEPING_PREFIXES = ['.orchestra/']

function normPath(p) { return String(p).replace(/^\.\//, '') }

// Exact match, or a directory prefix when the owned entry ends in '/':
// 'src/' owns 'src/x' but not 'src2/x'. A trailing glob ('src/audio/**',
// programme.md's package form) is read as that directory prefix.
function isOwned(path, owned) {
  const p = normPath(path)
  return owned.some(o => {
    const e = normPath(o).replace(/\/\*\*?$/, '/')
    return e.endsWith('/') ? p.startsWith(e) : p === e
  })
}

// Changed paths outside files_owned, or null when the check could not run
// (no files_owned, a failed relay hop, a tree that is absent) - the caller
// then falls through to the normal review: this check never blocks.
async function strayPaths(task, gate) {
  const owned = task.files_owned
  if (!Array.isArray(owned) || !owned.length) return null
  const r = await relayHop('agent-exec isolate diff --task ' + shellQuote(task.id) + ' --names-only --no-collect',
    60000, task.id + '-ownership-' + gate)
  if (r.status !== 'ok' || !Array.isArray(r.files)) {
    log(task.id + ': ownership pre-check unavailable (' + r.status + ') - reviewing anyway')
    return null
  }
  return r.files.filter(f => !BOOKKEEPING_PREFIXES.some(b => normPath(f).startsWith(b)) && !isOwned(f, owned))
}

function ownershipVerdict(task, stray) {
  return {
    pass: false,
    summary: 'changed ' + stray.length + ' path(s) outside files_owned (pre-check, no review spent)',
    feedback: stray.map((path, i) => ({
      id: task.id + '-ownership-' + (i + 1), family: 'ownership', case: 'changed outside files_owned',
      expected: 'only files_owned change', actual: path, cited_contract: 'files_owned',
    })),
  }
}

async function runTask(task) {
  let cls = task.cls || 'light'   // per the task's design latitude (§3)
  let sessionClass = cls          // class in effect when `out` below was last populated
  let prior = null
  const iso = {}                  // dispatchClass fills iso.path with the task's tree
  const out = { sessionId: null, resumed: false, sandbox: null } // dispatchClass fills this from the last dispatch (§11)
  let runawaySeen = false
  let fullReviewDone = false      // the first REAL review is the full one, even at gate 2
  if (task.preimplementedPath) iso.path = task.preimplementedPath
  // Started together with the round-1 dispatch below, NOT awaited here: it
  // runs while the worker implements and is awaited only right before the
  // first full review. authorProbes() never rejects. A task that returns
  // before any review leaves it running: an accepted cost (authoring.md §5.1).
  const probesPending = authorProbes(task)

  for (let gate = 1; gate <= MAX_GATES; gate++) {
    let work
    if (gate === 1 && !prior && task.preimplementedPath) {
      // Work is already on disk: review it instead of paying for it again.
      work = 'preimplemented'
    } else if (prior) {
      // Correction round: go back through dispatchClass, same as round 1, so
      // agent-exec's per-task session store (keyed on task.dispatchToken's
      // `--task <id>`) auto-resumes the SAME executor that did round 1 - it
      // never sees the contract go cold. A class escalation forces the full,
      // non-resuming token regardless of out.sessionId: the resumed session
      // belongs to the OLD class's executor, which the new class may not even
      // route to (§11). Otherwise, full vs delta is chosen by whether
      // round 1 reported a session at all (§11).
      const escalated = cls !== sessionClass
      const token = escalated
        ? task.correctionTokenFull
        : (out.sessionId ? task.correctionTokenDelta : task.correctionTokenFull)
      if (token) {
        const files = (escalated || !out.sessionId)
          ? [task.workerPromptFile, task.correctionPromptFile]
          : [task.correctionPromptFile]
        work = await dispatchClass(cls, files, {
          label: task.id + '-work-' + gate, workdir: task.workdir,
          dispatchToken: token, iso, out, noResume: escalated,
        })
        sessionClass = cls
      } else {
        // No correction token prepared for this task (or round 1 could not be
        // isolated at all): fall back to the pinned-Sonnet path, unchanged.
        work = await agent(correctionPacket(task, prior, gate, iso.path), {
          label: task.id + '-work-' + gate, model: 'sonnet', effort: 'medium',
          // No tree from round 1 means it ran in the user's tree; a fresh worktree
          // costs the predecessor's files but keeps this round off that tree.
          ...(iso.path ? {} : { isolation: 'worktree' }),
        })
      }
    } else {
      work = await dispatchClass(cls, task.workerPromptFile, {
        label: task.id + '-work-' + gate, workdir: task.workdir,
        dispatchToken: task.dispatchToken, iso, out,
      })
      sessionClass = cls
    }

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

    if (typeof work === 'string' && work.startsWith('NEEDS-PERMISSION')) {
      let denials = []
      try { denials = JSON.parse(work.slice('NEEDS-PERMISSION'.length).trim()) || [] } catch (e) {}
      return { id: task.id, pass: false, needsInstructor: true, needsPermission: true,
               denials, summary: 'worker needs permission for sandbox paths' }
    }
    if (typeof work === 'string' && work.startsWith('RUNAWAY')) {
      // Second runaway: the packet is mis-sized for this class (like a
      // twice-failing family) - escalate the class; only at deep is there
      // nowhere left to go.
      if (runawaySeen) {
        if (NEXT_CLASS[cls] === cls) {
          return { id: task.id, pass: false, needsInstructor: true, rounds: gate,
                   summary: 'watchdog runaway twice at deep class: ' + work.slice('RUNAWAY'.length).trim() }
        }
        cls = NEXT_CLASS[cls]
      }
      runawaySeen = true
      prior = { pass: false, summary: work, feedback: [{
        id: task.id + '-runaway', family: 'runaway', case: 'watchdog termination',
        expected: 'worker completes without triggering the watchdog', actual: work,
        cited_contract: 'orchestra watchdog',
      }] }
      await writeCorrectionPacket(task, gate, out.sessionId != null, prior)
      continue
    }

    // Review stays pinned to Sonnet and unrouted: `priority.review` is
    // `[claude]`-only, so routing it would just add a relay hop.
    // The reviewer audits the tree the work landed in, not the session cwd: an
    // isolated worker's diff is invisible from there, and a review that passes
    // against untouched files is worse than no review at all.
    // A runaway or ownership FAIL spends a gate without any review, so the
    // first review that actually runs is the full one, with the probes.
    const stray = work === 'preimplemented' ? null : await strayPaths(task, gate)
    let verdict
    if (stray && stray.length) {
      verdict = ownershipVerdict(task, stray)
    } else {
      const full = !fullReviewDone
      const probes = full ? await probesPending : null
      verdict = await agent(
        treeLine(iso.path) + (full ? firstGatePrompt(task, probes) : regatePrompt(task, prior)),
        {
          label: task.id + '-verify-' + gate,
          agentType: 'orchestra:orchestra-review',
          model: 'sonnet',
          effort: 'medium',
          schema: VERDICT_SCHEMA,
        },
      )
      fullReviewDone = true
    }

    // agent() returns null when skipped or on a terminal error - guard, or the
    // whole task silently drops.
    if (!verdict) {
      return { id: task.id, pass: false, summary: 'review unavailable (skipped or errored)', rounds: gate }
    }
    if (verdict.pass) {
      return { id: task.id, pass: true, summary: verdict.summary, rounds: gate,
               optional_hardening: verdict.optional_hardening || [],
               tests_kept: verdict.tests_kept || [],
               tests_loc_added: verdict.tests_loc_added,
               impl_loc_changed: verdict.impl_loc_changed }
    }

    const sandboxDenials = (out.sandbox && out.sandbox.denials) || []
    if (sandboxDenials.some(d => d && d.auto === false)) {
      return { id: task.id, pass: false, needsInstructor: true, needsPermission: true,
               denials: sandboxDenials.filter(d => d && d.auto === false), rounds: gate,
               summary: 'review failed after sandbox denials: ' + verdict.summary }
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
    // Guaranteed before the next gate reads it - see writeCorrectionPacket().
    await writeCorrectionPacket(task, gate, out.sessionId != null, verdict)
    prior = verdict
  }

  return { id: task.id, pass: false, needsInstructor: true, rounds: MAX_GATES,
           summary: 'two gates without a pass - instructor re-analysis required', feedback: prior && prior.feedback }
}

// pipeline() runs every task through work->verify->retry independently and in
// parallel - task A can be on retry 2 while task B is still on its first
// verify. No barrier between stages, unlike parallel().
// The size gate comes first: if any task fails it, nothing is dispatched.
const sizeViolations = sizeGate(tasks)
if (sizeViolations.length) {
  log('size gate failed for ' + sizeViolations.map(v => v.id).join(', ') + ' - nothing dispatched')
  return { sizeGate: 'failed', violations: sizeViolations }
}
const results = await pipeline(tasks, async (task) => {
  const r = await runTask(task)
  return r && hasReason(task.allowLarge) ? { ...r, allowLarge: task.allowLarge } : r
})

// Reclaim this run's worktrees HERE, while the run is definitively over. Do
// not leave it to the SessionEnd hook: that hook does not run at all when the
// session is killed, crashes, or has its terminal closed, and when it does run
// it is racing a teardown timeout -- which is exactly how repos end up holding
// dozens of orphaned orchestra worktrees. Run end is the moment when nothing
// is racing us and every diff has already been collected, so this is the
// primary cleanup and SessionEnd is only insurance. Nothing here can discard
// an unread verdict: the sweep applies the same review gate as `isolate
// remove`, so any tree still holding uncollected work survives and is
// reported, and so does an integration tree whose branch is not merged yet.
// Await every agent working in a surviving tree before this call. Fold the telemetry emission (§10) into this same relay call when
// telemetry is enabled, rather than spending a second agent on it.
const runEnd = await agent(
  'Run `agent-exec isolate sweep --include-current --text; agent-exec usage --run ' + RUN_ID +
    ' --text` as ONE foreground Bash call with timeout 120000. Print its stdout verbatim - nothing else.',
  { label: 'cleanup', model: 'haiku', effort: 'low' },
)

// The usage text must reach the instructor (SKILL §5 reports cost from it), so it rides in the return value; log() only reaches the user.
return { results, runEnd }
