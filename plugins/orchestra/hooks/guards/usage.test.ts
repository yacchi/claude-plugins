import { expect, test } from 'claude-code/testing'
import type { Io, RateLimit } from '../lib/io.ts'
import { checkUsage, usageAdvice } from './usage.ts'

// The plan's five-hour window decides whether new agents start. Pinned: the
// stop level (95 by default, 90 still in config reads as unset), the advice
// once per 5-point step, the two main-thread escapes, the exempt control-plane
// agents, and fail-open on every doubt.

type Script = {
    windows?: readonly RateLimit[] | 'throw',
    env?: string,
    config?: Readonly<Record<string, unknown>> | 'missing' | 'failed',
}

let seq = 0
const freshSession = (): string => `usage-test-${++seq}`

const fakeIo = (s: Script): Io => ({
    env: async (name: string) => name === 'ORCHESTRA_USAGE_GUARD' ? s.env : undefined,
    rateLimits: async () => {
        if (s.windows === 'throw') {
            throw new Error('no reading')
        }

        return s.windows ?? []
    },
    run: async (argv: readonly string[]) => {
        if (s.config === 'missing') {
            return { exitCode: 127, stdout: '', stderr: '' }
        }
        if (s.config === 'failed') {
            return { exitCode: 1, stdout: '', stderr: 'boom' }
        }

        return { exitCode: 0, stdout: JSON.stringify({ enforcement: s.config ?? {} }), stderr: '' }
    },
}) as unknown as Io

const five = (percentUsed: number, resetsAt?: string): RateLimit[] => [{ kind: 'five_hour', percentUsed, resetsAt }]

const main = (io: Io, sessionId: string, tool = 'Agent', extra: Record<string, unknown> = {}) =>
    checkUsage(io, tool, { description: 'do it', prompt: 'do the thing', subagent_type: 'general-purpose', ...extra }, sessionId, undefined)

test('the stop level is 95 by default; 90 in config reads as unset', async () => {
    expect(await main(fakeIo({ windows: five(94.9), config: 'missing' }), freshSession())).toBeUndefined()
    expect(await main(fakeIo({ windows: five(95), config: 'missing' }), freshSession())).toContain('threshold 95%')
    expect(await main(fakeIo({ windows: five(92), config: { usage_guard: 90 } }), freshSession())).toBeUndefined()
})

test('usage_guard lowers the stop level; "off" and false disable it, and so does ORCHESTRA_USAGE_GUARD=off', async () => {
    expect(await main(fakeIo({ windows: five(75), config: { usage_guard: 75 } }), freshSession())).toContain('threshold 75%')
    expect(await main(fakeIo({ windows: five(74.9), config: { usage_guard: 75 } }), freshSession())).toBeUndefined()
    expect(await main(fakeIo({ windows: five(99), config: { usage_guard: 'off' } }), freshSession())).toBeUndefined()
    expect(await main(fakeIo({ windows: five(99), config: { usage_guard: false } }), freshSession())).toBeUndefined()
    expect(await main(fakeIo({ windows: five(99), env: 'off', config: 'missing' }), freshSession())).toBeUndefined()
})

test('no five-hour window, or a rejected reading, allows', async () => {
    expect(await main(fakeIo({ windows: [{ kind: 'seven_day', percentUsed: 99 }], config: 'missing' }), freshSession())).toBeUndefined()
    expect(await main(fakeIo({ windows: [], config: 'missing' }), freshSession())).toBeUndefined()
    expect(await main(fakeIo({ windows: 'throw', config: 'missing' }), freshSession())).toBeUndefined()
})

test('the control-plane agents are exempt; other agents and Workflow are denied', async () => {
    const io = fakeIo({ windows: five(99), config: 'missing' })
    expect(await main(io, freshSession(), 'Agent', { subagent_type: 'orchestra:orchestra-wave' })).toBeUndefined()
    expect(await main(io, freshSession(), 'Task', { subagent_type: ' Orchestra:Orchestra-Relay ' })).toBeUndefined()
    expect(await main(io, freshSession(), 'Agent', { subagent_type: 'orchestra:orchestra-light' })).toBeDefined()
    expect(await main(io, freshSession(), 'Workflow', {})).toBeDefined()
})

test('main-thread escape is honored twice per session, and the third is denied', async () => {
    const io = fakeIo({ windows: five(96), config: 'missing' })
    const sid = freshSession()
    const escaped = { description: 'x [orchestra:allow-usage: user asked]' }
    expect(await main(io, sid, 'Agent', escaped)).toBeUndefined()
    expect(await main(io, sid, 'Agent', { prompt: 'p [orchestra:allow-usage]' })).toBeUndefined()
    const third = await main(io, sid, 'Agent', escaped)
    expect(third).toContain('the 2 escapes of this session are used up')
    expect(await main(io, freshSession(), 'Agent', escaped)).toBeUndefined()
})

test('the escape is ignored inside a subagent and for Workflow', async () => {
    const io = fakeIo({ windows: five(96), config: 'missing' })
    const escaped = { description: '[orchestra:allow-usage]' }
    const inside = await checkUsage(io, 'Agent', escaped, freshSession(), 'ag_1')
    expect(inside).toContain('threshold 95%')
    expect(await main(io, freshSession(), 'Workflow', escaped)).toContain('threshold 95%')
})

test('advice is allowed, once per 5-point step, from usage_advise (default 75)', async () => {
    const sid = freshSession()
    const io = (p: number) => fakeIo({ windows: five(p, '2026-10-26T12:00:00Z'), config: 'missing' })
    expect(await main(io(74.9), sid)).toBeUndefined()
    expect(await usageAdvice(io(74.9), 'Agent', {}, sid)).toBeUndefined()
    expect(await main(io(76), sid)).toBeUndefined()
    const first = await usageAdvice(io(76), 'Agent', {}, sid)
    expect(first).toContain('Claude plan usage is 76% of the 5-hour window (resets 2026-10-26T12:00:00Z).')
    expect(await usageAdvice(io(79), 'Agent', {}, sid), 'same step').toBeUndefined()
    expect(await usageAdvice(io(80), 'Agent', {}, sid), 'next step').toBeDefined()
    expect(await usageAdvice(io(84), 'Agent', {}, sid), 'same step').toBeUndefined()
    expect(await usageAdvice(io(94.5), 'Agent', {}, sid), 'step 90, below stop').toBeDefined()
})

test('a missing or "off" usage_advise: default 75 still advises; "off" stays silent', async () => {
    expect(await usageAdvice(fakeIo({ windows: five(80), config: {} }), 'Agent', {}, freshSession())).toBeDefined()
    expect(await usageAdvice(fakeIo({ windows: five(80), config: { usage_advise: 'off' } }), 'Agent', {}, freshSession())).toBeUndefined()
})

test('no advice at the stop level, for exempt agents, or for other tools', async () => {
    const io = fakeIo({ windows: five(96), config: 'missing' })
    expect(await usageAdvice(io, 'Agent', {}, freshSession())).toBeUndefined()
    const low = fakeIo({ windows: five(85), config: 'missing' })
    expect(await usageAdvice(low, 'Agent', { subagent_type: 'orchestra:orchestra-wave' }, freshSession())).toBeUndefined()
    expect(await usageAdvice(low, 'Bash', {}, freshSession())).toBeUndefined()
})
