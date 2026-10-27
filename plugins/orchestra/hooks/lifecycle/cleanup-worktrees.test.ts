import { describe, expect, test } from 'claude-code/testing'
import type { On } from 'claude-code'
import { endSession, fakeHost, freshSession, type AgentExecAnswer, type HostOptions } from '../testing/host.ts'

// SessionEnd removes only this session's clean worktrees and surfaces the
// ones holding uncollected work instead of discarding them; every switch and
// every failure leaves teardown alone.

const removed = (entries: unknown[]) => JSON.stringify({ removed: entries })
const done = (task: string) => ({ status: 'removed', task, path: `/tmp/w${task}`, branch: 'b', session: 's1' })

const host = (on: On, answer: (args: readonly string[]) => AgentExecAnswer, extra: Partial<HostOptions> = {}) =>
    fakeHost(on, { sessionId: freshSession(), agentExec: answer, ...extra })

const config = (cleanup?: string) => ({ exitCode: 0, stdout: JSON.stringify({ enforcement: cleanup ? { session_cleanup: cleanup } : {} }) })

const end = ($: any, sessionId: string, cwd = '/w/repo') => endSession($, sessionId, cwd)

const removals = (runs: string[][]) => runs.filter(argv => argv.includes('isolate'))

describe('cleanup-worktrees', () => {
    test('reports what it removed, kept and failed on', async ($, on) => {
        let report = ''
        const h = host(on, args => (args[0] === 'config' ? config() : { exitCode: 0, stdout: report }))

        report = removed([done('1'), done('2'), done('3')])
        await end($, 's1')
        expect(h.logs.at(-1)).toBe('orchestra: removed 3 session worktree(s).')
        expect(removals(h.runs).at(-1)).toEqual(expect.arrayContaining(['--session', 's1', '--repo', '/w/repo']))

        report = removed([done('1'), { status: 'dirty', task: 't2', path: '/tmp/w2', files: ['a.txt'] }])
        await end($, 's1')
        const mixed = h.logs.at(-1) ?? ''
        expect(mixed).toContain('removed 1 session worktree(s)')
        expect(mixed).toContain('kept 1 worktree(s) still holding work:\n  t2: /tmp/w2')
        expect(mixed).toContain('--force')

        report = removed([{ status: 'dirty', task: 't1', path: '/tmp/w1' }])
        await end($, 's1')
        expect(h.logs.at(-1)).not.toContain('removed 0')

        report = removed([done('1'), { status: 'error', task: 't2', reason: 'worktree lock held' }])
        await end($, 's1')
        expect(h.logs.at(-1)).toContain('1 worktree removal(s) failed: worktree lock held')
    })

    test('lists trees kept for child trees, and says so when only those remain', async ($, on) => {
        let report = ''
        const h = host(on, args => (args[0] === 'config' ? config() : { exitCode: 0, stdout: report }))

        report = removed([{ status: 'has-children', task: 'feat', path: '/w/feat' }])
        await end($, 's1')
        expect(h.logs.at(-1)).toContain('kept 1 worktree(s) still holding work:\n  feat: /w/feat (still has child trees)')
        expect(h.logs.at(-1)).not.toContain('land')
    })

    test('points at the land command when an assembly holds work not landed', async ($, on) => {
        let report = ''
        const h = host(on, args => (args[0] === 'config' ? config() : { exitCode: 0, stdout: report }))

        report = removed([{
            status: 'dirty',
            task: 'assembly',
            path: '/w/assembly',
            reason: 'assembly 20261028-abcdef12 holds work not landed on your branch; land it first',
        }])
        await end($, 's1')
        expect(h.logs.at(-1)).toContain('kept 1 worktree(s) still holding work:\n  assembly: /w/assembly\n')
        expect(h.logs.at(-1)).toContain('orchestra: an assembly waits to be landed: from your checkout, run `agent-exec isolate land -m "<message>"`')
    })

    test('stays silent when nothing was done or the output is unusable', async ($, on) => {
        let answer: AgentExecAnswer = { exitCode: 0 }
        const h = host(on, args => (args[0] === 'config' ? config() : answer))
        for (const next of [
            { exitCode: 0, stdout: removed([]) },
            { exitCode: 0, stdout: removed([{ status: 'absent', task: 't1' }]) },
            { exitCode: 1, stdout: 'not json' },
            { exitCode: 0, stdout: 'not json' },
        ]) {
            answer = next
            await end($, 's1')
        }
        expect(h.logs).toEqual([])
    })

    test('never removes when switched off or without a session', async ($, on) => {
        let cleanup = 'off'
        const h = host(on, args => (args[0] === 'config' ? config(cleanup) : { exitCode: 0, stdout: removed([done('1')]) }))
        await end($, 's1')
        cleanup = 'on'
        await end($, '')
        await end($, '!!! ***')
        expect(removals(h.runs)).toEqual([])
        expect(h.logs).toEqual([])
    })

    test('kill switch: ORCHESTRA_SESSION_CLEANUP=off wins over config', async ($, on) => {
        const h = host(on, args => (args[0] === 'config' ? config('on') : { exitCode: 0, stdout: removed([done('1')]) }),
            { env: { ORCHESTRA_SESSION_CLEANUP: 'off' } })
        await end($, 's1')
        expect(removals(h.runs)).toEqual([])
    })

    test('agent-exec missing: silent', async ($, on) => {
        const h = host(on, () => 'missing')
        await end($, 's1')
        expect(removals(h.runs)).toEqual([])
        expect(h.logs).toEqual([])
    })
})
