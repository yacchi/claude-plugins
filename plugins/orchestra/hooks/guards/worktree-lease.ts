import type { Io } from '../lib/io.ts'
import { field, isSupervisor, type WorkerCall } from '../lib/agents.ts'
import { enforcementOf, loadEnforcement } from '../lib/config.ts'
import { dirname, join, normalize, realpath } from '../lib/paths.ts'
import { gitCall, hasEscape, redirections, segments, stripData } from '../lib/shell.ts'

// One writer per worktree. A worktree gives a task a tree of its own, but
// nothing stops a SECOND agent being pointed at it while the first is still
// there: a message to a workflow-spawned engineer resumed a COPY of it, and
// the copy and the original both committed the same files two seconds apart.
// Neither can tell from inside the tree that the other exists.
//
// A lease per linked worktree lives in that worktree's own git dir
// (`.git/worktrees/<name>/orchestra-lease.json`: never in the tree, never
// committed). The first subagent to WRITE takes it, each later write by the
// same agent refreshes it, and a write by a DIFFERENT subagent is denied while
// the holder was active within the window (default 120s). SubagentStop drops
// the holder's leases, so a correction round right after the previous one
// finished is never blocked; the window is the fallback for a holder that died
// without a SubagentStop.
//
// A write is an Edit/Write/MultiEdit/NotebookEdit target, or Bash running a
// state-changing git verb or redirecting into a file. Reads, tests and `git
// diff` never touch the lease, so a reviewer is unaffected. The main worktree
// is never leased: same-tree parallel work with disjoint ownership is
// supported there. Exempt: the main thread and orchestra-delegate.
//
// Escape hatches: [orchestra:allow-lease(: reason)] in the command or
// description; ORCHESTRA_LEASE_GUARD=off; enforcement.worktree_lease: off;
// fail-open on anything unexpected. Window: ORCHESTRA_LEASE_WINDOW or
// enforcement.worktree_lease_window (seconds).

const STRUCTURED = new Set(['Edit', 'Write', 'MultiEdit', 'NotebookEdit'])
const MUTATING = new Set(['commit', 'add', 'rm', 'mv', 'reset', 'checkout', 'switch', 'restore',
    'merge', 'cherry-pick', 'rebase', 'revert', 'am', 'apply', 'stash', 'clean', 'pull'])
const DEFAULT_WINDOW = 120

type Lease = { agent_id?: unknown, agent_type?: unknown, last_seen?: unknown, since?: unknown }

// The lease files each agent took, dropped at its SubagentStop. Kept in
// memory: a reload that loses it leaves the window to expire the lease.
const held = new Map<string, Set<string>>()

const str = (v: unknown): string => (typeof v === 'string' ? v : '')

const bashTargets = (command: string, base: string, resolve: (base: string, p: string) => string): string[] => {
    const out: string[] = []
    let here = base
    for (const raw of segments(stripData(command))) {
        const seg = raw.trim()
        if (!seg) {
            continue
        }
        // `cd <tree> && git commit` writes there, not in the starting cwd.
        const cd = /^cd\s+(\S+)$/.exec(seg)
        if (cd?.[1] && !/[$`*?]/.test(cd[1])) {
            here = resolve(here, cd[1])
            continue
        }
        for (const tok of redirections(seg)) {
            if (tok !== '/dev/null' && !/[$`*?]/.test(tok)) {
                out.push(resolve(here, tok))
            }
        }
        const git = gitCall(seg)
        if (git && MUTATING.has(git.verb)) {
            const c = /-C\s+(\S+)/.exec(git.globals)
            out.push(c?.[1] ? resolve(here, c[1]) : here)
        }
    }

    return out
}

const readLease = async (io: Io, path: string): Promise<Lease | undefined> => {
    try {
        const v: unknown = JSON.parse(await io.read(path))

        return typeof v === 'object' && v !== null && !Array.isArray(v) ? v as Lease : undefined
    } catch {
        return undefined
    }
}

// The lease file and the worktree's top for a path inside a LINKED worktree;
// undefined for the main worktree and outside any repository.
const leaseFile = async (io: Io, path: string): Promise<{ file: string, top: string } | undefined> => {
    let dir = normalize(path)
    for (;;) {
        const stat = await io.stat(dir, false).catch(() => undefined)
        if (stat?.kind === 'dir') {
            break
        }
        if (dir === '/') {
            return undefined
        }
        dir = dirname(dir)
    }
    const r = await io.run(
        ['git', '-C', dir, 'rev-parse', '--path-format=absolute', '--git-dir', '--git-common-dir', '--show-toplevel'],
        { timeoutMs: 5000 },
    ).catch(() => undefined)
    if (r?.exitCode !== 0) {
        return undefined
    }
    const [gitDir, common, top] = r.stdout.split('\n')
    if (!gitDir || !common || !top) {
        return undefined
    }
    if (await realpath(io, gitDir) === await realpath(io, common)) {
        return undefined
    }

    return { file: `${gitDir}/orchestra-lease.json`, top }
}

const windowFromEnv = async (io: Io): Promise<number | undefined> => {
    const v = await io.env('ORCHESTRA_LEASE_WINDOW')
    const n = v ? Number(v) : Number.NaN

    return Number.isFinite(n) ? n : undefined
}

const reason = (top: string, lease: Lease, age: number, limit: number): string => {
    const holder = str(lease.agent_type) || 'another agent'

    return `orchestra: the worktree ${top} is leased by ${holder} (agent ${str(lease.agent_id).slice(0, 12)}), which wrote to it ${Math.trunc(age)}s ago. `
        + 'Two agents writing one worktree corrupt each other\'s work: this has happened when a '
        + 'message to a running workflow agent resumed a COPY of it, and when a retry started '
        + 'while the original was still running. If you were started for this task while another '
        + 'agent may still be working on it, you are the duplicate: stop now, make no further '
        + 'changes anywhere, and report DUPLICATE. If you are certain you are the only agent '
        + `assigned here (a new round after the previous agent finished), wait ${Math.trunc(limit - age) + 1}s for the `
        + 'lease to expire and retry once. Override deliberately with '
        + '[orchestra:allow-lease: <reason>] in the command or its description.'
}

export const checkWorktreeLease = async (io: Io, call: WorkerCall): Promise<string | undefined> => {
    if (isSupervisor(call.agentType)) {
        return undefined
    }
    const command = field(call.input, 'command')
    if (hasEscape('lease', command, field(call.input, 'description'))) {
        return undefined
    }
    const mentionsHome = [command, field(call.input, 'file_path'), field(call.input, 'notebook_path')].some(s => s.includes('~'))
    const home = mentionsHome ? await io.env('HOME') ?? '' : ''
    const resolve = (base: string, p: string): string =>
        normalize(join(base, home && (p === '~' || p.startsWith('~/')) ? home + p.slice(1) : p))
    let targets: string[] = []
    if (STRUCTURED.has(call.tool)) {
        targets = ['file_path', 'notebook_path']
            .map(k => field(call.input, k))
            .filter(p => p !== '')
            .map(p => resolve(call.cwd, p))
    } else if (call.tool === 'Bash') {
        targets = bashTargets(command, call.cwd, resolve)
    }
    if (targets.length === 0 || await io.env('ORCHESTRA_LEASE_GUARD') === 'off') {
        return undefined
    }

    const now = await io.now() / 1000
    const done = new Set<string>()
    for (const target of targets) {
        const found = await leaseFile(io, target)
        if (!found || done.has(found.file)) {
            continue
        }
        done.add(found.file)
        const lease = await readLease(io, found.file)
        const mine = lease !== undefined && lease.agent_id === call.agentId
        if (lease && !mine) {
            const age = now - (Number(lease.last_seen) || 0)
            let limit = await windowFromEnv(io)
            if (limit === undefined) {
                // Config only matters when about to deny: the common path stays cheap.
                const e = enforcementOf(await loadEnforcement(io))
                if (e.worktree_lease === 'off') {
                    continue
                }
                const w = Number(e.worktree_lease_window)
                limit = Number.isFinite(w) && w ? w : DEFAULT_WINDOW
            }
            if (age < limit) {
                return reason(found.top, lease, age, limit)
            }
        }
        const since = mine ? lease?.since ?? now : now
        try {
            await io.write(found.file, JSON.stringify({ agent_id: call.agentId, agent_type: call.agentType, last_seen: now, since }))
            if (!mine) {
                const files = held.get(call.agentId) ?? new Set<string>()
                files.add(found.file)
                held.set(call.agentId, files)
            }
        } catch {
            // a lease we could not write is no reason to block the write
        }
    }

    return undefined
}

// SubagentStop: drop every lease this agent still holds.
export const releaseLeases = async (io: Io, agentId: string): Promise<void> => {
    const files = held.get(agentId)
    held.delete(agentId)
    if (!files || await io.env('ORCHESTRA_LEASE_GUARD') === 'off') {
        return
    }
    for (const file of files) {
        const lease = await readLease(io, file)
        if (lease?.agent_id === agentId) {
            await io.run(['rm', '-f', file], { timeoutMs: 5000 }).catch(() => undefined)
        }
    }
}
