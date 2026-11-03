import { describe, expect, test } from 'claude-code/testing'
import { callTool, fakeHost, freshSession, startAgent } from '../testing/host.ts'

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

// The Bash listener's two command guards: the worker node guard denies before
// classification, and every classified command runs behind the process guard
// prefix (a foreground one also asks for its verdict once it has run).
const PREFIX = '/x/agent-exec proc guard --root $$ --token 0123456789abcdef </dev/null >/dev/null 2>&1; '
const TOKEN = '0123456789abcdef'
const REASON = 'process guard: 312 processes under pid 4242 (limit 200)'
const CAPPED = 'pnpm test -- --maxWorkers=2'
const NODE_OPTIONS_REPLACED = 'NODE_OPTIONS=--no-experimental-strip-types pnpm test'

type Classify = { op?: string, scope?: 'full' | 'targeted', capped?: boolean, guarded?: boolean }

const classified = ({ op = 'jest', scope = 'targeted', capped = false, guarded = false }: Classify) => JSON.stringify({
    op,
    heavy: true,
    scope,
    ...(capped ? { capped_command: CAPPED } : {}),
    ...(guarded ? { guard_prefix: PREFIX, guard_token: TOKEN } : {}),
})

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

    test('a main-thread command agent-exec guards runs behind the prefix, and a kill is reported on its result', async ($, on) => {
        let asked: number | undefined
        const h = fakeHost(on, {
            sessionId: freshSession(),
            agentExec: args => {
                if (args[0] === 'slot' && args[1] === 'classify') {
                    return { exitCode: 0, stdout: classified({ guarded: true }) }
                }
                if (args[0] === 'proc') {
                    asked = h.commands.length

                    return { exitCode: 0, stdout: JSON.stringify({ killed: true, reason: REASON }) }
                }

                return { exitCode: 0, stdout: args[0] === 'config' ? '{"enforcement": {}}' : '' }
            },
        })
        const r = await callTool($, undefined, { tool: 'Bash', command: 'pnpm test', description: 'run tests' }) as { deny?: string, result?: unknown, context?: string[] }
        expect(r.deny).toBeUndefined()
        expect(r.context).toHaveLength(1)
        expect(r.context?.[0]).toContain(`agent-exec's process guard killed this command: ${REASON}`)
        expect(h.commands).toEqual([PREFIX + 'pnpm test'])
        expect(asked).toBe(1)
        expect(h.runs.some(r => r[4] === 'slot' && r[5] === 'acquire')).toBe(false)
        expect(h.runs.filter(r => r[4] === 'slot' && r[5] === 'classify')).toHaveLength(1)
    })

    test('a main-thread command the process guard did not kill returns the tool result unchanged', async ($, on) => {
        fakeHost(on, {
            sessionId: freshSession(),
            agentExec: args => args[0] === 'slot' && args[1] === 'classify'
                ? { exitCode: 0, stdout: classified({ guarded: true }) }
                : args[0] === 'proc'
                    ? { exitCode: 0, stdout: '{"killed":false}' }
                    : { exitCode: 0, stdout: args[0] === 'config' ? '{"enforcement": {}}' : '' },
        })
        expect(await callTool($, undefined, { tool: 'Bash', command: 'pnpm test', description: 'run tests' })).toEqual({ result: 'ran' })
    })

    test('the main thread\'s full suite is classified but not denied', async ($, on) => {
        fakeHost(on, {
            sessionId: freshSession(),
            agentExec: args => args[0] === 'slot' && args[1] === 'classify'
                ? { exitCode: 0, stdout: classified({ op: 'go test', scope: 'full', guarded: true }) }
                : { exitCode: 0, stdout: args[0] === 'config' ? '{"enforcement": {}}' : '' },
        })
        const r = await callTool($, undefined, { tool: 'Bash', command: 'go test ./...', description: 'full suite' }) as { deny?: string }
        expect(r.deny).toBeUndefined()
    })

    test('a worker\'s capped command runs as the capped line behind the prefix; the slot asks for the command as given', async ($, on) => {
        const h = fakeHost(on, {
            sessionId: freshSession(),
            agentExec: args => args[0] === 'slot' && args[1] === 'classify'
                ? { exitCode: 0, stdout: classified({ capped: true, guarded: true }) }
                : { exitCode: 0, stdout: args[0] === 'config' ? '{"enforcement": {}}' : '' },
        })
        startAgent($, 'ag_cap', 'orchestra:orchestra-light', '/w')
        await callTool($, 'ag_cap', { tool: 'Bash', command: 'pnpm test', description: 'run tests' })
        expect(h.commands).toEqual([PREFIX + CAPPED])
        expect(h.runs.find(r => r[4] === 'slot' && r[5] === 'acquire')?.[6]).toBe('--command=pnpm test')
        expect(h.runs.some(r => r[4] === 'trace' && r.join(' ').includes(PREFIX))).toBe(false)
    })

    test('a worker\'s capped command without a guard runs as the capped line alone', async ($, on) => {
        const h = fakeHost(on, {
            sessionId: freshSession(),
            agentExec: args => args[0] === 'slot' && args[1] === 'classify'
                ? { exitCode: 0, stdout: classified({ capped: true }) }
                : { exitCode: 0, stdout: args[0] === 'config' ? '{"enforcement": {}}' : '' },
        })
        startAgent($, 'ag_cap2', 'orchestra:orchestra-light', '/w')
        await callTool($, 'ag_cap2', { tool: 'Bash', command: 'pnpm test', description: 'run tests' })
        expect(h.commands).toEqual([CAPPED])
    })

    test('a background command runs behind the prefix, is not admitted and is not asked for a verdict', async ($, on) => {
        const h = fakeHost(on, {
            sessionId: freshSession(),
            agentExec: args => args[0] === 'slot' && args[1] === 'classify'
                ? { exitCode: 0, stdout: classified({ capped: true, guarded: true }) }
                : { exitCode: 0, stdout: args[0] === 'config' ? '{"enforcement": {}}' : '' },
        })
        startAgent($, 'ag_bg', 'orchestra:orchestra-light', '/w')
        await callTool($, 'ag_bg', { tool: 'Bash', command: 'pnpm test', description: 'run tests', run_in_background: true })
        expect(h.commands).toEqual([PREFIX + 'pnpm test'])
        expect(h.runs.some(r => r[4] === 'proc')).toBe(false)
        expect(h.runs.some(r => r[4] === 'slot' && r[5] === 'acquire')).toBe(false)
    })

    test('a command agent-exec does not guard runs as given, with no verdict asked', async ($, on) => {
        const h = fakeHost(on, { sessionId: freshSession() })
        expect(await callTool($, undefined, { tool: 'Bash', command: 'ls', description: 'list' })).toEqual({ result: 'ran' })
        expect(h.commands).toEqual(['ls'])
        expect(h.runs.some(r => r[4] === 'proc')).toBe(false)
    })
})
