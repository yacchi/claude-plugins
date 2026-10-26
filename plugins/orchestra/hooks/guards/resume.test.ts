import { describe, expect, test } from 'claude-code/testing'
import type { AgentInfo, Io } from '../lib/io.ts'
import { checkResume } from './resume.ts'

// Resuming a worker that already holds a long context re-reads all of it on
// every turn. Pinned: the limit and its off switches, the name and id lookup,
// the calls that must not reach agent-exec, the one-time escape, and every
// failure of the context reading, which must allow the call.

type Options = {
    agents?: AgentInfo[],
    contextTokens?: number | null,
    limit?: number | null,
    contextStdout?: string,
    contextFails?: boolean,
    enforcement?: Readonly<Record<string, unknown>>,
    env?: Readonly<Record<string, string>>,
}

const fakeIo = (options: Options = {}) => {
    const runs: string[][] = []
    const out = (exitCode: number, stdout = '') => ({ exitCode, stdout, stderr: '', isStdoutTruncated: false, isStderrTruncated: false })
    const run = async (argv: readonly string[]) => {
        const args = argv.slice(4)
        runs.push([...args])
        if (args[0] === 'config') {
            return out(0, JSON.stringify({ enforcement: options.enforcement ?? {} }))
        }
        if (args[0] === 'hook') {
            if (options.contextFails) {
                return out(1)
            }

            const limit = options.limit === undefined ? 100000 : options.limit

            return out(0, options.contextStdout ?? JSON.stringify({ context_tokens: options.contextTokens ?? null, model: 'claude-test', limit, transcript: null }))
        }

        return out(127)
    }
    const io = {
        env: async (name: string) => options.env?.[name],
        run,
        agents: async () => options.agents ?? [],
    } as unknown as Io

    return { io, runs }
}

const agent = (id: string, name?: string): AgentInfo => ({ id, type: 'orchestra:orchestra-light', status: 'idle', name })

const hookCalls = (runs: readonly string[][]) => runs.filter(r => r[0] === 'hook')

let sessionCount = 0
const session = () => `resume-session-${++sessionCount}`

describe('resume guard', () => {
    test('denies at or over the default limit, allows just below it', async () => {
        const id = 'ag_limit'
        const big = fakeIo({ agents: [agent(id)], contextTokens: 120000 })
        expect(await checkResume(big.io, { to: id }, session())).toContain('orchestra: this agent already holds ~120k tokens')
        const below = fakeIo({ agents: [agent(id)], contextTokens: 99999 })
        expect(await checkResume(below.io, { to: id }, session())).toBeUndefined()
        const exact = fakeIo({ agents: [agent(id)], contextTokens: 100000 })
        expect(await checkResume(exact.io, { to: id }, session()), 'exactly the limit').toContain('already holds ~100k tokens')
    })

    test('the limit returned by agent-exec is the one compared, not the raw config value', async () => {
        const id = 'ag_configured'
        const { io } = fakeIo({ agents: [agent(id)], contextTokens: 60000, limit: 50000, enforcement: { resume_max_tokens: 90000 } })
        expect(await checkResume(io, { to: id }, session())).toContain('limit 50k')

        const ignored = fakeIo({ agents: [agent(id)], contextTokens: 60000, limit: 100000, enforcement: { resume_max_tokens: 50000 } })
        expect(await checkResume(ignored.io, { to: id }, session())).toBeUndefined()
    })

    test('a name resolves to its agent id, and the [ref] suffix is stripped', async () => {
        const id = 'ag_named'
        const { io, runs } = fakeIo({ agents: [agent(id, 'reviewer-1')], contextTokens: 120000 })
        expect(await checkResume(io, { to: 'reviewer-1 [ref-9]' }, session())).toBeDefined()
        expect(hookCalls(runs)[0]).toEqual(expect.arrayContaining(['--agent', id]))
    })

    test('an unknown name, `main`, and an empty target allow without asking agent-exec for context', async () => {
        const { io, runs } = fakeIo({ agents: [agent('ag_other', 'other')], contextTokens: 500000 })
        expect(await checkResume(io, { to: 'nobody' }, session())).toBeUndefined()
        expect(await checkResume(io, { to: 'main' }, session())).toBeUndefined()
        expect(await checkResume(io, { to: '' }, session())).toBeUndefined()
        expect(await checkResume(io, { message: 'no target' }, session())).toBeUndefined()
        expect(hookCalls(runs)).toHaveLength(0)
    })

    test('null tokens, a failed call and unparseable output all allow', async () => {
        const id = 'ag_unreadable'
        const nullTokens = fakeIo({ agents: [agent(id)], contextTokens: null })
        expect(await checkResume(nullTokens.io, { to: id }, session())).toBeUndefined()
        const failed = fakeIo({ agents: [agent(id)], contextFails: true })
        expect(await checkResume(failed.io, { to: id }, session())).toBeUndefined()
        const garbled = fakeIo({ agents: [agent(id)], contextStdout: 'not json' })
        expect(await checkResume(garbled.io, { to: id }, session())).toBeUndefined()
    })

    test('the escape lets the first resume of an agent through, not a second one', async () => {
        const id = 'ag_escape'
        const { io } = fakeIo({ agents: [agent(id)], contextTokens: 120000 })
        const message = 'continue [orchestra:allow-resume: the history is the review]'
        expect(await checkResume(io, { to: id, message }, session())).toBeUndefined()
        expect(await checkResume(io, { to: id, message }, session())).toContain('already holds')
    })

    test('a null limit (guard off or unknown model) allows, and ORCHESTRA_RESUME_GUARD=off allows', async () => {
        const id = 'ag_switched'
        const huge = { agents: [agent(id)], contextTokens: 500000 }
        expect(await checkResume(fakeIo({ ...huge, limit: null }).io, { to: id }, session())).toBeUndefined()
        expect(await checkResume(fakeIo({ ...huge, env: { ORCHESTRA_RESUME_GUARD: 'off' } }).io, { to: id }, session())).toBeUndefined()
    })

    test('ORCHESTRA_ENFORCEMENT=off allows without reaching agent-exec', async () => {
        const id = 'ag_enforcement_off'
        const { io, runs } = fakeIo({ agents: [agent(id)], contextTokens: 500000, env: { ORCHESTRA_ENFORCEMENT: 'off' } })
        expect(await checkResume(io, { to: id }, session())).toBeUndefined()
        expect(runs).toEqual([])
    })
})
