import type { Io } from '../lib/io.ts'
import { agentExec, loadEnforcement } from '../lib/config.ts'

// SessionEnd: remove the finishing session's orchestra worktrees, keeping any
// with uncollected changes -- unreviewed work is the only record of what a
// worker tried, so it is surfaced, never discarded.
//
// A BACKSTOP, NOT THE MECHANISM: SessionEnd never runs when the session is
// killed, and when it does it races teardown. The pipeline sweeps at run end
// (skills/run/SKILL.md); this is insurance for express-lane work and abnormal
// endings, and `agent-exec isolate sweep` (/cleanup) is the manual catch-all.
// `--session` only ever matches this session's worktrees, never another's or
// legacy `orchestra/<task>` ones.
//
// Escape hatches: ORCHESTRA_SESSION_CLEANUP=off; enforcement.session_cleanup:
// off; fail-open on everything unexpected, the removal under a hard timeout.

type Entry = { status?: unknown, task?: unknown, path?: unknown, reason?: unknown }

// The summary for `agent-exec isolate remove` output, or undefined to stay silent.
const summarize = (stdout: string): string | undefined => {
    let removed: unknown
    try {
        removed = (JSON.parse(stdout) as { removed?: unknown }).removed
    } catch {
        return undefined
    }
    if (!Array.isArray(removed)) {
        return undefined
    }
    const entries = removed.filter((x): x is Entry => typeof x === 'object' && x !== null)
    const removedCount = entries.filter(x => x.status === 'removed').length
    const dirty = entries.filter(x => x.status === 'dirty')
    const errors = entries.filter(x => x.status === 'error')
    if (removedCount === 0 && dirty.length === 0 && errors.length === 0) {
        return undefined
    }
    const lines: string[] = []
    if (removedCount > 0) {
        lines.push(`orchestra: removed ${removedCount} session worktree(s).`)
    }
    if (dirty.length > 0) {
        lines.push(`orchestra: kept ${dirty.length} worktree(s) with uncollected changes:`)
        for (const x of dirty) {
            lines.push(`  ${String(x.task)}: ${String(x.path)}`)
        }
        lines.push('  inspect: agent-exec isolate diff --task <id>   |   '
            + 'discard: agent-exec isolate remove --task <id> --force   |   '
            + 'review all leftovers later: /cleanup')
    }
    if (errors.length > 0) {
        lines.push(`orchestra: ${errors.length} worktree removal(s) failed: ${String(errors[0]?.reason)}`)
    }

    return lines.join('\n')
}

export const cleanupWorktrees = async (io: Io, rawSessionId: string, cwd: string): Promise<void> => {
    const sessionId = rawSessionId.replace(/[^A-Za-z0-9_-]/g, '')
    if (!sessionId || await io.env('ORCHESTRA_SESSION_CLEANUP') === 'off') {
        return
    }
    const lookup = await loadEnforcement(io)
    if (lookup.kind === 'missing' || (lookup.kind === 'ok' && lookup.enforcement.session_cleanup === 'off')) {
        return
    }
    const repo = cwd || await io.sessionCwd().catch(() => '')
    const r = await agentExec(io, ['isolate', 'remove', '--session', sessionId, '--repo', repo], 20000)
    // A nonzero exit still prints the per-worktree report it managed.
    const summary = summarize(r.kind === 'missing' ? '' : r.stdout ?? '')
    if (summary) {
        io.log(summary)
    }
}
