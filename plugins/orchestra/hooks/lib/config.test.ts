import { expect, test } from 'claude-code/testing'
import type { ProcessRunInit, ProcessRunResult } from 'claude-code'
import { agentExec } from './config.ts'
import type { Io } from './io.ts'

// A hand-built Io: `run` records what it was asked and answers with `result`.
const fakeIo = (
    result: Partial<ProcessRunResult> | Error,
    pluginRoot: () => string = () => '/p/orchestra',
) => {
    const calls: { argv: readonly string[], init?: ProcessRunInit }[] = []
    const io = {
        run: async (argv: readonly string[], init?: ProcessRunInit) => {
            calls.push({ argv, init })
            if (result instanceof Error) {
                throw result
            }

            return { exitCode: 0, stdout: '', stderr: '', isStdoutTruncated: false, isStderrTruncated: false, ...result }
        },
        pluginRoot,
    } as Io

    return { io, calls }
}

test('agentExec runs the bin/agent-exec of the plugin root through the environment', async ($, on) => {
    const { io, calls } = fakeIo({ exitCode: 0, stdout: '{}' })

    await agentExec(io, ['config', '--json'], 5000)

    expect(calls[0].argv[0]).toEqual('sh')
    expect(calls[0].argv[3]).toEqual('agent-exec')
    expect(calls[0].argv.slice(4)).toEqual(['config', '--json'])
    expect(calls[0].init?.env).toEqual({ ORCHESTRA_AGENT_EXEC: '/p/orchestra/bin/agent-exec' })
    expect(calls[0].init?.timeoutMs).toEqual(5000)
})

test('agentExec probe never looks agent-exec up on PATH', async ($, on) => {
    const { io, calls } = fakeIo({ exitCode: 0 })

    await agentExec(io, ['config'], 1000)

    expect(/command\s+-v/.test(calls[0].argv[2])).toEqual(false)
})

test('agentExec with an unknown plugin root passes an empty launcher and still runs', async ($, on) => {
    const { io, calls } = fakeIo({ exitCode: 127 }, () => {
        throw new Error('no plugin root')
    })

    const r = await agentExec(io, ['config'], 1000)

    expect(calls.length).toEqual(1)
    expect(calls[0].init?.env).toEqual({ ORCHESTRA_AGENT_EXEC: '' })
    expect(r).toEqual({ kind: 'missing' })
})

test('agentExec maps exit 0 to ok with stdout', async ($, on) => {
    const { io } = fakeIo({ exitCode: 0, stdout: 'hello' })

    expect(await agentExec(io, ['x'], 1000)).toEqual({ kind: 'ok', stdout: 'hello' })
})

test('agentExec maps a nonzero exit to failed', async ($, on) => {
    const { io } = fakeIo({ exitCode: 1, stdout: 'partial' })

    expect(await agentExec(io, ['x'], 1000)).toEqual({ kind: 'failed', stdout: 'partial' })
})

test('agentExec maps a rejected run to failed', async ($, on) => {
    const { io } = fakeIo(new Error('timed out'))

    expect(await agentExec(io, ['x'], 1000)).toEqual({ kind: 'failed' })
})
