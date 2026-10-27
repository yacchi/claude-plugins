import type { Io } from '../lib/io.ts'
import { field } from '../lib/agents.ts'
import { enforcementOf, isOff, loadEnforcement } from '../lib/config.ts'
import { dirname, join, normalize } from '../lib/paths.ts'
import { gitCall, hasEscape, segments, stripDataKeepingVars, words } from '../lib/shell.ts'
import { leaseFile } from './worktree-lease.ts'

// An orchestra branch's commits are authored "orchestra" with "orchestra
// integrate <id>" subjects: a `git merge --ff-only orchestra/...` from the
// user's checkout put 273 of them into the user's history. The sanctioned path
// is `agent-exec isolate land`, one commit with the user's identity and a real
// message. This guard makes the improvised merge fail with directions to it.
//
// A git call naming an orchestra branch is denied when it moves or creates that
// ref (merge, rebase, reset, cherry-pick, pull, update-ref, push, a creating
// branch, checkout -b, switch -c) unless its directory is inside an orchestra
// worktree, where the runner and supervisors integrate. push is never exempt.
// Reading and deleting verbs (log, diff, branch --list, branch -D) pass.
//
// A merge or pull naming an Agent worktree branch (worktree-*, from the Agent
// tool and Workflow) is denied unless it is --ff-only: a plain merge records a
// merge commit that names that branch in the user's history. --squash skips
// land's checks, so it is denied too.
//
// A variable operand ($b, "$b", $(...)) or xargs hides the ref from its own
// line, so every ref word of the raw command counts for that call.
//
// The directory is `-C <path>` (relative to a preceding `cd`), else the last
// plain `cd <path>` earlier on the same command line, else the cwd. A `cd`
// whose target is a variable or glob is not followed, so the cwd applies.
//
// Escape hatches: [orchestra:allow-orchestra-merge: <reason>] in the command or
// description; ORCHESTRA_ENFORCEMENT=off; enforcement.orchestra_merge: off.
// Fail-open on anything unexpected, a failed config lookup included.

const ALWAYS_VERBS = new Set(['merge', 'rebase', 'reset', 'cherry-pick', 'pull'])
const ORCHESTRA_BRANCH = /^(?:origin\/)?(?:refs\/heads\/)?orchestra\//
const WORKTREE_BRANCH = /^(?:refs\/heads\/)?worktree-/
// A ref word of the raw command, after a quote, `=`, `(`, `:`, a backtick or whitespace.
const RAW_REF = /(?:^|[\s'"=(:`])((?:refs\/heads\/)?(?:orchestra\/[\w./*@-]+|worktree-[\w.*@-]+))/g
const BRANCH_READ_FLAGS = new Set(['-d', '-D', '--delete', '-m', '-M', '-l', '--list', '-a', '--all', '-r', '--remotes', '--contains', '--merged', '--no-merged', '--points-at', '--format', '-v', '-vv'])
const BRANCH_WRITE_FLAGS = new Set(['-f', '--force', '-c', '-C', '--copy'])
const SWITCH_CREATE_FLAGS = new Set(['-c', '-C', '--create', '--force-create'])
const MARKERS = ['orchestra-baseline', 'orchestra-adopted']

type Hit = { ref: string, dir: string, verb: string, worktree: boolean }

// `~` expands to HOME, as the lease guard does, so `git -C ~/x` is the path the user meant.
const landResolve = async (io: Io, base: string, p: string): Promise<string> => {
    const tilde = p === '~' || p.startsWith('~/')
    const home = tilde ? await io.env('HOME') ?? '' : ''

    return normalize(join(base, home ? home + p.slice(1) : p))
}

// The words of a git call's arguments before a bare `--`, which ends the refs.
const beforeDashes = (rest: string): string[] => {
    const args = words(rest)
    const end = args.indexOf('--')

    return end < 0 ? args : args.slice(0, end)
}

// The refs a word names: the source side of a push refspec, else each side of a
// range or `a:b` pair, without the `+`, `^` and `~` decorations.
const namedRefs = (operand: string, verb: string): string[] => {
    const sides = verb === 'push' ? [operand.split(':')[0] ?? ''] : operand.split(/\.\.\.|\.\.|:/)

    return sides.map(s => s.replace(/^[+^]/, '').replace(/(?:\^!|\^@|\^\d*|~\d+)$/, ''))
}

// Whether the verb moves or creates the ref it names, rather than reading it.
const movesRef = (verb: string, flags: ReadonlySet<string>, operands: number): boolean => {
    const has = (set: ReadonlySet<string>) => [...set].some(f => flags.has(f))
    if (ALWAYS_VERBS.has(verb)) {
        return true
    }
    switch (verb) {
        case 'update-ref':
            return !flags.has('-d')
        case 'push':
            return !flags.has('-d') && !flags.has('--delete')
        case 'branch':
            return !has(BRANCH_READ_FLAGS) && (has(BRANCH_WRITE_FLAGS) || operands >= 2)
        case 'checkout':
            return flags.has('-b') || flags.has('-B')
        case 'switch':
            return has(SWITCH_CREATE_FLAGS)
        default:
            return false
    }
}

const landHits = async (io: Io, command: string, cwd: string): Promise<Hit[]> => {
    const out: Hit[] = []
    let here = cwd
    for (const raw of segments(stripDataKeepingVars(command))) {
        const seg = raw.trim()
        const cd = /^cd\s+(\S+)$/.exec(seg)
        if (cd?.[1] && !/[$`*?]/.test(cd[1])) {
            here = await landResolve(io, here, cd[1])
            continue
        }
        const git = gitCall(seg)
        if (!git) {
            continue
        }
        const args = beforeDashes(git.rest)
        const flags = new Set(args.filter(w => w.startsWith('-')))
        const operands = args.filter(w => !w.startsWith('-'))
        const names = operands.flatMap(w => namedRefs(w, git.verb))
        if (git.viaXargs || operands.some(w => /[$`]/.test(w))) {
            names.push(...[...command.matchAll(RAW_REF)].map(m => m[1] ?? ''))
        }
        const landsFfOnly = flags.has('--ff-only') && !flags.has('--squash')
        const c = /-C\s+(\S+)/.exec(git.globals)
        const dir = c?.[1] ? await landResolve(io, here, c[1]) : here
        for (const ref of names) {
            if (ORCHESTRA_BRANCH.test(ref) && movesRef(git.verb, flags, operands.length)) {
                out.push({ ref, dir, verb: git.verb, worktree: false })
            } else if (WORKTREE_BRANCH.test(ref) && (git.verb === 'merge' || git.verb === 'pull') && !landsFfOnly) {
                out.push({ ref, dir, verb: git.verb, worktree: true })
            }
        }
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

const ESCAPE = 'To override deliberately, include [orchestra:allow-orchestra-merge: <reason>] in the command or its description.'

const orchestraReason = ({ ref, dir, verb }: Hit): string =>
    `orchestra: do not ${verb} an orchestra branch (${ref}) into ${dir}. `
    + 'Its commits are authored "orchestra" with "orchestra integrate <id>" subjects and must not reach the user\'s history. '
    + 'When the user asks to take the run\'s result, finish each integration tree with '
    + '`agent-exec isolate merge --task <into> -m "<subject>"` and land the request\'s assembly from the user\'s checkout with '
    + '`agent-exec isolate land -m "<subject in the repository\'s convention>"`: '
    + 'one squashed commit with the user\'s identity. '
    + ESCAPE

const worktreeReason = ({ ref, dir }: Hit): string =>
    `orchestra: do not merge an Agent worktree branch (${ref}) into ${dir}. `
    + 'A merge commit would name that branch in the user\'s history. '
    + 'When the user asks to take the worktree\'s result, land it with '
    + '`agent-exec isolate land --path <its worktree> -m "<subject in the repository\'s convention>"`: '
    + 'one squashed commit with the user\'s identity (`git worktree list` shows the path). '
    + `To keep the branch's own commits, use \`git merge --ff-only ${ref}\`. `
    + ESCAPE

export const checkLand = async (io: Io, input: Readonly<Record<string, unknown>>, cwd: string): Promise<string | undefined> => {
    try {
        const command = field(input, 'command')
        if (hasEscape('orchestra-merge', command, field(input, 'description'))
            || await io.env('ORCHESTRA_ENFORCEMENT') === 'off') {
            return undefined
        }
        let denied: Hit | undefined
        for (const hit of await landHits(io, command, cwd)) {
            if (hit.verb === 'push' || !await landInOrchestraTree(io, hit.dir)) {
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

        return denied.worktree ? worktreeReason(denied) : orchestraReason(denied)
    } catch {
        return undefined
    }
}
