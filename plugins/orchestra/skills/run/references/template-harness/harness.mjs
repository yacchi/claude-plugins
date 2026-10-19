// Regression guard for ../workflow-template.md's script: extracts its
// ```javascript block, runs it with stubbed Workflow globals, and checks the
// size gate, the concurrent probe author and the ownership pre-check.
// No dependencies.
//
//   node plugins/orchestra/skills/run/references/template-harness/harness.mjs
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const md = readFileSync(join(here, '..', 'workflow-template.md'), 'utf8')
const block = md.match(/```javascript\n([\s\S]*?)\n```/)
if (!block) throw new Error('no ```javascript block in workflow-template.md')
const body = block[1].replace(/^export const meta/m, 'const meta')
const AsyncFunction = Object.getPrototypeOf(async function () {}).constructor
const script = new AsyncFunction('agent', 'pipeline', 'parallel', 'log', 'args', body)

const tick = () => new Promise(r => setTimeout(r, 0))

// One stubbed run. `events` records agent() starts/ends in order; `probe`
// is what the probe author returns (null = failed author); `diffs` are the
// `isolate diff` file lists per pre-check hop, in order ('garbled' = an
// unparsable relay reply; past the end = only owned files).
async function run(tasks, { probe = 'ok', diffs = [] } = {}) {
  let diffHop = 0
  const events = []
  const calls = []
  const logs = []
  const agent = async (prompt, opts) => {
    const label = opts.label || ''
    calls.push({ label, prompt, opts })
    events.push('start:' + label)
    let out
    if (opts.agentType === 'orchestra:orchestra-relay') {
      // Round-1 wait hops take a few ticks so a concurrent start is observable.
      for (let i = 0; i < 3; i++) await tick()
      if (prompt.includes('isolate diff')) {
        const files = diffHop < diffs.length ? diffs[diffHop] : ['src/a.js', 'test/a.test.js']
        diffHop++
        out = files === 'garbled' ? 'sorry, I could not run that' : JSON.stringify({ status: 'ok', files })
      } else if (prompt.includes('write-file')) {
        out = JSON.stringify({ status: 'ok' })
      } else {
        out = prompt.includes('dispatch wait')
          ? JSON.stringify({ status: 'ok', answer: 'done', isolation: { isolate: true, path: '/wt/' + label } })
          : JSON.stringify({ status: 'detached' })
      }
    } else if (opts.schema && opts.schema.required.includes('files')) {
      // Slower than the round-1 start hop, so it must overlap round 1.
      for (let i = 0; i < 5; i++) await tick()
      out = probe === 'ok' ? { files: ['/probes/t1/test/a_probe.test.js'], mistakes: ['off-by-one'] } : null
    } else if (opts.schema) {
      out = { pass: true, summary: 'ok' }
    } else {
      out = 'swept'
    }
    events.push('end:' + label)
    return out
  }
  const pipeline = async (items, fn) => Promise.all(items.map(fn))
  const parallel = async fns => Promise.all(fns.map(f => f()))
  const result = await script(agent, pipeline, parallel, m => logs.push(m), { tasks })
  return { result, events, calls, logs }
}

const base = id => ({
  id, cls: 'light', dispatchToken: 'tok-' + id, workerPromptFile: '/c/' + id + '.md',
  workerPrompt: 'contract', verifierPrompt: 'probe for: off-by-one', probeDir: '/probes/' + id,
  files_owned: ['src/a.js', 'test/'], est_seconds: 200,
})

let passed = 0
let failed = 0
async function check(name, fn) {
  try {
    await fn()
    passed++
    console.log('PASS ' + name)
  } catch (e) {
    failed++
    console.log('FAIL ' + name + ': ' + e.message)
  }
}
function assert(cond, msg) { if (!cond) throw new Error(msg) }

await check('(a) oversized task fails the size gate with zero agent() calls', async () => {
  const big = { ...base('big'), est_seconds: 900 }
  const wide = { ...base('wide'), files_owned: ['a', 'b', 'c', 'd'] }
  const bare = { ...base('bare'), files_owned: [], est_seconds: undefined }
  const { result, calls } = await run([base('ok'), big, wide, bare])
  assert(calls.length === 0, 'agent() called ' + calls.length + ' times')
  assert(result.sizeGate === 'failed', 'sizeGate=' + result.sizeGate)
  const ids = result.violations.map(v => v.id).join(',')
  assert(ids === 'big,wide,bare', 'violations ' + ids)
  assert(result.violations.find(v => v.id === 'bare').problems.length === 2, 'bare should have 2 problems')
  assert(result.violations.every(v => /split by file ownership/.test(v.hint) && /allowLarge/.test(v.hint)), 'hint')
})

await check('(b) allowLarge passes the gate and is echoed in the result', async () => {
  const big = { ...base('big'), est_seconds: 900, allowLarge: 'one generated file' }
  const { result, logs } = await run([big])
  assert(Array.isArray(result) && result[0].pass === true, 'task did not run to a pass')
  assert(result[0].allowLarge === 'one generated file', 'allowLarge not echoed')
  assert(logs.some(l => l.includes('allowLarge') && l.includes('one generated file')), 'allowLarge not logged')
})

await check('(c) probe author starts before round 1 resolves, is awaited before review', async () => {
  const { events, calls } = await run([base('t1')])
  const i = e => events.indexOf(e)
  assert(i('start:t1-probes') >= 0, 'probe author never started')
  assert(i('start:t1-probes') < i('end:t1-work-1-dispatch-start'), 'probes started after the round-1 start hop returned')
  assert(i('start:t1-work-1-dispatch-start') < i('end:t1-probes'), 'round 1 waited for the probe author')
  assert(i('end:t1-probes') < i('start:t1-verify-1'), 'review started before probes resolved')
  const probes = calls.find(c => c.label === 't1-probes')
  assert(probes.opts.model === 'sonnet' && !probes.prompt.includes('WORKING TREE'), 'probe author model/tree')
  const review = calls.find(c => c.label === 't1-verify-1').prompt
  assert(review.includes('/probes/t1/test/a_probe.test.js'), 'review prompt lacks probe files')
  assert(review.includes('at most 12 shell invocations; an interrupted or over-budget review is a FAIL'), 'budget line')
})

await check('(c2) a failed probe author does not fail the task; review runs without probes', async () => {
  const { result, calls, logs } = await run([base('t1')], { probe: 'null' })
  assert(result[0].pass === true, 'task failed')
  const review = calls.find(c => c.label === 't1-verify-1').prompt
  assert(!review.includes('PRE-AUTHORED PROBES') && review.includes('at most 12 shell'), 'review prompt')
  assert(logs.some(l => l.includes('without probes')), 'not logged')
})

await check('(c3) noProbes skips the author and is logged', async () => {
  const t = { ...base('t1'), probeDir: undefined, noProbes: 'docs-only change' }
  const { result, calls, logs } = await run([t])
  assert(result[0].pass === true, 'task failed')
  assert(!calls.some(c => c.label === 't1-probes'), 'probe author ran')
  assert(logs.some(l => l.includes('docs-only change')), 'not logged')
})

await check('(d) stray path: no reviewer call, ownership correction round, then the full review', async () => {
  const t = { ...base('t1'), files_owned: ['src/', 'test/'], correctionPromptFile: '/c/t1-fix.md' }
  // test/ is owned (test files listed in files_owned are never flagged),
  // .orchestra/ is bookkeeping, and 'src/' must not own 'src2/x.js'.
  const { result, calls } = await run([t], { diffs: [['src/a.js', 'test/a.test.js', '.orchestra/state', 'src2/x.js']] })
  assert(!calls.some(c => c.label === 't1-verify-1'), 'reviewer ran at gate 1')
  const diff = calls.find(c => c.label === 't1-ownership-1')
  assert(diff && diff.prompt.includes('--names-only --no-collect'), 'pre-check command')
  const write = calls.find(c => c.label === 't1-correction-write-1')
  assert(write, 'no correction packet written')
  assert(write.prompt.includes('"family":"ownership"') && write.prompt.includes('"cited_contract":"files_owned"') &&
    write.prompt.includes('"expected":"only files_owned change"') && write.prompt.includes('"actual":"src2/x.js"'), 'finding shape')
  assert(!write.prompt.includes('test/a.test.js') && !write.prompt.includes('.orchestra/state') &&
    !write.prompt.includes('"actual":"src/a.js"'), 'owned/bookkeeping path flagged')
  const fix = calls.find(c => c.label === 't1-work-2')
  assert(fix && fix.prompt.includes('src2/x.js'), 'no ownership correction round')
  const review = calls.find(c => c.label === 't1-verify-2')
  assert(review && review.prompt.includes('PRE-AUTHORED PROBES') && !review.prompt.includes('RE-GATE'), 'gate 2 not the full review')
  assert(result[0].pass === true && result[0].rounds === 2, 'result')
})

await check('(d2) a trailing glob owns its directory; everything owned goes straight to review', async () => {
  const t = { ...base('t1'), files_owned: ['src/audio/**', 'test/'] }
  const { calls } = await run([t], { diffs: [['src/audio/mix.js', 'test/mix.test.js']] })
  assert(calls.some(c => c.label === 't1-verify-1'), 'owned paths flagged as stray')
})

await check('(e) a failed pre-check hop falls through to the review', async () => {
  const { result, calls, logs } = await run([base('t1')], { diffs: ['garbled', 'garbled'] })
  assert(calls.some(c => c.label === 't1-verify-1'), 'review did not run')
  assert(result[0].pass === true && result[0].rounds === 1, 'result')
  assert(logs.some(l => l.includes('ownership pre-check unavailable')), 'not logged')
})

console.log(passed + ' passed, ' + failed + ' failed')
process.exit(failed ? 1 : 0)
