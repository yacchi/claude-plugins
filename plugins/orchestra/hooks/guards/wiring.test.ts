import { describe, expect, test } from 'claude-code/testing'
import { beginTurn, callTool, fakeHost, freshSession, startAgent } from '../testing/host.ts'

// The usage, fan-out and resume guards reach their calls through the module's
// one tool.call listener. One smoke test per guard, through that listener, so
// a call is denied or allowed by the wiring, not only by the guard alone.

const SONNET = { subagent_type: 'general-purpose', model: 'sonnet', description: 'x', prompt: 'do the thing' }

describe('guard wiring', () => {
    test('usage: a main-thread Agent call is denied at the stop level', async ($, on) => {
        fakeHost(on, { sessionId: freshSession(), rateLimits: [{ kind: 'five_hour', percentUsed: 95 }] })
        const held = await callTool($, undefined, { tool: 'Agent', ...SONNET })
        expect(held.deny).toContain('orchestra: Claude plan usage is')
    })

    test('usage: a main-thread Agent call below the stop level passes this guard', async ($, on) => {
        fakeHost(on, { sessionId: freshSession(), rateLimits: [{ kind: 'five_hour', percentUsed: 50 }] })
        expect((await callTool($, undefined, { tool: 'Agent', ...SONNET })).deny).toBeUndefined()
    })

    test('usage: at 80% an Agent call is allowed and carries the advice once', async ($, on) => {
        fakeHost(on, { sessionId: freshSession(), rateLimits: [{ kind: 'five_hour', percentUsed: 80 }] })
        const first = await callTool($, undefined, { tool: 'Agent', ...SONNET }) as { deny?: string, context?: string[] }
        expect(first.deny).toBeUndefined()
        expect(first.context).toHaveLength(1)
        expect(first.context?.[0]).toContain('orchestra: Claude plan usage is 80%')
        const second = await callTool($, undefined, { tool: 'Agent', ...SONNET }) as { context?: string[] }
        expect(second.context ?? []).toHaveLength(0)
    })

    test('usage: a Workflow call is denied at the stop level', async ($, on) => {
        fakeHost(on, { sessionId: freshSession(), rateLimits: [{ kind: 'five_hour', percentUsed: 95 }] })
        const held = await callTool($, undefined, { tool: 'Workflow', script: 'export const meta = {}' })
        expect(held.deny).toContain('orchestra: Claude plan usage is')
    })

    test('fan-out: a planner spawns only scouts and checkers', async ($, on) => {
        fakeHost(on, { sessionId: freshSession() })
        startAgent($, 'wiring-planner', 'orchestra:orchestra-planner', '/w')
        const denied = await callTool($, 'wiring-planner', { tool: 'Agent', ...SONNET })
        expect(denied.deny).toContain('orchestra: a planner spawns only')
    })

    test('fan-out: a scout is cut off at its tool budget, reads included', async ($, on) => {
        fakeHost(on, { sessionId: freshSession() })
        startAgent($, 'wiring-scout', 'orchestra:orchestra-scout', '/w')
        const read = { tool: 'Read', file_path: '/w/a.ts' }
        for (let i = 0; i < 15; i++) {
            expect((await callTool($, 'wiring-scout', read)).deny, `call ${i + 1}`).toBeUndefined()
        }
        expect((await callTool($, 'wiring-scout', read)).deny).toContain('orchestra: tool budget spent')
    })

    test('fan-out: a spec writer is cut off at its tool budget, but Write and Edit are never counted', async ($, on) => {
        fakeHost(on, { sessionId: freshSession() })
        startAgent($, 'wiring-writer', 'orchestra:orchestra-spec-writer', '/w')
        const read = { tool: 'Read', file_path: '/w/a.ts' }
        const write = { tool: 'Write', file_path: '/w/spec.md', content: 'x' }
        for (let i = 0; i < 40; i++) {
            expect((await callTool($, 'wiring-writer', read)).deny, `read ${i + 1}`).toBeUndefined()
            expect((await callTool($, 'wiring-writer', write)).deny, `write ${i + 1}`).toBeUndefined()
        }
        expect((await callTool($, 'wiring-writer', read)).deny).toContain('orchestra: tool budget spent (40 tool calls for an orchestra-spec-writer)')
        expect((await callTool($, 'wiring-writer', write)).deny).toBeUndefined()
    })

    test('resume: SendMessage to an agent that already holds a long context is denied', async ($, on) => {
        fakeHost(on, {
            sessionId: freshSession(),
            agentExec: args => args[0] === 'hook' && args[1] === 'context'
                ? { exitCode: 0, stdout: JSON.stringify({ context_tokens: 200000, limit: 100000, transcript: '/t' }) }
                : { exitCode: 0, stdout: '{"enforcement": {}}' },
        })
        startAgent($, 'w1', 'orchestra:orchestra-light', '/w', { name: 'w1' })
        const held = await callTool($, undefined, { tool: 'SendMessage', to: 'w1', message: 'fix it' })
        expect(held.deny).toContain('orchestra: this agent already holds')
    })
})

// The Bash listener's command guards. The worker node guard denies before
// classification. A Bash call's input is never changed: Claude Code's auto mode
// reviewed the call, so a heavy foreground call only asks the process watcher
// for a kill report, after the command has run.
const NODE_OPTIONS_REPLACED = 'NODE_OPTIONS=--no-experimental-strip-types pnpm test'
const REASON = 'pid 4242 (a fork bomb)'
const HEAVY = JSON.stringify({ op: 'jest', heavy: true, scope: 'targeted' })
const LIGHT = JSON.stringify({ op: 'ls', heavy: false, scope: 'targeted' })

// agent-exec's answers: the classify answer, and the process guard's report.
const answers = (classify: string, proc = '{"killed":false}') => (args: string[]) => {
    if (args[0] === 'slot' && args[1] === 'classify') {
        return { exitCode: 0, stdout: classify }
    }
    if (args[0] === 'proc' && args[1] === 'guard-result') {
        return { exitCode: 0, stdout: proc }
    }

    return { exitCode: 0, stdout: args[0] === 'config' ? '{"enforcement": {}}' : '' }
}

describe('process and node guard wiring', () => {
    test('a worker\'s NODE_OPTIONS replacement is denied before it is classified or run', async ($, on) => {
        const h = fakeHost(on, { sessionId: freshSession() })
        startAgent($, 'ag_node', 'orchestra:orchestra-light', '/w')
        const r = await callTool($, 'ag_node', { tool: 'Bash', command: NODE_OPTIONS_REPLACED, description: 'run tests' })
        expect(r.deny).toContain('replaces NODE_OPTIONS')
        expect(h.commands).toEqual([])
        expect(h.runs.some(r => r[4] === 'slot' && r[5] === 'classify')).toBe(false)
    })

    test('the same command on the main thread is not denied and runs as given', async ($, on) => {
        const h = fakeHost(on, { sessionId: freshSession() })
        const r = await callTool($, undefined, { tool: 'Bash', command: NODE_OPTIONS_REPLACED, description: 'run tests' })
        expect(r.deny).toBeUndefined()
        expect(h.commands).toEqual([NODE_OPTIONS_REPLACED])
    })

    test('the main thread\'s full suite is classified but not denied', async ($, on) => {
        fakeHost(on, {
            sessionId: freshSession(),
            agentExec: answers(JSON.stringify({ op: 'go test', heavy: true, scope: 'full' })),
        })
        const r = await callTool($, undefined, { tool: 'Bash', command: 'go test ./...', description: 'full suite' }) as { deny?: string }
        expect(r.deny).toBeUndefined()
    })

    test('a heavy foreground command the process guard killed reports the kill after it ran', async ($, on) => {
        let asked = -1
        const h = fakeHost(on, {
            sessionId: freshSession(),
            now: 5_000_000,
            agentExec: args => {
                if (args[0] === 'proc' && args[1] === 'guard-result') {
                    asked = h.commands.length

                    return { exitCode: 0, stdout: JSON.stringify({ killed: true, reason: REASON }) }
                }

                return answers(HEAVY)(args)
            },
        })
        const res = await callTool($, undefined, { tool: 'Bash', command: 'pnpm test', description: 'run tests' }) as { deny?: string, context?: string[] }
        expect(res.deny).toBeUndefined()
        expect(res.context).toHaveLength(1)
        expect(res.context?.[0]).toContain(`orchestra: agent-exec's process guard killed this command: ${REASON}`)
        const procs = h.runs.filter(r => r[4] === 'proc')
        expect(procs).toHaveLength(1)
        expect(procs[0].slice(4)).toEqual(['proc', 'guard-result', '--since=5000000'])
        expect(asked).toBe(1)
        expect(h.runs.filter(r => r[4] === 'slot' && r[5] === 'classify').map(r => r.slice(4))).toEqual([['slot', 'classify', '--command=pnpm test']])
        expect(h.runs.some(r => r[4] === 'slot' && r[5] === 'acquire')).toBe(false)
    })

    test('a heavy foreground command the process guard did not kill is returned as the tool produced it', async ($, on) => {
        fakeHost(on, { sessionId: freshSession(), agentExec: answers(HEAVY, '{"killed":false}') })
        expect(await callTool($, undefined, { tool: 'Bash', command: 'pnpm test', description: 'run tests' })).toEqual({ result: 'ran' })
    })

    test('no Bash call is rewritten: each reaches the base handler as given', async ($, on) => {
        const h = fakeHost(on, {
            sessionId: freshSession(),
            agentExec: answers(JSON.stringify({
                op: 'jest',
                heavy: true,
                scope: 'targeted',
                capped_command: 'pnpm test -- --maxWorkers=2',
                guard_prefix: '/x/agent-exec proc guard --root $$ --token 0123456789abcdef </dev/null >/dev/null 2>&1; ',
                guard_token: '0123456789abcdef',
            })),
        })
        startAgent($, 'ag_in', 'orchestra:orchestra-light', '/w')
        const foreground = { tool: 'Bash', command: 'pnpm test', description: 'run tests' }
        const main = { tool: 'Bash', command: 'pnpm test', description: 'run tests' }
        const background = { tool: 'Bash', command: 'pnpm test', description: 'run tests', run_in_background: true }
        await callTool($, 'ag_in', foreground)
        await callTool($, undefined, main)
        await callTool($, 'ag_in', background)
        expect(h.inputs).toHaveLength(3)
        for (const [i, args] of [foreground, main, background].entries()) {
            expect(h.inputs[i]).toMatchObject(args)
            expect(h.inputs[i].command).toBe('pnpm test')
        }
        expect(JSON.stringify(h.inputs)).not.toMatch(/capped_command|guard_prefix|maxWorkers=2|proc guard/)
        expect(h.commands).toEqual(['pnpm test', 'pnpm test', 'pnpm test'])
        expect(h.runs.some(r => r[4] === 'trace' && r.join(' ').includes('proc guard'))).toBe(false)
    })

    test('a worker\'s heavy background command is not admitted and is never asked for a kill', async ($, on) => {
        const h = fakeHost(on, { sessionId: freshSession(), agentExec: answers(HEAVY) })
        startAgent($, 'ag_bg', 'orchestra:orchestra-light', '/w')
        await callTool($, 'ag_bg', { tool: 'Bash', command: 'pnpm test', description: 'run tests', run_in_background: true })
        expect(h.commands).toEqual(['pnpm test'])
        expect(h.runs.some(r => r[4] === 'slot' && r[5] === 'acquire')).toBe(false)
        expect(h.runs.some(r => r[4] === 'proc')).toBe(false)
    })

    test('a command agent-exec classifies as not heavy is not asked for a kill', async ($, on) => {
        const h = fakeHost(on, { sessionId: freshSession(), agentExec: answers(LIGHT) })
        expect(await callTool($, undefined, { tool: 'Bash', command: 'ls', description: 'list' })).toEqual({ result: 'ran' })
        expect(h.runs.some(r => r[4] === 'proc')).toBe(false)
    })

    test('a command agent-exec cannot classify is not asked for a kill', async ($, on) => {
        const h = fakeHost(on, { sessionId: freshSession() })
        expect(await callTool($, undefined, { tool: 'Bash', command: 'ls', description: 'list' })).toEqual({ result: 'ran' })
        expect(h.runs.some(r => r[4] === 'proc')).toBe(false)
    })

    test('each main-loop turn starts the process watcher, and a subagent turn does not', async ($, on) => {
        const h = fakeHost(on, { sessionId: freshSession() })
        await beginTurn($, 't1')
        await beginTurn($, 's1', 'ag_w')
        await beginTurn($, 't2')
        await new Promise(resolve => setTimeout(resolve, 0))
        const watches = h.runs.filter(r => r[4] === 'proc' && r[5] === 'watch')
        expect(watches).toHaveLength(2)
        expect(watches.every(r => r.slice(4).join(' ') === 'proc watch')).toBe(true)
    })

    test('a missing agent-exec does not hold up a main-loop turn', async ($, on) => {
        fakeHost(on, { sessionId: freshSession(), agentExec: args => args[0] === 'proc' ? 'missing' : { exitCode: 0, stdout: '' } })
        expect(await beginTurn($, 't1')).toEqual({ turnId: 't1' })
    })

    test('a failing agent-exec does not hold up a main-loop turn', async ($, on) => {
        fakeHost(on, { sessionId: freshSession(), agentExec: args => args[0] === 'proc' ? { exitCode: 1 } : { exitCode: 0, stdout: '' } })
        expect(await beginTurn($, 't1')).toEqual({ turnId: 't1' })
    })
})
