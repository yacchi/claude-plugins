import { expect, mock, test } from 'claude-code/testing'
import type { On } from 'claude-code'

// The bottom stands for the engine: it answers spawns with fresh ids and records
// what the HUD pins to the status line and toasts.
const setup = (on: On) => {
    const statuses: (string | undefined)[] = []
    const toasts: string[] = []
    let n = 0
    on('agent.spawn', (_$, e) => ({ model: e.model ?? 'claude-sonnet-5', agentId: `a${++n}` }))
    on('ui.status', (_$, e) => {
        statuses.push(e.text)

        return { value: undefined }
    })
    on('ui.toast', (_$, e) => {
        toasts.push(e.text)

        return { value: undefined }
    })

    on('turn.complete', () => ({ text: '' }))

    return { statuses, toasts }
}


// The kit types `$` strictly; these helpers pass only the fields the HUD reads.
const spawn = ($: any, args: { prompt: string, description: string, subagentType: string, model?: string }) =>
    $.agent.spawn(args)

const finish =($: any, agentId: string | undefined, answer = '', reason = 'answer') =>
    $.turn.complete({ agentId, answer, reason, durationMs: 1, isAborted: false, turnId: 't' })

test('tracks only orchestra agents, clears status and failures when idle', async ($, on) => {
    const { statuses, toasts } = setup(on)
    await spawn($, { prompt: 'x', description: 'plain', subagentType: 'Explore' })
    expect(statuses).toEqual([])

    const a = await spawn($, { prompt: 'x', description: 'impl', subagentType: 'orchestra:orchestra-light', model: 'haiku' })
    const r = await spawn($, { prompt: 'x', description: 'relay', subagentType: 'orchestra:orchestra-relay' })
    expect(statuses.at(-1)).toContain('2 running')
    expect(statuses.at(-1)).toContain('ext 1')

    await finish($, undefined)
    await finish($, 'unknown')
    expect(statuses.at(-1)).toContain('2 running')

    await finish($, a.agentId, '```json\n{"pass": false}\n```')
    expect(toasts.length).toBe(1)
    expect(statuses.at(-1)).toContain('✗1')
    await finish($, r.agentId, 'not json {')
    expect(statuses.at(-1)).toBeUndefined()

    await spawn($, { prompt: 'x', description: 'again', subagentType: 'orchestra:orchestra-light' })
    expect(statuses.at(-1)).not.toContain('✗')
})

test('hud: false emits nothing', { options: { hud: false } }, async ($, on) => {
    const { statuses, toasts } = setup(on)
    const a = await spawn($, { prompt: 'x', description: 'impl', subagentType: 'orchestra:orchestra-light' })
    await finish($, a.agentId, '{"pass": false}')
    expect(statuses).toEqual([])
    expect(toasts).toEqual([])
})

// A wave beneath the HUD: a scripted `wave status --json`, the runner's
// process alive or gone, the process table, the state file there or not.
// Every command run is recorded; a Bash call answers as a backgrounded one
// unless told otherwise.
const waveHost = (on: On) => {
    const host = {
        state: { packages: {}, needs: [], stopped: null } as Record<string, unknown>,
        alive: true,
        exists: true,
        runs: [] as string[][],
        background: true,
        ps: '',
    }
    const done = (exitCode: number, stdout = '') =>
        ({ value: { exitCode, stdout, stderr: '', isStdoutTruncated: false, isStderrTruncated: false } })
    on('process.run', (_$, e) => {
        const argv = [...e.argv]
        host.runs.push(argv)
        if (argv[0] === 'pgrep') {
            return done(host.alive ? 0 : 1)
        }
        if (argv[0] === 'ps') {
            return done(0, host.ps)
        }
        if (argv[0] === 'sh' && argv.slice(4, 6).join(' ') === 'wave status') {
            return done(0, JSON.stringify(host.state))
        }

        return done(127)
    })
    on('fs.exists', () => ({ value: host.exists }))
    on('session.cwd', () => ({ value: '/repo' }))
    on('tool.call', () => ({ result: { stdout: '', stderr: '', interrupted: false, ...(host.background ? { backgroundTaskId: 'b1' } : {}) } }))
    const clock = mock.clock(on, { now: 1_000_000 })
    const statusCalls = () => host.runs.filter(r => r[0] === 'sh' && r.slice(4, 6).join(' ') === 'wave status')

    return { host, clock, statusCalls }
}

const bash = ($: any, command: string, agentId?: string, background = true) =>
    $.tool.call({ tool: 'Bash', command, run_in_background: background, ...(agentId ? { agentId } : {}) })

const pkgs = (statuses: string[]) => Object.fromEntries(statuses.map((status, i) => [`p${i}`, { status }]))

test('wave run: --state forms parsed, a subagent\'s run and a run without --state ignored', async ($, on) => {
    setup(on)
    const { host, clock, statusCalls } = waveHost(on)
    await bash($, 'agent-exec wave run --plan p.json')
    await bash($, 'agent-exec wave run --state /sub/state.json', 'a1')
    await bash($, 'cd /repo && agent-exec wave run --plan p.json --state=/w/one.json')
    await bash($, 'agent-exec wave run --state "/w/two dir/state.json" --plan p.json')
    await bash($, "agent-exec wave run --state 'rel/state.json'")
    await clock.advance(15_000)
    const polled = statusCalls().map(r => r[7]).sort()
    expect(polled).toEqual(['/repo/rel/state.json', '/w/one.json', '/w/two dir/state.json'])

    host.alive = false
    await clock.advance(15_000)
})

test('wave run: one toast per need, combined with the agent part, dropped and unpolled once stopped', async ($, on) => {
    const { statuses, toasts } = setup(on)
    const { host, clock, statusCalls } = waveHost(on)
    const a = await spawn($, { prompt: 'x', description: 'impl', subagentType: 'orchestra:orchestra-light', model: 'haiku' })
    host.state = { packages: pkgs(['integrated', 'implementing', 'pending']), needs: [], stopped: { reason: 'old', at: 1 } }
    await bash($, 'agent-exec wave run --plan p.json --state /w/state.json')
    await clock.advance(15_000)
    expect(statuses.at(-1)).toContain('1 running · haiku 1')
    expect(statuses.at(-1)).toContain('wave 1/3 · 1 running')

    host.state = { ...host.state, needs: [{ id: 'p1', kind: 'escalate' }] }
    await clock.advance(15_000)
    await clock.advance(15_000)
    expect(toasts).toEqual(['orchestra: wave needs escalate — p1'])
    expect(statuses.at(-1)).toContain('1 needs')
    host.state = { ...host.state, needs: [{ id: 'p1', kind: 'escalate' }, { id: null, kind: 'environment' }] }
    await clock.advance(15_000)
    expect(toasts.length).toBe(2)

    await finish($, a.agentId)
    expect(statuses.at(-1)).toContain('wave 1/3')

    host.state = { ...host.state, stopped: { reason: 'stop requested', at: 1_000_100 } }
    await clock.advance(15_000)
    expect(statuses.at(-1)).toBeUndefined()
    const calls = statusCalls().length
    await clock.advance(60_000)
    expect(statusCalls().length).toBe(calls)
})

test('wave run: dropped when the runner ends, and a foreground run once its call returns', async ($, on) => {
    const { statuses } = setup(on)
    const { host, clock, statusCalls } = waveHost(on)
    host.state = { packages: pkgs(['integrated', 'pending']), needs: [], stopped: null }
    await bash($, 'agent-exec wave run --state /w/a.json')
    await clock.advance(15_000)
    expect(statuses.at(-1)).toContain('wave 1/2')
    host.alive = false
    await clock.advance(15_000)
    expect(statuses.at(-1)).toBeUndefined()

    host.alive = true
    host.background = false
    await bash($, 'agent-exec wave run --state /w/b.json', undefined, false)
    expect(statuses.at(-1)).toBeUndefined()
    const calls = statusCalls().length
    await clock.advance(60_000)
    expect(statusCalls().length).toBe(calls)
})

test('wave run: a shell-expanded --state is taken from the runner started, by its literal flags', async ($, on) => {
    setup(on)
    const { host, clock, statusCalls } = waveHost(on)
    host.ps = [
        '  301 01:02:03 /bin/zsh -c cd /repo && agent-exec wave run --state "$SP/wave/state.json" --into prog-a',
        '  302 01:02:03 /cache/agent-exec wave run --state /sp/wave/state.json --into prog-a',
        '  303    00:02 /cache/agent-exec wave run --state /other/state.json --into prog-b',
    ].join('\n')
    await bash($, 'cd /repo && agent-exec wave run --plan "$SP/wave/plan.json" --state "$SP/wave/state.json" --into prog-a')
    await clock.advance(15_000)
    expect([...new Set(statusCalls().map(r => r[7]))]).toEqual(['/sp/wave/state.json'])

    host.alive = false
    await clock.advance(15_000)
})

test('wave run: run_in_background without a backgroundTaskId is still tracked', async ($, on) => {
    const { statuses } = setup(on)
    const { host, clock } = waveHost(on)
    host.background = false
    host.state = { packages: pkgs(['integrated', 'pending']), needs: [], stopped: null }
    await bash($, 'agent-exec wave run --state /w/c.json')
    await clock.advance(15_000)
    expect(statuses.at(-1)).toContain('wave 1/2')

    host.alive = false
    await clock.advance(15_000)
})
