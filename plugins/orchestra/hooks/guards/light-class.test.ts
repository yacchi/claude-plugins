import { describe, expect, test } from 'claude-code/testing'
import type { On } from 'claude-code'
import { callTool, fakeHost, freshSession, type HostOptions } from '../testing/host.ts'

// enforcement.light_class=block is a nudge, never a wall. Pinned above all:
// at most ONE deny per session even as orchestra's retries rewrite the prompt
// every round (a fingerprint-only cap would deny every round), and the
// carve-outs decide before agent-exec is ever asked.

type Stub = { lightClass?: string, executor?: string, model?: string, effort?: string }

const host = (on: On, stub: Stub, extra: Partial<HostOptions> = {}) => fakeHost(on, {
    sessionId: freshSession(),
    agentExec: args => args[0] === 'config'
        ? { exitCode: 0, stdout: JSON.stringify({ enforcement: { light_class: stub.lightClass ?? 'block' } }) }
        : { exitCode: 0, stdout: JSON.stringify({ executor: stub.executor ?? 'pi', model: stub.model ?? '', effort: stub.effort ?? '' }) },
    ...extra,
})

const PI = { executor: 'pi', model: 'openai-codex/gpt-5.6-luna', effort: 'medium' }

const spawn = ($: unknown, args: Record<string, unknown>) =>
    callTool($, undefined, { tool: 'Agent', description: 'implement x', prompt: 'implement the thing', ...args })

const haiku = { model: 'haiku' }
const execs = (runs: string[][]) => runs.filter(argv => argv[3] === 'agent-exec').length

describe('light-class', () => {
    test('denies the first direct Haiku spawn once, then never again this session', async ($, on) => {
        const h = host(on, PI)
        const first = await spawn($, haiku)
        expect(first.deny).toContain('agent-exec dispatch --class light')
        expect(first.deny).toContain('prefers pi (model openai-codex/gpt-5.6-luna, effort medium)')
        const asked = execs(h.runs)
        expect((await spawn($, haiku)).deny, 'identical retry').toBeUndefined()
        expect(execs(h.runs), 'the cached session never asks agent-exec again').toBe(asked)
        const retry = 'implement the thing\n\nThis is retry 2 of 3. Your previous attempt failed verification; apply this feedback exactly: {"missing":["edge case"]}'
        expect((await spawn($, { ...haiku, prompt: retry })).deny, 'a mutated retry').toBeUndefined()
        expect((await spawn($, { ...haiku, prompt: 'an unrelated task' })).deny, 'an unrelated task').toBeUndefined()
    })

    test('carve-outs decide before agent-exec is asked', async ($, on) => {
        const h = host(on, PI)
        const allowed: Record<string, unknown>[] = [
            { ...haiku, prompt: 'run agent-exec dispatch --class light ...' },
            { ...haiku, prompt: 'x [orchestra:allow-claude: needs Read-only tools]' },
            { ...haiku, tools: ['Read'] },
            { ...haiku, disallowedTools: ['Bash'] },
            { ...haiku, subagent_type: 'Explore' },
            { ...haiku, subagent_type: 'my-custom-agent' },
            { ...haiku, subagent_type: 'orchestra:orchestra-review' },
            { model: 'sonnet' },
            { model: 'opus' },
        ]
        for (const args of allowed) {
            expect((await spawn($, args)).deny, JSON.stringify(args)).toBeUndefined()
        }
        expect(execs(h.runs)).toBe(0)
    })

    test('generic and orchestra-light calls stay in scope', async ($, on) => {
        host(on, PI)
        expect((await spawn($, { ...haiku, subagent_type: 'general-purpose' })).deny).toBeDefined()
    })

    test('orchestra-light is deniable whatever its model', async ($, on) => {
        host(on, PI)
        expect((await spawn($, { subagent_type: 'orchestra:orchestra-light' })).deny).toBeDefined()
    })

    test('light_class=off never asks route', async ($, on) => {
        const h = host(on, { lightClass: 'off' })
        expect((await spawn($, haiku)).deny).toBeUndefined()
        expect(h.runs.some(argv => argv.includes('route'))).toBe(false)
    })

    test('nothing better than claude to redirect to', async ($, on) => {
        host(on, { executor: 'claude' })
        expect((await spawn($, haiku)).deny).toBeUndefined()
    })

    test('agent-exec missing fails open', async ($, on) => {
        host(on, PI, { agentExec: () => 'missing' })
        expect((await spawn($, haiku)).deny).toBeUndefined()
    })

    test('kill switch: ORCHESTRA_ENFORCEMENT=off asks nothing', async ($, on) => {
        const h = host(on, PI, { env: { ORCHESTRA_ENFORCEMENT: 'off' } })
        expect((await spawn($, haiku)).deny).toBeUndefined()
        expect(execs(h.runs)).toBe(0)
    })
})
