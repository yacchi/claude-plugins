import { expect, test } from 'claude-code/testing'
import type { ProcessRunResult } from 'claude-code'
import type { Io } from './io.ts'
import { writeHandoff } from './handoff.ts'

// A hand-built Io: `run` records every argv it is asked to run; `agentType`
// answers with `type`, or rejects when `type` is an Error.
const fakeIo = (type: string | undefined | Error) => {
    const runs: { argv: readonly string[], timeoutMs?: number }[] = []
    const asked: string[] = []
    const io = {
        run: async (argv: readonly string[], init?: { timeoutMs?: number }): Promise<ProcessRunResult> => {
            runs.push({ argv, timeoutMs: init?.timeoutMs })

            return { exitCode: 0, stdout: '{"path":"/x"}', stderr: '', isStdoutTruncated: false, isStderrTruncated: false }
        },
        agentType: async (agentId: string) => {
            asked.push(agentId)
            if (type instanceof Error) {
                throw type
            }

            return type
        },
        pluginRoot: () => '/p',
    } as unknown as Io

    return { io, runs, asked }
}

test('writeHandoff runs hook handoff once for an orchestra agent, answer kept whole', async () => {
    const { io, runs } = fakeIo('orchestra:orchestra-light')
    await writeHandoff(io, 's1', 'ag_1', 'a\nb')
    expect(runs.length).toBe(1)
    expect(runs[0].argv.slice(4)).toEqual(['hook', 'handoff', '--session', 's1', '--agent', 'ag_1', '--type', 'orchestra:orchestra-light', '--answer=a\nb'])
    expect(runs[0].timeoutMs).toBe(10000)
})

test('writeHandoff cuts the answer to 16000 characters', async () => {
    const { io, runs } = fakeIo('orchestra:orchestra-light')
    await writeHandoff(io, 's1', 'ag_1', 'x'.repeat(20000))
    expect(runs[0].argv.at(-1)).toBe(`--answer=${'x'.repeat(16000)}`)
})

test('writeHandoff runs nothing for a non-orchestra or unknown agent type', async () => {
    for (const type of ['general-purpose', undefined, 'orchestrator:x']) {
        const { io, runs } = fakeIo(type)
        await writeHandoff(io, 's1', 'ag_1', 'a')
        expect(runs).toEqual([])
    }
})

test('writeHandoff runs nothing and asks nothing for an empty session or agent id', async () => {
    const noSession = fakeIo('orchestra:orchestra-light')
    await writeHandoff(noSession.io, '', 'ag_1', 'a')
    expect(noSession.runs).toEqual([])
    expect(noSession.asked).toEqual([])

    const noAgent = fakeIo('orchestra:orchestra-light')
    await writeHandoff(noAgent.io, 's1', '', 'a')
    expect(noAgent.runs).toEqual([])
    expect(noAgent.asked).toEqual([])
})

test('writeHandoff resolves when agentType rejects', async () => {
    const { io, runs } = fakeIo(new Error('boom'))
    await expect(writeHandoff(io, 's1', 'ag_1', 'a')).resolves.toBeUndefined()
    expect(runs).toEqual([])
})

test('writeHandoff resolves when the run rejects', async () => {
    const io = {
        run: async () => {
            throw new Error('boom')
        },
        agentType: async () => 'orchestra:orchestra-light',
        pluginRoot: () => '/p',
    } as unknown as Io
    await expect(writeHandoff(io, 's1', 'ag_1', 'a')).resolves.toBeUndefined()
})
