import { describe, expect, test } from 'claude-code/testing'
import { callTool, fakeHost, freshSession, startAgent } from '../testing/host.ts'

// The wave watcher's guard and the handoff writer, through the module's one
// tool.call and turn.complete listeners.

const NEXT0 = 'the wave run command you were given, with --detach appended'
const OTHER = `orchestra: the wave watcher runs only \`agent-exec wave run … --detach\` and \`agent-exec wave wait --state <state> --max-wait 150 [--since <seq>]\`; it never reads files, sleeps or polls another way. Next: ${NEXT0}.`
const BACKGROUND = `orchestra: the wave watcher runs every command in the foreground (timeout 600000), never with run_in_background. Next: ${NEXT0}.`

// Every Bash call is classified as light and not heavy, so no slot is taken
// and no kill report is asked for; every other agent-exec answer is empty config.
const light = (args: readonly string[]) => args[0] === 'slot' && args[1] === 'classify'
    ? { exitCode: 0, stdout: '{"op":"x","heavy":false,"scope":"targeted"}' }
    : { exitCode: 0, stdout: '{"enforcement": {}}' }

const WAVE_RUN = 'agent-exec wave run --plan p --detach'
const WAVE_WAIT = 'agent-exec wave wait --state /st/w.json --max-wait 150'

describe('wave watcher wiring', () => {
    test('deny: the watcher\'s other command is denied with the commands it may run', async ($, on) => {
        const h = fakeHost(on, { sessionId: freshSession(), env: { ORCHESTRA_TRACE: 'off' }, agentExec: light })
        startAgent($, 'wiring-wave-1', 'orchestra:orchestra-wave', '/w')
        const r = await callTool($, 'wiring-wave-1', { tool: 'Bash', command: 'cat /tmp/out', description: 'read' })
        expect(r.deny).toBe(OTHER)
        expect(h.commands).not.toContain('cat /tmp/out')
    })

    test('deny: the watcher\'s wave run without --detach gets the watcher\'s text, not launch\'s', async ($, on) => {
        fakeHost(on, { sessionId: freshSession(), env: { ORCHESTRA_TRACE: 'off' }, agentExec: light })
        startAgent($, 'wiring-wave-2', 'orchestra:orchestra-wave', '/w')
        const r = await callTool($, 'wiring-wave-2', { tool: 'Bash', command: 'agent-exec wave run --plan p', description: 'run' })
        expect(r.deny).toBe(OTHER)
    })

    test('deny: a wait call with run_in_background is denied', async ($, on) => {
        fakeHost(on, { sessionId: freshSession(), env: { ORCHESTRA_TRACE: 'off' }, agentExec: light })
        startAgent($, 'wiring-wave-3', 'orchestra:orchestra-wave', '/w')
        const r = await callTool($, 'wiring-wave-3', { tool: 'Bash', command: WAVE_WAIT, description: 'wait', run_in_background: true })
        expect(r.deny).toBe(BACKGROUND)
    })

    test('allow: the watcher\'s detached run and its wait pass, exactly as given', async ($, on) => {
        const h = fakeHost(on, { sessionId: freshSession(), env: { ORCHESTRA_TRACE: 'off' }, agentExec: light })
        startAgent($, 'wiring-wave-4', 'orchestra:orchestra-wave', '/w')
        expect((await callTool($, 'wiring-wave-4', { tool: 'Bash', command: WAVE_RUN, description: 'run' })).deny).toBeUndefined()
        expect((await callTool($, 'wiring-wave-4', { tool: 'Bash', command: WAVE_WAIT, description: 'wait' })).deny).toBeUndefined()
        expect(h.commands).toContain(WAVE_RUN)
        expect(h.commands).toContain(WAVE_WAIT)
    })

    test('not the watcher: a general-purpose subagent and the main thread may read files', async ($, on) => {
        fakeHost(on, { sessionId: freshSession(), env: { ORCHESTRA_TRACE: 'off' }, agentExec: light })
        startAgent($, 'wiring-gp-1', 'general-purpose', '/w')
        expect((await callTool($, 'wiring-gp-1', { tool: 'Bash', command: 'cat /tmp/out', description: 'read' })).deny).toBeUndefined()
        expect((await callTool($, undefined, { tool: 'Bash', command: 'cat /tmp/out', description: 'read' })).deny).toBeUndefined()
    })
})

describe('wave watcher context', () => {
    const RUNNING = `orchestra: the wave is still running (status running): that is not a final answer. Run \`${WAVE_WAIT} --since 7\` now, and return only on needs, held, done, stopped or crashed.`
    const MOVED = `orchestra: this call was moved to the background, so its answer will not reach you. Do not read its output file. Run \`${WAVE_WAIT} --since 7\` in the foreground now.`

    test('each answer keeps the tool\'s own context and appends the watcher\'s, if any', async ($, on) => {
        // The base tool answers the watcher's calls in the order they are made.
        const replies: { result?: unknown, context?: string[] }[] = [
            { result: { stdout: '{"status":"detached","state":"/st/w.json"}\n' }, context: ['tool note'] },
            { result: { stdout: 'hop 1\n{"status":"running","seq":7}\n' }, context: ['tool note'] },
            { result: { stdout: '', backgroundTaskId: 'b1' }, context: ['tool note'] },
            { result: { stdout: '{"status":"done","seq":9}' }, context: ['tool note'] },
        ]
        fakeHost(on, { sessionId: freshSession(), env: { ORCHESTRA_TRACE: 'off' }, agentExec: light, toolCall: () => replies.shift() })
        startAgent($, 'wiring-ctx-1', 'orchestra:orchestra-wave', '/w')
        expect((await callTool($, 'wiring-ctx-1', { tool: 'Bash', command: WAVE_RUN, description: 'run' })).context).toEqual(['tool note'])
        expect((await callTool($, 'wiring-ctx-1', { tool: 'Bash', command: WAVE_WAIT, description: 'wait' })).context).toEqual(['tool note', RUNNING])
        expect((await callTool($, 'wiring-ctx-1', { tool: 'Bash', command: `${WAVE_WAIT} --since 7`, description: 'wait' })).context).toEqual(['tool note', MOVED])
        expect((await callTool($, 'wiring-ctx-1', { tool: 'Bash', command: `${WAVE_WAIT} --since 7`, description: 'wait' })).context).toEqual(['tool note'])
    })
})

// The handoff: a subagent's turn end writes its handoff file; the main loop's
// and other agents' do not.
const flush = async (): Promise<void> => {
    for (let i = 0; i < 20; i++) {
        await new Promise(r => setTimeout(r, 0))
    }
}

const handoffRuns = (runs: readonly string[][]) => runs.filter(r => r[0] === 'sh' && r[3] === 'agent-exec' && r[4] === 'hook' && r[5] === 'handoff')

describe('handoff wiring', () => {
    test('an orchestra agent\'s turn end writes its handoff with the answer as one argument', async ($, on) => {
        const session = freshSession()
        const h = fakeHost(on, { sessionId: session, env: { ORCHESTRA_TRACE: 'off' }, agentExec: light })
        startAgent($, 'wiring-ho-1', 'orchestra:orchestra-light', '/w')
        await $.turn.complete({ agentId: 'wiring-ho-1', turnId: 't', answer: 'all done', reason: 'answer', durationMs: 1, isAborted: false })
        await flush()
        const runs = handoffRuns(h.runs)
        expect(runs).toHaveLength(1)
        expect(runs[0].slice(4)).toEqual(['hook', 'handoff', '--session', session, '--agent', 'wiring-ho-1', '--type', 'orchestra:orchestra-light', '--answer=all done'])
    })

    test('a general-purpose subagent and the main loop write no handoff', async ($, on) => {
        const h = fakeHost(on, { sessionId: freshSession(), env: { ORCHESTRA_TRACE: 'off' }, agentExec: light })
        startAgent($, 'wiring-ho-gp', 'general-purpose', '/w')
        await $.turn.complete({ agentId: 'wiring-ho-gp', turnId: 't', answer: 'all done', reason: 'answer', durationMs: 1, isAborted: false })
        await $.turn.complete({ turnId: 't', answer: 'all done', reason: 'answer', durationMs: 1, isAborted: false })
        await flush()
        expect(handoffRuns(h.runs)).toHaveLength(0)
    })
})
