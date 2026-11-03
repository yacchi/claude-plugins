import { describe, expect, test } from 'claude-code/testing'
import type { ProcessRunResult } from 'claude-code'
import { type AgentExecAnswer, callTool, fakeHost, freshSession, startAgent } from '../testing/host.ts'
import type { Io } from './io.ts'
import { classifyCommand, ensureWatcher, guardResult, withHeavySlot } from './slot.ts'

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
    test('the argv is slot classify with the command alone; a caller\'s directory never reaches agent-exec', async () => {
        const seen: string[][] = []
        const io = {
            run: async (argv: readonly string[]): Promise<ProcessRunResult> => {
                seen.push(argv.slice(4))

                return { exitCode: 0, stdout: '{"op":"jest","heavy":true,"scope":"targeted"}', stderr: '', isStdoutTruncated: false, isStderrTruncated: false }
            },
        } as unknown as Io
        await classifyCommand(io, 'pnpm jest', '/w/task-a')
        expect(seen).toEqual([['slot', 'classify', '--command=pnpm jest']])
    })

    test('an answer with stale cap or guard fields is reduced to op, heavy and scope', async () => {
        const stale = '{"op":"jest","heavy":true,"scope":"targeted","kind":"test","pool":"test","capped_command":"pnpm jest --maxWorkers=2","guard_prefix":"/x/agent-exec proc guard --root $$ --token 0123456789abcdef; ","guard_token":"0123456789abcdef"}'
        const r = await classifyCommand(ioAnswering(stale, []), 'pnpm jest')
        expect(r).toEqual({ op: 'jest', heavy: true, scope: 'targeted' })
        expect(Object.keys(r ?? {}).sort()).toEqual(['heavy', 'op', 'scope'])
    })

    test('an answer missing or mistyping op, heavy or scope, or not an object, gives nothing', async () => {
        const answers = [
            '{"heavy":true,"scope":"targeted"}',
            '{"op":"jest","heavy":"true","scope":"targeted"}',
            '{"op":"jest","heavy":true,"scope":"all"}',
            '{"op":1,"heavy":true,"scope":"full"}',
            'null',
            'not json',
        ]
        for (const stdout of answers) {
            expect(await classifyCommand(ioAnswering(stdout, []), 'pnpm jest'), stdout).toBeUndefined()
        }
    })

    test('a failed or missing agent-exec gives nothing, even with a valid body', async () => {
        const body = '{"op":"jest","heavy":true,"scope":"targeted"}'
        expect(await classifyCommand(ioExiting(1, body, []), 'pnpm jest')).toBeUndefined()
        expect(await classifyCommand(ioExiting(127, body, []), 'pnpm jest')).toBeUndefined()
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
    const killed = `{"killed":true,"mem_bytes":1,"procs":312,"reason":${JSON.stringify(reason)},"root":4242}`

    test('a kill since the command started is reported in full, asked by --since', async () => {
        const seen: string[] = []
        expect(await guardResult(ioExiting(0, killed, seen), 1730000000123)).toBe(text)
        expect(seen).toEqual(['proc guard-result --since=1730000000123'])
    })

    test('a fractional start is floored and a negative one clamped to zero', async () => {
        const seen: string[] = []
        await guardResult(ioExiting(0, '{"killed":false}', seen), 12.9)
        await guardResult(ioExiting(0, '{"killed":false}', seen), -5)
        expect(seen).toEqual(['proc guard-result --since=12', 'proc guard-result --since=0'])
    })

    test('no kill, a record without a reason, or a non-object answers nothing', async () => {
        const answers = ['{"killed":false}', '{"killed":true}', '{"killed":"true","reason":"x"}', 'null', 'not json']
        for (const stdout of answers) {
            expect(await guardResult(ioExiting(0, stdout, []), 1)).toBeUndefined()
        }
    })

    test('agent-exec missing or failed answers nothing and never throws', async () => {
        expect(await guardResult(ioExiting(127, '', []), 1)).toBeUndefined()
        expect(await guardResult(ioExiting(1, '', []), 1)).toBeUndefined()
    })

    test('a run that rejects answers nothing and never throws', async () => {
        const io = { run: async () => { throw new Error('spawn failed') } } as unknown as Io
        expect(await guardResult(io, 1)).toBeUndefined()
    })
})

describe('ensureWatcher', () => {
    test('runs proc watch and resolves nothing, whatever agent-exec answers', async () => {
        const seen: string[] = []
        expect(await ensureWatcher(ioExiting(0, 'started', seen))).toBeUndefined()
        expect(await ensureWatcher(ioExiting(1, '', seen))).toBeUndefined()
        expect(await ensureWatcher(ioExiting(127, '', seen))).toBeUndefined()
        expect(seen).toEqual(['proc watch', 'proc watch', 'proc watch'])
    })

    test('a run that rejects resolves nothing and never throws', async () => {
        const io = { run: async () => { throw new Error('spawn failed') } } as unknown as Io
        expect(await ensureWatcher(io)).toBeUndefined()
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
