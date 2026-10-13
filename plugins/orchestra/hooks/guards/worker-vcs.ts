import type { Io } from '../lib/io.ts'
import { field, isSupervisor, type WorkerCall } from '../lib/agents.ts'
import { loadEnforcement } from '../lib/config.ts'
import { gitCall, hasEscape, segments, stripData, words } from '../lib/shell.ts'

// Workers must not run destructive VCS commands against the user's shared
// working tree. Transcripts held 35 of them (`git checkout --`, `git restore`,
// `git reset --hard`, `git clean -fd`) from every model tier, despite three
// generations of escalating prose in the worker prompts: a worker's only view
// is `git status`, and a diff it did not author reads as contamination.
// Worktree isolation is the real fix; this is the second layer for runs that
// did not isolate, non-repo checkouts, and workers that wander out.
//
// Deny only when ALL hold: Bash from a subagent (the main thread resolving a
// rebase with `git checkout --ours` is doing its job); the command INVOKES a
// destructive git operation (heredoc/quoted text is not an invocation); the
// cwd's branch is not orchestra's own `orchestra/*` (a worker may reset its
// own worktree) -- EXCEPT `git stash`, denied in every worktree because
// refs/stash is one stack shared by all of them (two parallel workers once
// popped each other's entries, swapping their changes); not orchestra-delegate
// (owns snapshots and rollbacks); no escape hatch.
//
// Escape hatches: [orchestra:allow-vcs(: reason)] in the command or
// description; ORCHESTRA_VCS_GUARD=off; enforcement.worker_vcs: off; fail-open
// on anything unexpected.

type Hit = 'none' | 'tree' | 'shared'

const classify = (segment: string): Hit => {
    const call = gitCall(segment)
    if (!call) {
        return 'none'
    }
    const ws = words(call.rest)
    const has = (...flags: string[]): boolean => ws.some(w => flags.includes(w))
    switch (call.verb) {
        case 'checkout':
            // -b/-B create a branch; a bare branch switch refuses to clobber.
            // What destroys work is a pathspec restore or a forced switch.
            if (has('-b', '-B')) {
                return 'none'
            }

            return has('--', '.', '-f', '--force', '--ours', '--theirs') ? 'tree' : 'none'
        case 'restore':
            // --staged alone only rewrites the index; with --worktree/-W the
            // working tree is back in scope.
            return has('--staged') && !has('--worktree', '-W') ? 'none' : 'tree'
        case 'reset':
            return has('--hard', '--merge', '--keep') ? 'tree' : 'none'
        case 'clean':
            return ws.some(w => /^-[A-Za-z]*f/.test(w)) ? 'tree' : 'none'
        case 'stash': {
            const sub = ws[0] ?? 'push'
            // Anything that pushes to, pops from, applies or drops the shared
            // stack can take another worker's entry; list/show only read it.
            if (['push', 'save', 'store', 'pop', 'apply', 'drop', 'clear', 'branch'].includes(sub) || sub.startsWith('-')) {
                return 'shared'
            }

            return sub === 'create' ? 'tree' : 'none'
        }
        case 'worktree':
            return ws[0] === 'remove' && has('-f', '--force') ? 'tree' : 'none'
        default:
            return 'none'
    }
}

const STASH_REASON = 'orchestra: a worker must not use git stash (push/pop/apply/drop). refs/stash is a single stack shared by EVERY worktree of this repository, so parallel workers pop each other\'s entries -- this has swapped two workers\' changes between their worktrees. Use the per-worktree equivalent instead, which nobody else can see or pop: \'agent-exec shelf push [--keep-index] [-- PATH...]\', then \'agent-exec shelf pop\' (also: list, apply, drop). To override deliberately, include [orchestra:allow-vcs: <reason>] in the command or its description.'

const treeReason = (branch: string): string =>
    `orchestra: a worker must not run destructive VCS commands against the user's shared working tree (branch '${branch}'). This tree holds work from other tasks and from the user; a diff you did not author is not contamination and must not be reverted, stashed, or reset. If your task genuinely needs to undo something, undo only what YOU wrote, by editing the file back. If you need a tree of your own, ask the supervisor to dispatch you with 'agent-exec dispatch --isolate always --task <id>', which gives you a worktree where this restriction does not apply. To override deliberately, include [orchestra:allow-vcs: <reason>] in the command or its description.`

export const checkWorkerVcs = async (io: Io, call: WorkerCall): Promise<string | undefined> => {
    if (call.tool !== 'Bash') {
        return undefined
    }
    const command = field(call.input, 'command')
    const kinds = segments(stripData(command)).map(classify)
    const hit: Hit = kinds.includes('shared') ? 'shared' : kinds.includes('tree') ? 'tree' : 'none'
    if (hit === 'none') {
        return undefined
    }
    if (await io.env('ORCHESTRA_VCS_GUARD') === 'off') {
        return undefined
    }
    if (hasEscape('vcs', command, field(call.input, 'description')) || isSupervisor(call.agentType) || !call.cwd) {
        return undefined
    }
    const head = await io.run(['git', 'rev-parse', '--abbrev-ref', 'HEAD'], { cwd: call.cwd, timeoutMs: 5000 })
        .catch(() => undefined)
    const branch = head?.exitCode === 0 ? head.stdout.trim() : ''
    if (!branch) {
        // Not a repository: there is no shared working tree to protect.
        return undefined
    }
    if (hit === 'tree' && branch.startsWith('orchestra/')) {
        return undefined
    }
    // Default to blocking: this only runs inside an orchestra install, and
    // preventing the accident is the point.
    const lookup = await loadEnforcement(io, call.sessionId)
    if (lookup.kind === 'ok' && lookup.enforcement.worker_vcs === 'off') {
        return undefined
    }

    return hit === 'shared' ? STASH_REASON : treeReason(branch)
}
