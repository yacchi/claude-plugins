import { basename, words } from '../lib/shell.ts'

// Claude Code moves a foreground Bash call to the background after 180 s. The
// watcher's wave wait used to hop for 560 s, so its answer never came back as
// the tool result and it then read the output file. Each hop now waits at most
// 150 s, and the watcher is held to its two commands: a call it may not make is
// denied with the command it should run next, and an answer that is not final
// is said so on the tool result itself.

export const WAVE_WATCHER = 'orchestra:orchestra-wave'

// The state path and seq the watcher last saw, per agent. It is only read to
// name the next command in a deny or a context text.
const tracked = new Map<string, { state?: string, seq?: number }>()

type Parsed = { verb: 'run' } | { verb: 'wait', state: string } | undefined

const WAIT_FLAGS = ['--state', '--max-wait', '--since']

const isObject = (v: unknown): v is Record<string, unknown> =>
    typeof v === 'object' && v !== null && !Array.isArray(v)

// Each wait flag once, as `--flag <v>` or `--flag=<v>`. A value may not start
// with `-`, so a missing value does not swallow the next flag.
const waitFlags = (rest: string[]): Map<string, string> | undefined => {
    const out = new Map<string, string>()
    for (let i = 0; i < rest.length; i++) {
        const w = rest[i]
        const flag = WAIT_FLAGS.find(f => w === f || w.startsWith(`${f}=`))
        if (flag === undefined || out.has(flag)) {
            return undefined
        }
        let value: string
        if (w === flag) {
            const next = rest[i + 1]
            if (next === undefined || next.startsWith('-')) {
                return undefined
            }
            value = next
            i++
        } else {
            value = w.slice(flag.length + 1)
            if (value === '') {
                return undefined
            }
        }
        out.set(flag, value)
    }
    return out
}

const parse = (command: string): Parsed => {
    const c = command.trim()
    if (/[;&|<>`\n\r]|\$\(/.test(c)) {
        return undefined
    }
    const ws = words(c)
    if (basename(ws[0] ?? '') !== 'agent-exec' || ws[1] !== 'wave') {
        return undefined
    }
    if (ws[2] === 'run') {
        // `--detach` must be last and cannot stand alone: a plan path comes before it.
        return ws.length >= 5 && ws[ws.length - 1] === '--detach' ? { verb: 'run' } : undefined
    }
    if (ws[2] !== 'wait') {
        return undefined
    }
    const flags = waitFlags(ws.slice(3))
    const state = flags?.get('--state')
    const maxWait = flags?.get('--max-wait')
    if (state === undefined || maxWait === undefined || !/^[0-9]+$/.test(maxWait)) {
        return undefined
    }
    // Above 170 s the hop would be moved to the background, as the 560 s one was.
    const n = Number(maxWait)
    return n >= 1 && n <= 170 ? { verb: 'wait', state } : undefined
}

const next = (agentId: string): string => {
    const entry = tracked.get(agentId)
    if (entry?.state === undefined) {
        return 'the wave run command you were given, with --detach appended'
    }
    const since = entry.seq === undefined ? '' : ` --since ${String(entry.seq)}`
    return `\`agent-exec wave wait --state ${entry.state} --max-wait 150${since}\``
}

const lastJson = (stdout: string): Record<string, unknown> | undefined => {
    const lines = stdout.split('\n').filter(l => l.trim() !== '')
    const last = lines[lines.length - 1]
    if (last === undefined) {
        return undefined
    }
    try {
        const v: unknown = JSON.parse(last)
        return isObject(v) ? v : undefined
    } catch {
        return undefined
    }
}

// Deny text for a Bash call of the watcher agent agentId, or undefined to allow.
export const checkWaveWatcher = (agentId: string, input: Readonly<Record<string, unknown>>): string | undefined => {
    try {
        if (input.run_in_background === true) {
            return `orchestra: the wave watcher runs every command in the foreground (timeout 600000), never with run_in_background. Next: ${next(agentId)}.`
        }
        if (parse(String(input.command ?? '')) === undefined) {
            return `orchestra: the wave watcher runs only \`agent-exec wave run … --detach\` and \`agent-exec wave wait --state <state> --max-wait 150 [--since <seq>]\`; it never reads files, sleeps or polls another way. Next: ${next(agentId)}.`
        }
        return undefined
    } catch {
        return undefined
    }
}

// Context to append to the result of an allowed Bash call of the watcher, or undefined.
export const waveWatcherContext = (agentId: string, input: Readonly<Record<string, unknown>>, result: unknown): string | undefined => {
    try {
        const p = parse(String(input.command ?? ''))
        if (p === undefined) {
            return undefined
        }
        const out = isObject(result) ? result.result : undefined
        const stdout = isObject(out) && typeof out.stdout === 'string' ? out.stdout : ''
        const answer = lastJson(stdout)

        if (p.verb === 'run') {
            const state = answer?.state
            if (typeof state === 'string') {
                // A new wave has no seq yet: the next wait has no --since.
                tracked.set(agentId, { state })
            }
        } else {
            let entry = tracked.get(agentId)
            if (entry?.state !== p.state) {
                entry = { state: p.state }
                tracked.set(agentId, entry)
            }
            const seq = answer?.seq
            if (typeof seq === 'number') {
                entry.seq = seq
            }
        }

        if (isObject(out) && typeof out.backgroundTaskId === 'string') {
            return `orchestra: this call was moved to the background, so its answer will not reach you. Do not read its output file. Run ${next(agentId)} in the foreground now.`
        }
        if (p.verb === 'wait' && answer?.status === 'running') {
            return `orchestra: the wave is still running (status running): that is not a final answer. Run ${next(agentId)} now, and return only on needs, held, done, stopped or crashed.`
        }
        return undefined
    } catch {
        return undefined
    }
}
