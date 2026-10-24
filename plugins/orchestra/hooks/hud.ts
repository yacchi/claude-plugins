import type { EngineInterface, Register, Timer } from 'claude-code'
import { agentExec } from './lib/config.ts'
import type { Io } from './lib/io.ts'

// Live orchestra visibility: a status-line entry while orchestra agents run
// or a wave the main thread started is in progress, a toast only on notable
// failures and on each new wave need. Never blocks or rewrites anything.

type Tracked = { bucket: string, description: string }

const tracked = new Map<string, Tracked>()
let failures = 0

// A wave the main thread started with `agent-exec wave run --state <path>`,
// keyed by that path. `part` is unset until a status read succeeds.
type Wave = {
    statePath: string,
    startedMs: number,
    timer?: Timer,
    part?: string,
    seen: boolean,
    misses: number,
    busy: boolean,
    toasted: Set<string>,
}

const waves = new Map<string, Wave>()

const POLL_MS = 15_000
const MAX_MISSES = 3
const LOCATE_MS = 2000
const IN_FLIGHT = new Set(['implementing', 'verifying', 'fixing', 'ready'])

const familyOf = (model: string | undefined): string => {
    const m = (model ?? '').toLowerCase()
    for (const family of ['haiku', 'sonnet', 'opus', 'fable']) {
        if (m.includes(family)) {
            return family
        }
    }

    return 'other'
}

// Lenient: a ```json fence first, else the outermost {...}.
const parseVerdict = (answer: string | undefined): Record<string, unknown> | undefined => {
    if (typeof answer !== 'string') {
        return undefined
    }
    const fenced = answer.match(/```(?:json)?\s*([\s\S]*?)```/)
    const candidates = [fenced?.[1], answer.slice(answer.indexOf('{'), answer.lastIndexOf('}') + 1)]
    for (const text of candidates) {
        if (!text) {
            continue
        }
        try {
            const value: unknown = JSON.parse(text)
            if (value !== null && typeof value === 'object' && !Array.isArray(value)) {
                return value as Record<string, unknown>
            }
        } catch {
            // not JSON; try the next candidate
        }
    }

    return undefined
}

const isRecord = (v: unknown): v is Record<string, unknown> =>
    typeof v === 'object' && v !== null && !Array.isArray(v)

// A flag's value in a command line (`--flag X`, `--flag=X`, either quoted).
const flagOf = (args: string, flag: string): string | undefined => {
    const m = args.match(new RegExp(`(?:^|\\s)--${flag}(?:=|\\s+)(?:'([^']*)'|"([^"]*)"|([^\\s;&|)]+))`))

    return m ? (m[1] ?? m[2] ?? m[3]) || undefined : undefined
}

// An `agent-exec wave run` anywhere in a command line: its --state as typed,
// the text after it, and whether a `cd` before it moved the directory a
// relative path is read against. Undefined without one or without --state.
type WaveRun = { state: string, args: string, cd: boolean }

const waveRunOf = (command: string): WaveRun | undefined => {
    const run = command.match(/agent-exec\s+wave\s+run\b/)
    if (!run || run.index === undefined) {
        return undefined
    }
    const args = command.slice(run.index + run[0].length)
    const state = flagOf(args, 'state')

    return state === undefined
        ? undefined
        : { state, args, cd: /(?:^|[\s;&|(])cd\s/.test(command.slice(0, run.index)) }
}

// The flags that tell this call's runner from another one in the process table.
const MATCHED_FLAGS = ['plan', 'run-id', 'into']

// The state path the command names, when it can be told without the shell:
// `~` and `$HOME` expanded, a relative path read against the session cwd.
// Undefined when only the shell knows (another variable, a `cd` first).
const typedStatePath = async ($: EngineInterface, run: WaveRun): Promise<string | undefined> => {
    let state = run.state
    const home = state.match(/^(?:~|\$HOME|\$\{HOME\})(?=\/|$)/)
    if (home) {
        const dir = await $.env.get('HOME')
        if (!dir) {
            return undefined
        }
        state = dir + state.slice(home[0].length)
    }
    if (state.includes('$') || state.startsWith('~')) {
        return undefined
    }
    if (state.startsWith('/')) {
        return state
    }
    if (run.cd) {
        return undefined
    }
    const cwd = await $.session.cwd().catch(() => '')

    return cwd ? `${cwd}/${state}` : undefined
}

// `ps` etime, `[[dd-]hh:]mm:ss`, in seconds.
const elapsedOf = (etime: string): number => {
    const [days, clock] = etime.includes('-') ? etime.split('-') : ['0', etime]

    return Number(days) * 86_400 + (clock ?? '').split(':').reduce((s, n) => s * 60 + Number(n), 0)
}

// A process's working directory (`lsof -Fn` prints it as `n<path>`).
const cwdOf = async ($: EngineInterface, pid: string): Promise<string | undefined> => {
    const r = await $.process.run(['lsof', '-a', '-p', pid, '-d', 'cwd', '-Fn'], { timeoutMs: 5000 })
    const line = r.exitCode === 0 ? r.stdout.split('\n').find(l => l.startsWith('n/')) : undefined

    return line?.slice(1)
}

// The state path of the runner a call started when only the shell could
// expand it: the runner's own argv carries the expanded path. Among running
// `wave run`s not tracked yet, those agreeing on every literal --plan/--run-id/
// --into of the call, the newest. A relative argv path (a `cd` first) is read
// against that process's cwd.
const runnerStatePath = async ($: EngineInterface, run: WaveRun): Promise<string | undefined> => {
    const r = await $.process.run(['ps', '-axwwo', 'pid=,etime=,command='], { timeoutMs: 5000 })
    if (r.exitCode !== 0) {
        return undefined
    }
    const wanted = MATCHED_FLAGS
        .map(flag => [flag, flagOf(run.args, flag)] as const)
        .filter(([, value]) => value !== undefined && !value.includes('$'))
    const literalRelative = run.cd && !/[$~]/.test(run.state)
    const found: { state: string, elapsed: number, pid: number }[] = []
    for (const line of r.stdout.split('\n')) {
        const m = line.match(/^\s*(\d+)\s+(\S+)\s+(.*)$/)
        const theirs = m ? waveRunOf(m[3] ?? '') : undefined
        if (!m || !theirs || !wanted.every(([flag, value]) => flagOf(theirs.args, flag) === value)) {
            continue
        }
        let state: string | undefined = theirs.state
        if (!state.startsWith('/')) {
            const cwd = literalRelative && state === run.state ? await cwdOf($, m[1] ?? '').catch(() => undefined) : undefined
            state = cwd ? `${cwd}/${state}` : undefined
        }
        if (state !== undefined && !waves.has(state)) {
            found.push({ state, elapsed: elapsedOf(m[2] ?? ''), pid: Number(m[1]) })
        }
    }
    found.sort((a, b) => a.elapsed - b.elapsed || b.pid - a.pid)

    return found[0]?.state
}

const agentPart = (): string | undefined => {
    if (tracked.size === 0) {
        return undefined
    }
    const counts = new Map<string, number>()
    for (const { bucket } of tracked.values()) {
        counts.set(bucket, (counts.get(bucket) ?? 0) + 1)
    }
    const parts = [`${tracked.size} running`]
    for (const [bucket, n] of counts) {
        parts.push(`${bucket === 'relay' ? 'ext' : bucket} ${n}`)
    }
    if (failures > 0) {
        parts.push(`✗${failures}`)
    }

    return parts.join(' · ')
}

// The one status line: the agent part and every wave's part side by side,
// cleared only when there is neither.
const render = ($: EngineInterface): void => {
    const parts = [agentPart(), ...[...waves.values()].map(w => w.part)].filter(p => p !== undefined)
    $.ui.status(parts.length > 0 ? `orch ▸ ${parts.join(' │ ')}` : undefined)
}

const escapeRegex = (s: string): string => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')

// state.json records a stop but not a run that ended done, so whether the
// runner still runs is asked of the process table; undefined when that
// cannot be told (no pgrep, a failure).
const runnerAlive = async ($: EngineInterface, statePath: string): Promise<boolean | undefined> => {
    try {
        const pattern = `wave run .*--state[= ]${escapeRegex(statePath)}( |$)`
        const r = await $.process.run(['pgrep', '-f', pattern], { timeoutMs: 5000 })
        if (r.exitCode === 0) {
            return true
        }

        return r.exitCode === 1 ? false : undefined
    } catch {
        return undefined
    }
}

const untrack = ($: EngineInterface, wave: Wave): void => {
    wave.timer?.cancel()
    if (waves.get(wave.statePath) === wave) {
        waves.delete(wave.statePath)
    }
    render($)
}

// One poll: refresh the wave's part, toast each need not toasted before, and
// stop tracking once the run stopped or ended, its state file went away, or
// status failed MAX_MISSES times running.
const poll = async ($: EngineInterface, wave: Wave): Promise<void> => {
    if (wave.busy) {
        return
    }
    wave.busy = true
    try {
        const alive = await runnerAlive($, wave.statePath)
        if (!await $.fs.exists(wave.statePath).catch(() => false)) {
            // A missing state file ends tracking only once the file was seen
            // (the run's state went away), or the runner is gone, or, when
            // pgrep cannot tell, after MAX_MISSES polls in a row: before its
            // first write a live runner may still be setting up.
            if (wave.seen || alive === false || (alive === undefined && ++wave.misses >= MAX_MISSES)) {
                untrack($, wave)
            }

            return
        }
        const io = { run: (argv, init) => $.process.run(argv, init) } as Pick<Io, 'run'> as Io
        const r = await agentExec(io, ['wave', 'status', '--state', wave.statePath, '--json'], 10_000)
        if (r.kind === 'missing') {
            untrack($, wave)

            return
        }
        let st: Record<string, unknown> | undefined
        try {
            const value: unknown = r.kind === 'ok' ? JSON.parse(r.stdout) : undefined
            st = isRecord(value) ? value : undefined
        } catch {
            st = undefined
        }
        if (!st) {
            if (++wave.misses >= MAX_MISSES) {
                untrack($, wave)
            }

            return
        }
        wave.seen = true
        wave.misses = 0

        const packages = isRecord(st.packages) ? Object.values(st.packages).filter(isRecord) : []
        const integrated = packages.filter(p => p.status === 'integrated').length
        const running = packages.filter(p => IN_FLIGHT.has(String(p.status))).length
        const needs = Array.isArray(st.needs) ? st.needs.filter(isRecord) : []
        for (const need of needs) {
            const id = typeof need.id === 'string' ? need.id : ''
            const key = `${id}:${String(need.kind)}`
            if (!wave.toasted.has(key)) {
                wave.toasted.add(key)
                $.ui.toast(`orchestra: wave needs ${String(need.kind)}${id ? ` — ${id}` : ''}`)
            }
        }
        const parts = [`wave ${integrated}/${packages.length}`, `${running} running`]
        if (needs.length > 0) {
            parts.push(`${needs.length} needs`)
        }
        wave.part = parts.join(' · ')

        // A stop recorded before this run started is an earlier run's.
        const stoppedAt = isRecord(st.stopped) && typeof st.stopped.at === 'number' ? st.stopped.at * 1000 : undefined
        if ((stoppedAt !== undefined && stoppedAt >= wave.startedMs) || alive === false) {
            untrack($, wave)
        } else if (waves.get(wave.statePath) === wave) {
            render($)
        }
    } finally {
        wave.busy = false
    }
}

const startTracking = async ($: EngineInterface, statePath: string): Promise<Wave> => {
    const previous = waves.get(statePath)
    previous?.timer?.cancel()
    const wave: Wave = {
        statePath,
        startedMs: await $.clock.now(),
        seen: false,
        misses: 0,
        busy: false,
        toasted: previous?.toasted ?? new Set(),
    }
    waves.set(statePath, wave)
    wave.timer = $.clock.every(POLL_MS, () => {
        void poll($, wave).catch(() => undefined)
    })

    return wave
}

// Tracks the runner a backgrounded call started by its argv, asking again
// LOCATE_MS later up to MAX_MISSES times: the call may return before the
// shell has started it.
const locate = async ($: EngineInterface, run: WaveRun, attempt: number): Promise<void> => {
    const statePath = await runnerStatePath($, run)
    if (statePath !== undefined) {
        await startTracking($, statePath)
    } else if (attempt < MAX_MISSES) {
        $.clock.after(LOCATE_MS, () => {
            void locate($, run, attempt + 1).catch(() => undefined)
        })
    }
}

export const register: Register = (on, options) => {
    if (options.hud === false) {
        return
    }

    on('agent.spawn', async ($, e, next) => {
        const result = await next(e)
        try {
            if (result.agentId && (e.subagentType.startsWith('orchestra:') || e.workflow)) {
                const bucket = e.subagentType === 'orchestra:orchestra-relay'
                    ? 'relay'
                    : familyOf(result.model || e.model)
                tracked.set(result.agentId, { bucket, description: (e.description ?? '').slice(0, 60) })
                render($)
            }
        } catch {
            // the HUD must never break a spawn
        }

        return result
    }).catch(($, e, next) => next(e))

    // A wave run is tracked from the main thread's Bash call that starts it.
    // A backgrounded call returns at once and the polls watch the runner; a
    // call that returns without a background task has run (or failed to
    // start) to its end, so one last read toasts its needs and it is dropped.
    // A state path only the shell could expand is taken from the runner's
    // argv once the backgrounded call returned.
    on('tool.call', { tool: 'Bash' }, async ($, e, next) => {
        let run: WaveRun | undefined
        let wave: Wave | undefined
        try {
            run = e.agentId ? undefined : waveRunOf(String(e.command ?? ''))
            const statePath = run && await typedStatePath($, run)
            if (statePath !== undefined) {
                wave = await startTracking($, statePath)
            }
        } catch {
            // the HUD must never break a tool call
        }
        const result = await next(e)
        try {
            const output: unknown = result.result
            const background = result.deny === undefined
                && ((isRecord(output) && typeof output.backgroundTaskId === 'string') || e.run_in_background === true)
            if (wave && !background) {
                wave.timer?.cancel()
                if (result.deny === undefined) {
                    await poll($, wave)
                }
                untrack($, wave)
            } else if (run && !wave && background) {
                void locate($, run, 1).catch(() => undefined)
            }
        } catch {
            // the HUD must never break a tool call
        }

        return result
    }).catch(($, e, next) => next(e))

    on('turn.complete', async ($, e, next) => {
        try {
            const agent = e.agentId ? tracked.get(e.agentId) : undefined
            if (e.agentId && agent) {
                tracked.delete(e.agentId)
                const verdict = parseVerdict(e.answer)
                const status = verdict?.status
                if (e.reason === 'aborted') {
                    // an interrupted agent's partial answer is not a verdict
                } else if (e.reason === 'error') {
                    $.ui.toast(`orchestra: agent error — ${agent.description}`)
                } else if (verdict?.pass === false) {
                    failures++
                    $.ui.toast(`orchestra: review FAIL — ${agent.description}`)
                } else if (status === 'needs-permission' || status === 'runaway' || status === 'unavailable') {
                    $.ui.toast(`orchestra: ${status} — ${agent.description}`)
                } else if (typeof e.answer === 'string' && e.answer.trim().startsWith('ESCALATE')) {
                    $.ui.toast(`orchestra: ESCALATE — ${agent.description}`)
                }
                if (tracked.size === 0) {
                    failures = 0
                }
                render($)
            }
        } catch {
            // the HUD must never break a turn
        }

        return next(e)
    }).catch(($, e, next) => next(e))
}
