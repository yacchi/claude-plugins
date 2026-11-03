import { agentExec } from './config.ts'
import type { Io } from './io.ts'

// Machine-wide admission for a worker's heavy Bash commands (installs, tests,
// typechecks, lints, builds): past about five at once they all slow down
// several times over. agent-exec classifies the command (the one place that
// knows what is heavy) and, when it is, blocks until a slot of its pool is
// free (tests, lints and typechecks on the test pool, the rest on the heavy
// pool). The wait happens here, before the tool starts, so it never counts
// against the Bash tool's own timeout.
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
const CLASSIFY_TIMEOUT_MS = 5000
const GUARD_RESULT_TIMEOUT_MS = 5000
// A guard token is what agent-exec mints (16 hex characters); the bound keeps
// anything else off the shell line the prefix is spliced into.
const GUARD_TOKEN = /^[A-Za-z0-9_-]{8,64}$/

// What agent-exec tells of a command line: the op it names, whether it is
// heavy, whether it runs a whole repository's or workspace's suite, when the
// command should run under a worker-count cap or with the pnpm store entries
// the capped command line, and, when the command is heavy, the process guard's
// prefix (run before the command in the same shell) and its token.
export type CommandClass = { op: string, heavy: boolean, scope: 'full' | 'targeted', capped?: string, guard?: { prefix: string, token: string } }

// The command's class, or undefined when agent-exec could not tell (missing,
// failed, or bad output). cwd is the directory the command runs in, which
// decides the git tree whose pnpm store entries apply.
export const classifyCommand = async (io: Io, command: string, cwd?: string): Promise<CommandClass | undefined> => {
    const args = ['slot', 'classify', `--command=${command}`]
    if (cwd) {
        args.push(`--cwd=${cwd}`)
    }
    const r = await agentExec(io, args, CLASSIFY_TIMEOUT_MS)
    if (r.kind !== 'ok') {
        return undefined
    }
    try {
        const data: unknown = JSON.parse(r.stdout)
        if (typeof data !== 'object' || data === null) {
            return undefined
        }
        const d = data as Record<string, unknown>
        if (typeof d.op !== 'string' || typeof d.heavy !== 'boolean' || (d.scope !== 'full' && d.scope !== 'targeted')) {
            return undefined
        }

        const capped = d.capped_command
        const isCapped = typeof capped === 'string' && capped !== '' && capped !== command
        const prefix = d.guard_prefix
        const token = d.guard_token
        const guard = typeof prefix === 'string' && prefix !== '' && typeof token === 'string' && GUARD_TOKEN.test(token)
            ? { prefix, token }
            : undefined

        return {
            op: d.op,
            heavy: d.heavy,
            scope: d.scope,
            ...(isCapped ? { capped } : {}),
            ...(guard ? { guard } : {}),
        }
    } catch {
        return undefined
    }
}

// The process guard's report of a command it killed, or undefined when it
// killed none (or agent-exec could not tell). Asked once, after the guarded Bash
// call returned: agent-exec writes the record before it kills anything, so it
// never has to be polled for.
export const guardResult = async (io: Io, token: string): Promise<string | undefined> => {
    try {
        const r = await agentExec(io, ['proc', 'guard-result', `--token=${token}`], GUARD_RESULT_TIMEOUT_MS)
        if (r.kind !== 'ok') {
            return undefined
        }
        const data: unknown = JSON.parse(r.stdout)
        if (typeof data !== 'object' || data === null) {
            return undefined
        }
        const d = data as Record<string, unknown>
        if (d.killed !== true || typeof d.reason !== 'string') {
            return undefined
        }

        return `orchestra: agent-exec's process guard killed this command: ${d.reason}. Its process tree outgrew this machine (a fork bomb, or too many test workers at once). Run it again with fewer workers (e.g. jest --maxWorkers=2, vitest --maxWorkers=2) through the package's test script, and never start a test runner from node -e.`
    } catch {
        return undefined
    }
}

// The release of a held slot, or undefined when the command runs unadmitted.
export const acquireHeavy = async (io: Io, command: string): Promise<(() => Promise<void>) | undefined> => {
    const r = await agentExec(
        io,
        ['slot', 'acquire', `--command=${command}`, '--max-wait', MAX_WAIT],
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
