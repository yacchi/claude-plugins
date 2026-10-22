import { agentExec } from './config.ts'
import type { Io } from './io.ts'

// Machine-wide admission for a worker's heavy Bash commands (installs, tests,
// typechecks, lints, builds): past about five at once they all slow down
// several times over. agent-exec classifies the command (the one place that
// knows what is heavy) and, when it is, blocks until one of the machine's
// heavy slots is free. The wait happens here, before the tool starts, so it
// never counts against the Bash tool's own timeout.
//
// The slot's owner is agent-exec's parent, this Claude Code process: a
// session that dies frees its slots. Every failure (agent-exec missing, an
// error, a timeout, bad output) runs the command unadmitted, never blocked.

// One process.run may last at most 600 s; agent-exec gives up a minute
// earlier, so a timed-out call never leaves a waiter behind that takes a
// slot nobody releases.
const ACQUIRE_TIMEOUT_MS = 600_000
const MAX_WAIT = '540s'
const RELEASE_TIMEOUT_MS = 5000

// The release of a held slot, or undefined when the command runs unadmitted.
export const acquireHeavy = async (io: Io, command: string): Promise<(() => Promise<void>) | undefined> => {
    const r = await agentExec(
        io,
        ['slot', 'acquire', '--pool', 'heavy', `--command=${command}`, '--max-wait', MAX_WAIT],
        ACQUIRE_TIMEOUT_MS,
    )
    if (r.kind !== 'ok') {
        return undefined
    }
    let token = ''
    try {
        const data: unknown = JSON.parse(r.stdout)
        if (typeof data === 'object' && data !== null) {
            const d = data as Record<string, unknown>
            if (d.admitted === true && typeof d.token === 'string') {
                token = d.token
            }
        }
    } catch {
        return undefined
    }
    if (!token) {
        return undefined
    }

    return async () => {
        await agentExec(io, ['slot', 'release', token], RELEASE_TIMEOUT_MS)
    }
}

// Runs `run` holding a heavy slot when the command needs one; the slot is
// released however `run` ends (a rejection included: an aborted tool call).
export const withHeavySlot = async <T>(io: Io, command: string, run: () => Promise<T>): Promise<T> => {
    const release = await acquireHeavy(io, command).catch(() => undefined)
    try {
        return await run()
    } finally {
        await release?.().catch(() => undefined)
    }
}
