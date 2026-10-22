import { describe, expect, test } from 'claude-code/testing'
import type { On } from 'claude-code'
import { callTool, fakeHost, freshSession, type HostOptions } from '../testing/host.ts'

// General-purpose subagents on the instructor's own model (an omitted
// `model` inherits it) were the largest avoidable cost. Pinned: one deny per
// instructor session, never on the retry, and the carve-outs that keep it a
// nudge.

const host = (on: On, extra: Partial<HostOptions> = {}) => {
    const sessionId = freshSession()

    return { sessionId, ...fakeHost(on, { sessionId, ...extra }) }
}

const begin = ($: any, sessionId: string, model: string | undefined) =>
    $.classic.SessionStart({ session_id: sessionId, source: 'startup', ...(model ? { model } : {}) })

const spawn = ($: unknown, args: Record<string, unknown>, agentId?: string) =>
    callTool($, agentId, { tool: 'Agent', description: 'look around', prompt: 'survey the repo', ...args })

const generic = { subagent_type: 'general-purpose' }

describe('opus-generalist', () => {
    test('denies once per instructor session; the retry is never blocked', async ($, on) => {
        const { sessionId } = host(on)
        await begin($, sessionId, 'claude-opus-5')
        const first = await spawn($, generic)
        expect(first.deny).toContain('leaves `model` out, so it inherits')
        expect((await spawn($, generic)).deny, 'the retry').toBeUndefined()
        expect((await spawn($, { ...generic, model: 'opus' })).deny, 'one deny per session, full stop').toBeUndefined()
    })

    for (const args of [{ ...generic, model: 'fable' }, { model: 'claude-fable-5-1' }]) {
        test(`instructor class: ${JSON.stringify(args)}`, async ($, on) => {
            const { sessionId } = host(on)
            await begin($, sessionId, 'claude-opus-5')
            expect((await spawn($, args)).deny).toContain(`names \`model: ${args.model}\``)
        })
    }

    test('carve-outs, and the marker does not spend the one deny', async ($, on) => {
        const { sessionId } = host(on)
        await begin($, sessionId, 'claude-fable-5')
        const allowed: Record<string, unknown>[] = [
            { ...generic, model: 'sonnet' },
            { ...generic, model: 'haiku' },
            { ...generic, model: 'opus' },
            { model: 'claude-opus-5-5' },
            { subagent_type: 'Explore' },
            { subagent_type: 'orchestra:orchestra-deep' },
            { ...generic, model: 'opus', prompt: '[orchestra:allow-opus: needs the deep model] audit' },
            { ...generic, prompt: 'run agent-exec dispatch --class light ...' },
        ]
        for (const args of allowed) {
            expect((await spawn($, args)).deny, JSON.stringify(args)).toBeUndefined()
        }
        expect((await spawn($, generic, 'ag_1')).deny, 'a subagent\'s own spawns').toBeUndefined()
        expect((await spawn($, generic)).deny, 'the one deny is still there').toBeDefined()
    })

    test('only instructor sessions', async ($, on) => {
        const { sessionId } = host(on)
        await begin($, sessionId, 'claude-sonnet-5')
        expect((await spawn($, generic)).deny, 'cheap session').toBeUndefined()
        await begin($, sessionId, undefined)
        expect((await spawn($, generic)).deny, 'unknown model').toBeUndefined()
    })

    test('kill switch: ORCHESTRA_OPUS_NUDGE=off', async ($, on) => {
        const { sessionId } = host(on, { env: { ORCHESTRA_OPUS_NUDGE: 'off' } })
        await begin($, sessionId, 'claude-opus-5')
        expect((await spawn($, generic)).deny).toBeUndefined()
    })

    test('enforcement.opus_generalist=off', async ($, on) => {
        const { sessionId } = host(on, { agentExec: () => ({ exitCode: 0, stdout: '{"enforcement": {"opus_generalist": false}}' }) })
        await begin($, sessionId, 'claude-opus-5')
        expect((await spawn($, generic)).deny).toBeUndefined()
    })
})
