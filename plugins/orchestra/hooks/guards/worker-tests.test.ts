import { describe, expect, test } from 'claude-code/testing'
import type { On } from 'claude-code'
import { type AgentExecAnswer, callTool, fakeHost, freshSession, type HostOptions, startAgent } from '../testing/host.ts'

// Workers ran the whole suite for every small task and it dominated the
// wall clock. Pinned: a worker's full run is denied, the instructor's main
// thread never is, and both escape hatches work. What counts as full is
// agent-exec's call (`slot classify`), scripted here.

const FULL = 'go test ./...'

const classifying = (args: readonly string[]): AgentExecAnswer => {
    if (args[0] === 'slot' && args[1] === 'classify') {
        const full = args[2] === `--command=${FULL}`

        return { exitCode: 0, stdout: `{"op":"go test","heavy":true,"kind":"test","pool":"test","scope":"${full ? 'full' : 'targeted'}"}\n` }
    }

    return { exitCode: 0, stdout: args[0] === 'config' ? '{"enforcement": {}}' : '' }
}

const host = (on: On, extra: Partial<HostOptions> = {}) => fakeHost(on, {
    sessionId: freshSession(),
    agentExec: classifying,
    ...extra,
})

const bash = async ($: unknown, command: string, description = 'run tests') => {
    await startAgent($, 'ag_1', 'orchestra:orchestra-light', '/w')

    return callTool($, 'ag_1', { tool: 'Bash', command, description })
}

describe('worker-tests', () => {
    test('a worker\'s full suite is denied, its targeted tests run', async ($, on) => {
        host(on)
        const r = await bash($, FULL)
        expect(r.deny).toContain('the whole test suite')
        expect(r.deny).toContain('agent-exec check --task')
        expect(await bash($, 'go test ./internal/x/...')).toEqual({ result: 'ran' })
    })

    test('the instructor\'s main thread is never denied', async ($, on) => {
        const h = host(on)
        expect(await callTool($, undefined, { tool: 'Bash', command: FULL, description: 'full suite' })).toEqual({ result: 'ran' })
        expect(h.runs.some(r => r[4] === 'slot')).toBe(false)
    })

    test('the main thread of a session a wave hosts is a worker', async ($, on) => {
        const sessionId = freshSession()
        host(on, {
            sessionId,
            repos: [{ main: '/repo', linked: [{ path: '/work/task-a', branch: 'orchestra/task-a' }] }],
            files: { '/repo/.git/worktrees/task-a/orchestra-baseline': 'base\n' },
        })
        await $.classic.SessionStart({ session_id: sessionId, source: 'startup', cwd: '/work/task-a' })
        expect((await callTool($, undefined, { tool: 'Bash', command: FULL, description: 'x' })).deny).toBeDefined()
    })

    test('escape marker in the description', async ($, on) => {
        host(on)
        expect((await bash($, FULL, 'post-integration run [orchestra:allow-full-suite: integrated tree]')).deny).toBeUndefined()
    })

    test('kill switch: ORCHESTRA_TEST_GUARD=off', async ($, on) => {
        host(on, { env: { ORCHESTRA_TEST_GUARD: 'off' } })
        expect((await bash($, FULL)).deny).toBeUndefined()
    })

    test('fails open when agent-exec is not installed', async ($, on) => {
        host(on, { agentExec: () => 'missing' })
        expect((await bash($, FULL)).deny).toBeUndefined()
    })
})
