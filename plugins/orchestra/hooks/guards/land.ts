import type { Io } from '../lib/io.ts'
import { field } from '../lib/agents.ts'
import { enforcementOf, isOff, loadEnforcement } from '../lib/config.ts'
import { dirname, join, normalize } from '../lib/paths.ts'
import { gitCall, hasEscape, segments, stripData, words } from '../lib/shell.ts'
import { leaseFile } from './worktree-lease.ts'

// An orchestra branch's commits are authored "orchestra" with "orchestra
// integrate <id>" subjects: a `git merge --ff-only orchestra/...` from the
// user's checkout put 273 of them into the user's history. The sanctioned path
// is `agent-exec isolate land`, one commit with the user's identity and a real
// message. This guard makes the improvised merge fail with directions to it.
//
// A merge, rebase, reset, cherry-pick or pull naming an orchestra branch is
// denied unless its directory is inside an orchestra worktree: a linked
// worktree whose git dir holds `orchestra-baseline` or `orchestra-adopted`,
// where the runner and supervisors integrate. Read-only verbs (log, diff) and
// branches that merely contain "orchestra" are never denied. Quoted and
// heredoc text is data.
//
// The directory is `-C <path>` (relative to a preceding `cd`), else the last
// plain `cd <path>` earlier on the same command line, else the cwd. A `cd`
// whose target is a variable or glob is not followed, so the cwd applies.
//
// Escape hatches: [orchestra:allow-orchestra-merge: <reason>] in the command or
// description; ORCHESTRA_ENFORCEMENT=off; enforcement.orchestra_merge: off.
// Fail-open on anything unexpected, a failed config lookup included.

const LAND_VERBS = new Set(['merge', 'rebase', 'reset', 'cherry-pick', 'pull'])
const ORCHESTRA_BRANCH = /^(?:origin\/)?(?:refs\/heads\/)?orchestra\//
const MARKERS = ['orchestra-baseline', 'orchestra-adopted']

type Hit = { ref: string, dir: string }

// `~` expands to HOME, as the lease guard does, so `git -C ~/x` is the path the user meant.
const landResolve = async (io: Io, base: string, p: string): Promise<string> => {
    const tilde = p === '~' || p.startsWith('~/')
    const home = tilde ? await io.env('HOME') ?? '' : ''

    return normalize(join(base, home ? home + p.slice(1) : p))
}

const landHits = async (io: Io, command: string, cwd: string): Promise<Hit[]> => {
    const out: Hit[] = []
    let here = cwd
    for (const raw of segments(stripData(command))) {
        const seg = raw.trim()
        const cd = /^cd\s+(\S+)$/.exec(seg)
        if (cd?.[1] && !/[$`*?]/.test(cd[1])) {
            here = await landResolve(io, here, cd[1])
            continue
        }
        const git = gitCall(seg)
        if (!git || !LAND_VERBS.has(git.verb)) {
            continue
        }
        const ref = words(git.rest).find(w => ORCHESTRA_BRANCH.test(w))
        if (ref === undefined) {
            continue
        }
        const c = /-C\s+(\S+)/.exec(git.globals)
        const dir = c?.[1] ? await landResolve(io, here, c[1]) : here
        out.push({ ref, dir })
    }

    return out
}

// True for a linked worktree the orchestra runner created or adopted.
const landInOrchestraTree = async (io: Io, dir: string): Promise<boolean> => {
    const found = await leaseFile(io, dir)
    if (!found) {
        return false
    }
    const gitDir = dirname(found.file)
    for (const marker of MARKERS) {
        if (await io.exists(`${gitDir}/${marker}`)) {
            return true
        }
    }

    return false
}

const reason = (ref: string, dir: string): string =>
    `orchestra: do not merge, rebase, reset, cherry-pick or pull an orchestra branch (${ref}) into ${dir}. `
    + 'Its commits are authored "orchestra" with "orchestra integrate <id>" subjects and must not reach the user\'s history. '
    + 'When the user asks to take the run\'s result, land it from the user\'s checkout with '
    + '`agent-exec isolate land --task <into> -m "<subject in the repository\'s convention>"`: '
    + 'one squashed commit with the user\'s identity. '
    + 'To override deliberately, include [orchestra:allow-orchestra-merge: <reason>] in the command or its description.'

export const checkLand = async (io: Io, input: Readonly<Record<string, unknown>>, cwd: string): Promise<string | undefined> => {
    try {
        const command = field(input, 'command')
        if (hasEscape('orchestra-merge', command, field(input, 'description'))
            || await io.env('ORCHESTRA_ENFORCEMENT') === 'off') {
            return undefined
        }
        let denied: Hit | undefined
        for (const hit of await landHits(io, command, cwd)) {
            if (!await landInOrchestraTree(io, hit.dir)) {
                denied = hit
                break
            }
        }
        if (!denied) {
            return undefined
        }
        // Looked up only when about to deny. A failed lookup is not a verdict: fail open.
        const lookup = await loadEnforcement(io)
        if (lookup.kind === 'failed' || isOff(enforcementOf(lookup).orchestra_merge)) {
            return undefined
        }

        return reason(denied.ref, denied.dir)
    } catch {
        return undefined
    }
}
