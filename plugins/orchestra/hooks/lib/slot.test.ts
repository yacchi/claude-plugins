import { describe, expect, test } from 'claude-code/testing'
import type { ProcessRunResult } from 'claude-code'
import { type AgentExecAnswer, callTool, fakeHost, freshSession, startAgent } from '../testing/host.ts'
import type { Io } from './io.ts'
import { withHeavySlot } from './slot.ts'

// agent-exec as the hooks see it: every command is a targeted heavy one, and
// `slot acquire` admits with token t1.
const admitting = (args: readonly string[]): AgentExecAnswer => {
    if (args[0] === 'slot' && args[1] === 'classify') {
        return { exitCode: 0, stdout: '{"op":"pnpm test","heavy":true,"kind":"test","pool":"test","scope":"targeted"}\n' }
    }
    if (args[0] === 'slot' && args[1] === 'acquire') {
        return { exitCode: 0, stdout: '{"admitted":true,"token":"t1","waited_ms":0}\n' }
    }

    return { exitCode: 0, stdout: args[0] === 'config' ? '{"enforcement": {}}' : '' }
}

// The admission calls (acquire, release); the classify before them is the
// full-suite guard's as much as admission's.
const slotCalls = (runs: readonly string[][]) => runs
    .filter(r => r[0] === 'sh' && r[4] === 'slot' && r[5] !== 'classify')
    .map(r => r.slice(5).join(' '))

describe('heavy slot admission', () => {
    test('a worker subagent\'s Bash call is admitted before it runs and released after', async ($, on) => {
        const h = fakeHost(on, { sessionId: freshSession(), agentExec: admitting })
        await startAgent($, 'ag_1', 'orchestra:orchestra-light', '/w')
        const r = await callTool($, 'ag_1', { tool: 'Bash', command: 'cd pkg && pnpm test', description: 'test' })
        expect(r).toEqual({ result: 'ran' })
        expect(slotCalls(h.runs)).toEqual([
            'acquire --command=cd pkg && pnpm test --max-wait 540s',
            'release t1',
        ])
    })

    test('the main thread of a session a wave hosts is admitted', async ($, on) => {
        const sessionId = freshSession()
        const h = fakeHost(on, {
            sessionId,
            agentExec: admitting,
            repos: [{ main: '/repo', linked: [{ path: '/work/task-a', branch: 'orchestra/task-a' }] }],
            files: { '/repo/.git/worktrees/task-a/orchestra-baseline': 'base\n' },
        })
        await $.classic.SessionStart({ session_id: sessionId, source: 'startup', cwd: '/work/task-a' })
        await callTool($, undefined, { tool: 'Bash', command: 'pnpm test', description: 'test' })
        expect(slotCalls(h.runs)).toEqual([expect.stringContaining('acquire'), 'release t1'])
    })

    test('the instructor\'s main thread and background commands never wait', async ($, on) => {
        const h = fakeHost(on, { sessionId: freshSession(), agentExec: admitting })
        await callTool($, undefined, { tool: 'Bash', command: 'pnpm test', description: 'test' })
        await startAgent($, 'ag_2', 'orchestra:orchestra-light', '/w')
        await callTool($, 'ag_2', { tool: 'Bash', command: 'pnpm test', description: 'test', run_in_background: true })
        expect(slotCalls(h.runs)).toEqual([])
    })

    test('a failed acquire runs the command unadmitted', async ($, on) => {
        const h = fakeHost(on, { sessionId: freshSession(), agentExec: args => (args[0] === 'slot' ? 'missing' : admitting(args)) })
        await startAgent($, 'ag_3', 'orchestra:orchestra-light', '/w')
        const r = await callTool($, 'ag_3', { tool: 'Bash', command: 'pnpm test', description: 'test' })
        expect(r).toEqual({ result: 'ran' })
        expect(slotCalls(h.runs)).toEqual([expect.stringContaining('acquire')])
    })
})

describe('withHeavySlot', () => {
    const ioAnswering = (stdout: string, seen: string[]): Io => ({
        run: async (argv: readonly string[]): Promise<ProcessRunResult> => {
            seen.push(argv.slice(4, 6).join(' '))

            return { exitCode: 0, stdout, stderr: '', isStdoutTruncated: false, isStderrTruncated: false }
        },
    }) as unknown as Io

    test('the slot is released when the tool call rejects', async () => {
        const seen: string[] = []
        const io = ioAnswering('{"admitted":true,"token":"t9"}', seen)
        const ran = withHeavySlot(io, 'pnpm test', () => Promise.reject(new Error('aborted')))
        await expect(ran).rejects.toThrow('aborted')
        expect(seen).toEqual(['slot acquire', 'slot release'])
    })

    test('unparseable output runs unadmitted and releases nothing', async () => {
        const seen: string[] = []
        expect(await withHeavySlot(ioAnswering('not json', seen), 'pnpm test', async () => 'ok')).toBe('ok')
        expect(seen).toEqual(['slot acquire'])
    })
})
