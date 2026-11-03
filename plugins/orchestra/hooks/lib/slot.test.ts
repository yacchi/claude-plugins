import { describe, expect, test } from 'claude-code/testing'
import type { ProcessRunResult } from 'claude-code'
import { type AgentExecAnswer, callTool, fakeHost, freshSession, startAgent } from '../testing/host.ts'
import type { Io } from './io.ts'
import { classifyCommand, guardResult, withHeavySlot } from './slot.ts'

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

describe('the fake host\'s record of Bash commands', () => {
    test('the main thread\'s Bash command is recorded from the tool call, not from agent-exec runs', async ($, on) => {
        const h = fakeHost(on, { sessionId: freshSession() })
        expect(await callTool($, undefined, { tool: 'Bash', command: 'echo hi', description: 'x' })).toEqual({ result: 'ran' })
        expect(h.commands).toEqual(['echo hi'])
    })
})

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

const ioAnswering = (stdout: string, seen: string[]): Io => ({
    run: async (argv: readonly string[]): Promise<ProcessRunResult> => {
        seen.push(argv.slice(4, 6).join(' '))

        return { exitCode: 0, stdout, stderr: '', isStdoutTruncated: false, isStderrTruncated: false }
    },
}) as unknown as Io

describe('classifyCommand', () => {
    const classified = (capped: string) => `{"op":"jest","heavy":true,"scope":"targeted","capped_command":${JSON.stringify(capped)}}`

    test('a capped command different from the input is carried', async () => {
        const io = ioAnswering(classified('pnpm jest --maxWorkers=2'), [])
        expect(await classifyCommand(io, 'pnpm jest')).toEqual({ op: 'jest', heavy: true, scope: 'targeted', capped: 'pnpm jest --maxWorkers=2' })
    })

    test('a capped command that is empty or the input itself is no cap', async () => {
        expect(await classifyCommand(ioAnswering(classified(''), []), 'pnpm jest')).toEqual({ op: 'jest', heavy: true, scope: 'targeted' })
        expect(await classifyCommand(ioAnswering(classified('pnpm jest'), []), 'pnpm jest')).toEqual({ op: 'jest', heavy: true, scope: 'targeted' })
    })

    test('the command\'s directory goes to classify as --cwd, and is left out when unknown', async () => {
        const argvs: (readonly string[])[] = []
        const io = {
            run: async (argv: readonly string[]): Promise<ProcessRunResult> => {
                argvs.push(argv)

                return { exitCode: 0, stdout: classified(''), stderr: '', isStdoutTruncated: false, isStderrTruncated: false }
            },
        } as unknown as Io
        await classifyCommand(io, 'pnpm jest', '/w/task-a')
        await classifyCommand(io, 'pnpm jest', '')
        expect(argvs[0]).toContain('--cwd=/w/task-a')
        expect(argvs[1].some(a => a.startsWith('--cwd'))).toBe(false)
    })

    test('the guard prefix and token are carried without a cap, and only with a well-formed token', async () => {
        const prefix = '/x/agent-exec proc guard --root $$ --token 0123456789abcdef </dev/null >/dev/null 2>&1; '
        const guarded = (token: string, guardPrefix = prefix) =>
            `{"op":"jest","heavy":true,"scope":"targeted","guard_prefix":${JSON.stringify(guardPrefix)},"guard_token":${JSON.stringify(token)}}`
        expect(await classifyCommand(ioAnswering(guarded('0123456789abcdef'), []), 'pnpm jest')).toEqual({
            op: 'jest',
            heavy: true,
            scope: 'targeted',
            guard: { prefix, token: '0123456789abcdef' },
        })
        expect(await classifyCommand(ioAnswering(guarded('abc1234'), []), 'pnpm jest')).not.toHaveProperty('guard')
        expect(await classifyCommand(ioAnswering(guarded('0123456789abcde;'), []), 'pnpm jest')).not.toHaveProperty('guard')
        expect(await classifyCommand(ioAnswering(guarded('0123456789abcdef', ''), []), 'pnpm jest')).not.toHaveProperty('guard')
    })
})

const ioExiting = (exitCode: number, stdout: string, seen: string[]): Io => ({
    run: async (argv: readonly string[]): Promise<ProcessRunResult> => {
        seen.push(argv.slice(4).join(' '))

        return { exitCode, stdout, stderr: '', isStdoutTruncated: false, isStderrTruncated: false }
    },
}) as unknown as Io

describe('guardResult', () => {
    const reason = 'process guard: 312 processes under pid 4242 (limit 200)'
    const text = `orchestra: agent-exec's process guard killed this command: ${reason}. Its process tree outgrew this machine (a fork bomb, or too many test workers at once). Run it again with fewer workers (e.g. jest --maxWorkers=2, vitest --maxWorkers=2) through the package's test script, and never start a test runner from node -e.`

    test('a killed command\'s record is asked for by token and reported in full', async () => {
        const seen: string[] = []
        const killed = `{"killed":true,"mem_bytes":1,"procs":312,"reason":${JSON.stringify(reason)},"root":4242}`
        expect(await guardResult(ioExiting(0, killed, seen), '0123456789abcdef')).toBe(text)
        expect(seen).toEqual(['proc guard-result --token=0123456789abcdef'])
    })

    test('no kill, a record without a reason, or a non-object answers nothing', async () => {
        const answers = ['{"killed":false}', '{"killed":true}', '{"killed":"true","reason":"x"}', 'null', 'not json']
        for (const stdout of answers) {
            expect(await guardResult(ioExiting(0, stdout, []), '0123456789abcdef')).toBeUndefined()
        }
    })

    test('agent-exec missing or failed answers nothing and never throws', async () => {
        expect(await guardResult(ioExiting(127, '', []), '0123456789abcdef')).toBeUndefined()
        expect(await guardResult(ioExiting(1, '', []), '0123456789abcdef')).toBeUndefined()
    })
})

describe('withHeavySlot', () => {
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
