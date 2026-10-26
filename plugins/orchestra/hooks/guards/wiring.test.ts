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
