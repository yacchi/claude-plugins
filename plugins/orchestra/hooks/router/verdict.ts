import type { Io } from '../lib/io.ts'
import { dirname } from '../lib/paths.ts'
import { leaseFile } from '../guards/worktree-lease.ts'

// The session's verdict, for the hooks that act only on some sessions (the
// turn-size nudge, Agent calls, a hosted worker's guards). Computed from the
// session itself, not from a SessionStart event, which a seated hook ahead of
// this module may hold back.

// `worker`: a session hosted in an orchestra task worktree, told to do its
// prompt itself rather than orchestrate.
export type Verdict = 'instructor' | 'cheap' | 'unknown' | 'worker'

// Only a definite answer is kept: `unknown` (no model readable yet) is asked
// again on the next call, so a transient failure is not frozen into it.
const verdicts = new Map<string, Verdict>()

// The task worktree a worker session is hosted in: a LINKED worktree whose git
// dir carries the orchestra baseline or adopted marker, and whose role is not
// `integration`. Anything unexpected reads as not a worker.
const workerTree = async (io: Io, cwd: string): Promise<string | undefined> => {
    try {
        const found = cwd ? await leaseFile(io, cwd) : undefined
        if (!found) {
            return undefined
        }
        const gitDir = dirname(found.file)
        const marked = await io.exists(`${gitDir}/orchestra-baseline`) || await io.exists(`${gitDir}/orchestra-adopted`)
        if (!marked) {
            return undefined
        }
        const role = `${gitDir}/orchestra-role`
        if (await io.exists(role) && (await io.read(role)).trim() === 'integration') {
            return undefined
        }

        return found.top
    } catch {
        return undefined
    }
}

const compute = async (io: Io): Promise<Verdict> => {
    const dir = await io.root().catch(() => '') || await io.sessionCwd().catch(() => '')
    if (await workerTree(io, dir) !== undefined) {
        return 'worker'
    }
    const model = (await io.model().catch(() => '')).trim()
    if (model === '') {
        return 'unknown'
    }

    // Opus or Fable is the expensive instructor tier the router is for.
    return /opus|fable/i.test(model) ? 'instructor' : 'cheap'
}

export const verdictOf = async (io: Io, sessionId: string): Promise<Verdict> => {
    const kept = verdicts.get(sessionId)
    if (kept !== undefined) {
        return kept
    }
    const verdict = await compute(io)
    if (sessionId && verdict !== 'unknown') {
        verdicts.set(sessionId, verdict)
    }

    return verdict
}
