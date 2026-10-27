import { expect, mock, test } from 'claude-code/testing'
import type { On } from 'claude-code'

// The bottom stands for the engine: it answers spawns with fresh ids, records
// what the HUD toasts, and draws an empty box where the HUD draws nothing.
const setup = (on: On) => {
    const toasts: string[] = []
    let n = 0
    on('agent.spawn', (_$, e) => ({ model: e.model ?? 'claude-sonnet-5', agentId: `a${++n}` }))
    on('ui.toast', (_$, e) => {
        toasts.push(e.text)

        return { value: undefined }
    })

    on('turn.complete', () => ({ text: '' }))
    on('ui.render', ($, e) => {
        const { Box } = $.ui.resolve(e)

        return h(Box, {}) as any
    })

    return { toasts }
}


// The kit types `$` strictly; these helpers pass only the fields the HUD reads.
const spawn = ($: any, args: { prompt: string, description: string, subagentType: string, model?: string }) =>
    $.agent.spawn(args)

const finish =($: any, agentId: string | undefined, answer = '', reason = 'answer') =>
    $.turn.complete({ agentId, answer, reason, durationMs: 1, isAborted: false, turnId: 't' })

test('tracks only orchestra agents, clears the band and failures when idle', async ($, on) => {
    const { toasts } = setup(on)
    await spawn($, { prompt: 'x', description: 'plain', subagentType: 'Explore' })
    expect(await bandRow($)).toBeUndefined()

    const a = await spawn($, { prompt: 'x', description: 'impl', subagentType: 'orchestra:orchestra-light', model: 'haiku' })
    const r = await spawn($, { prompt: 'x', description: 'relay', subagentType: 'orchestra:orchestra-relay' })
    expect(await bandRow($)).toContain('2 running')
    expect(await bandRow($)).toContain('ext 1')

    await finish($, undefined)
    await finish($, 'unknown')
    expect(await bandRow($)).toContain('2 running')

    await finish($, a.agentId, '```json\n{"pass": false}\n```')
    expect(toasts.length).toBe(1)
    expect(await bandRow($)).toContain('✗1')
    await finish($, r.agentId, 'not json {')
    expect(await bandRow($)).toBeUndefined()

    await spawn($, { prompt: 'x', description: 'again', subagentType: 'orchestra:orchestra-light' })
    expect(await bandRow($)).not.toContain('✗')
})

test('hud: false emits nothing', { options: { hud: false } }, async ($, on) => {
    const { toasts } = setup(on)
    const a = await spawn($, { prompt: 'x', description: 'impl', subagentType: 'orchestra:orchestra-light' })
    await finish($, a.agentId, '{"pass": false}')
    expect(await bandRow($)).toBeUndefined()
    expect(toasts).toEqual([])
})

// A wave beneath the HUD: a scripted `wave status --json`, the runner's
// process alive or gone, the process table, the state file there or not.
// Every command run is recorded; a Bash call answers as a backgrounded one
// unless told otherwise.
const waveHost = (on: On) => {
    const host = {
        state: { progress: prog([]), stopped: null } as Record<string, unknown>,
        alive: true,
        exists: true,
        runs: [] as string[][],
        background: true,
        ps: '',
        stdout: '',
        panes: [] as { id: string, title: string, isShown: boolean, isFocused: boolean, isPlaced: boolean }[],
        opened: [] as string[],
        places: true,
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
    on('tool.call', () => ({ result: { stdout: host.stdout, stderr: '', interrupted: false, ...(host.background ? { backgroundTaskId: 'b1' } : {}) } }))
    on('ui.panes', () => ({ value: host.panes }))
    on('ui.open', (_$, e) => {
        host.opened.push(e.id)
        host.panes = [{ id: e.id, title: e.title ?? e.id, isShown: true, isFocused: false, isPlaced: host.places }]

        return { value: host.places ? { isPlaced: true } : { isPlaced: false, reason: 'narrow' } }
    })
    on('ui.close', (_$, e) => {
        host.panes = host.panes.filter(p => p.id !== e.id)

        return { value: undefined }
    })
    const clock = mock.clock(on, { now: 1_000_000 })
    const statusCalls = () => host.runs.filter(r => r[0] === 'sh' && r.slice(4, 6).join(' ') === 'wave status')

    return { host, clock, statusCalls }
}

const bash = ($: any, command: string, agentId?: string, background = true) =>
    $.tool.call({ tool: 'Bash', command, run_in_background: background, ...(agentId ? { agentId } : {}) })

// A `progress` object: one package per status, the in-flight ones running.
const prog = (statuses: string[], extra: Record<string, unknown> = {}) => ({
    run_id: 'r1',
    total: statuses.length,
    integrated: statuses.filter(s => s === 'integrated').length,
    pending: statuses.filter(s => s === 'pending').length,
    blocked: 0,
    running: statuses.flatMap((s, i) => (s === 'implementing'
        ? [{ id: `p${i}`, cls: 'light', executor: 'herdr', model: 'haiku', phase: s, started_at: 1, elapsed_s: 360, est_seconds: 240 }]
        : [])),
    needs: [],
    next: [],
    recent: [],
    elapsed_s: 600,
    eta_s: null,
    eta_basis: 'none',
    cost_usd: null,
    ...extra,
})

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
    const { toasts } = setup(on)
    const { host, clock, statusCalls } = waveHost(on)
    const a = await spawn($, { prompt: 'x', description: 'impl', subagentType: 'orchestra:orchestra-light', model: 'haiku' })
    host.state = { progress: prog(['integrated', 'implementing', 'pending']), stopped: { reason: 'old', at: 1 } }
    await bash($, 'agent-exec wave run --plan p.json --state /w/state.json')
    await clock.advance(15_000)
    expect(await bandRow($)).toContain('1 running · haiku 1')
    expect(await bandRow($)).toContain('wave 1/3')

    const needs = (list: unknown[]) => ({ ...host.state, progress: prog(['integrated', 'implementing', 'pending'], { needs: list }) })
    host.state = needs([{ id: 'p1', kind: 'escalate', detail: 'x' }])
    await clock.advance(15_000)
    await clock.advance(15_000)
    expect(toasts).toEqual(['orchestra: wave needs escalate — p1'])
    host.state = needs([{ id: 'p1', kind: 'escalate', detail: 'x' }, { id: null, kind: 'environment', detail: 'y' }])
    await clock.advance(15_000)
    expect(toasts.length).toBe(2)

    await finish($, a.agentId)
    expect(await bandRow($)).toContain('wave 1/3')

    host.state = { ...host.state, stopped: { reason: 'stop requested', at: 1_000_100 } }
    await clock.advance(15_000)
    expect(await bandRow($)).toBeUndefined()
    const calls = statusCalls().length
    await clock.advance(60_000)
    expect(statusCalls().length).toBe(calls)
})

test('wave run: dropped when the runner ends, and a foreground run once its call returns', async ($, on) => {
    setup(on)
    const { host, clock, statusCalls } = waveHost(on)
    host.state = { progress: prog(['integrated', 'pending']), stopped: null }
    await bash($, 'agent-exec wave run --state /w/a.json')
    await clock.advance(15_000)
    expect(await bandRow($)).toContain('wave 1/2')
    host.alive = false
    await clock.advance(15_000)
    expect(await bandRow($)).toBeUndefined()

    host.alive = true
    host.background = false
    await bash($, 'agent-exec wave run --state /w/b.json', undefined, false)
    expect(await bandRow($)).toBeUndefined()
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
    setup(on)
    const { host, clock } = waveHost(on)
    host.background = false
    host.state = { progress: prog(['integrated', 'pending']), stopped: null }
    await bash($, 'agent-exec wave run --state /w/c.json')
    await clock.advance(15_000)
    expect(await bandRow($)).toContain('wave 1/2')

    host.alive = false
    await clock.advance(15_000)
})

test('wave run: a watcher subagent\'s detached run and wait are tracked, another subagent\'s run is not', async ($, on) => {
    setup(on)
    const { host, clock, statusCalls } = waveHost(on)
    host.background = false
    host.state = { progress: prog(['integrated', 'implementing']), stopped: null }
    const light = await spawn($, { prompt: 'x', description: 'impl', subagentType: 'orchestra:orchestra-light' })
    const watcher = await spawn($, { prompt: 'x', description: 'wave', subagentType: 'orchestra:orchestra-wave' })
    await bash($, 'agent-exec wave run --state /w/other.json --detach', light.agentId, false)
    host.stdout = '{"status":"detached","pid":7,"state":"/sp/w/state.json","log":"/sp/w/runner.log"}'
    await bash($, 'agent-exec wave run --plan "$SP/plan.json" --state "$SP/w/state.json" --detach', watcher.agentId, false)
    host.stdout = '{"status":"running","seq":3,"line":"…","progress":{}}'
    await bash($, 'agent-exec wave wait --state /w/watched.json --since 0', watcher.agentId, false)
    await clock.advance(15_000)
    expect([...new Set(statusCalls().map(r => r[7]))].sort()).toEqual(['/sp/w/state.json', '/w/watched.json'])
    expect(await bandRow($)).toContain('wave 1/2')

    host.stdout = '{"status":"done","seq":9,"line":"done","progress":{}}'
    await bash($, 'agent-exec wave wait --state /w/watched.json --since 3', watcher.agentId, false)
    const calls = statusCalls().length
    await clock.advance(15_000)
    expect(statusCalls().slice(calls).map(r => r[7])).toEqual(['/sp/w/state.json'])

    host.alive = false
    await clock.advance(15_000)
})

const BAND = {
    plugin: 'orchestra',
    component: 'AbovePrompt',
    props: { hasSurvey: false, isWorking: false, maxRows: 8, bodyColumns: 60, scroll: { offset: 0, bodyRows: 7 }, view: {} },
} as const

const PANE = {
    plugin: 'orchestra',
    component: 'Pane',
    requestId: 'orchestra',
    props: { title: 'orchestra', isFocused: false, bodyColumns: 40, placement: 'dock', scroll: { offset: 0, bodyRows: 12 }, view: {} },
} as const

const NARROW = { columns: 80, rows: 24, isFullscreen: false }

// `/orchestra:progress`, typed by the person.
const toggle = ($: any) => $.command.run({ command: 'progress' })

const texts = async (ui: { findAll: (q: { type: string }) => Promise<{ text: string }[]> }) =>
    (await ui.findAll({ type: 'Text' })).map(t => t.text)

const hasBand = async (ui: { findAll: (q: { type: string }) => Promise<{ text: string }[]> }) =>
    (await texts(ui)).some(t => t.startsWith('orch ▸'))

// The band's summary row on a terminal, as drawn now; undefined when no band is drawn.
const bandRow = async ($: any): Promise<string | undefined> => {
    const ui = await $.ui.mount({ ...BAND, surface: 'terminal', viewport: NARROW })
    const row = (await texts(ui)).find(t => t.startsWith('orch ▸'))
    await ui.unmount()

    return row
}

test('band: shown while a wave is tracked and no pane is up, gone once tracking stops', async ($, on) => {
    setup(on)
    const { host, clock } = waveHost(on)
    host.state = { progress: prog(['integrated', 'implementing', 'pending'], { next: ['p2'] }), stopped: null }
    await bash($, 'agent-exec wave run --state /w/band.json')
    await clock.advance(15_000)
    for (const surface of ['terminal', 'desktop'] as const) {
        const ui = await $.ui.mount({ ...BAND, surface, viewport: NARROW })
        expect(await texts(ui)).toEqual(['orch ▸ r1 · wave 1/3 · 1 running'])
        expect((await ui.findAll({ type: 'Button' })).map(b => [b.key, b.text])).toEqual([['orchestra-band-toggle', '詳細']])
        await ui.unmount()
    }
    expect(host.opened).toEqual([])

    // The person opens the pane: the band yields to it; closed, it is back.
    await toggle($)
    expect(host.opened).toEqual(['orchestra'])
    let ui = await $.ui.mount({ ...BAND, surface: 'terminal', viewport: NARROW })
    expect(await hasBand(ui)).toBe(false)
    await ui.unmount()
    await toggle($)
    expect(host.panes).toEqual([])

    ui = await $.ui.mount({ ...BAND, surface: 'terminal', viewport: NARROW })
    expect(await hasBand(ui)).toBe(true)
    host.alive = false
    await clock.advance(15_000)
    expect(await hasBand(ui)).toBe(false)
    await ui.unmount()
})

test('band: its button expands the running rows, needs and next, and collapses again', async ($, on) => {
    setup(on)
    const { host, clock } = waveHost(on)
    host.state = {
        progress: prog(['integrated', 'implementing', 'pending'], { needs: [{ id: 'p2', kind: 'escalate', detail: 'x' }], next: ['p2'] }),
        stopped: null,
    }
    await bash($, 'agent-exec wave run --state /w/open.json')
    await clock.advance(15_000)
    const ui = await $.ui.mount({ ...BAND, surface: 'terminal', viewport: NARROW })
    const summary = 'orch ▸ r1 · wave 1/3 · 1 running · 1 needs'
    expect(await texts(ui)).toEqual([summary])

    await ui.press({ key: 'orchestra-band-toggle' })
    const open = await texts(ui)
    expect(open[0]).toBe(summary)
    expect(open).toContain('p1 haiku 実装 6m')
    expect(open.some(t => t.startsWith('⚠ '))).toBe(true)
    expect(open).toContain('⏭ next: p2')
    expect((await ui.findAll({ type: 'Button' })).map(b => [b.key, b.text])).toEqual([['orchestra-band-toggle', '畳む']])

    await ui.press({ key: 'orchestra-band-toggle' })
    expect(await texts(ui)).toEqual([summary])
    await ui.unmount()

    host.alive = false
    await clock.advance(15_000)
})

test('band: the run line and the pane header name the run\'s account', async ($, on) => {
    setup(on)
    const { host, clock } = waveHost(on)
    host.state = { progress: prog(['integrated', 'implementing', 'pending'], { account: 'Acme' }), stopped: null }
    await bash($, 'agent-exec wave run --state /w/acct.json')
    await clock.advance(15_000)
    expect(await bandRow($)).toContain('r1 (Acme) · wave')

    const pane = await $.ui.mount({ ...PANE, surface: 'terminal' })
    expect((await texts(pane))[0]).toMatch(/^r1 \(Acme\)  /)
    await pane.unmount()

    host.alive = false
    await clock.advance(15_000)
})

test('pane: never opened unasked, even where it would dock; its own button closes it on every surface', async ($, on) => {
    setup(on)
    const { host, clock } = waveHost(on)
    host.state = { progress: prog(['implementing']), stopped: null }
    await bash($, 'agent-exec wave run --state /w/pane.json')
    await clock.advance(15_000)
    const docked = await $.ui.mount({ ...BAND, surface: 'terminal', viewport: { columns: 200, rows: 50, isFullscreen: true } })
    expect(await hasBand(docked)).toBe(true)
    await docked.unmount()
    expect(host.opened).toEqual([])

    for (const surface of ['terminal', 'desktop', 'mobile'] as const) {
        await toggle($)
        expect(host.panes.map(p => p.id)).toEqual(['orchestra'])
        const ui = await $.ui.mount({ ...PANE, surface })
        await ui.press({ key: 'orchestra-close' })
        expect(host.panes, surface).toEqual([])
        await ui.unmount()
    }
})

test('pane: running rows fit bodyColumns, the rest as … +k', async ($, on) => {
    setup(on)
    const { host, clock } = waveHost(on)
    const running = Array.from({ length: 12 }, (_, i) => ({
        id: `package-with-a-long-name-${i}`, cls: 'standard', executor: 'herdr', model: 'claude-sonnet-5',
        phase: 'verifying', started_at: 1, elapsed_s: 7_800, est_seconds: 600,
    }))
    host.state = {
        progress: prog([], { total: 40, integrated: 12, running, eta_s: 2_100, cost_usd: 4.1, next: ['a', 'b'], recent: [{ id: 'x', at: 1 }] }),
        stopped: null,
    }
    await bash($, 'agent-exec wave run --state /w/rows.json')
    await clock.advance(15_000)
    // A wide letter takes two cells.
    const cells = (s: string) => [...s].reduce((n, ch) => n + (/[　-鿿]/.test(ch) ? 2 : 1), 0)

    const pane = await $.ui.mount({ ...PANE, surface: 'terminal' })
    const lines = await texts(pane)
    expect(lines[0]).toBe('r1  12/40  経過 10m  残り ~35m  ~$4.10')
    const shown = lines.filter(l => l.startsWith('  package-')).length
    expect(shown).toBeLessThan(running.length)
    expect(lines).toContain(`  … +${running.length - shown}`)
    expect(lines.length).toBeLessThanOrEqual(PANE.props.scroll.bodyRows)
    expect(lines.every(l => cells(l) <= PANE.props.bodyColumns)).toBe(true)
    await pane.unmount()

    const band = await $.ui.mount({ ...BAND, surface: 'terminal', viewport: NARROW })
    await band.press({ key: 'orchestra-band-toggle' })
    const row = (await texts(band))[1] ?? ''
    expect(row).toMatch(/ · \+\d+$/)
    expect(cells(row)).toBeLessThanOrEqual(BAND.props.bodyColumns)
    await band.unmount()

    host.alive = false
    await clock.advance(15_000)
})
