import type { Io } from '../lib/io.ts'
import { field, isSupervisor, type WorkerCall } from '../lib/agents.ts'
import { loadEnforcement } from '../lib/config.ts'
import { inside, isAbsolute, join, realpath } from '../lib/paths.ts'
import { basename, hasEscape, redirections, segments, stripData, words } from '../lib/shell.ts'

// A worker subagent must not write into the user's MAIN working tree while it
// has an isolated worktree of its own. Observed: a Sonnet worker whose prompt
// opened "WORKING TREE: <path> / cd there FIRST ... Never touch files outside
// that path" ran Edit/Write against the same files in the user's tree; its
// rejected round-1 output is what the user would have kept. Prose does not
// constrain a worker, and `sandbox.enabled` is session-global and Bash-only.
//
// Deny only when ALL hold: the call comes from a subagent; it is not
// orchestra-delegate (integrates into the main tree by design); the session
// has an orchestra worktree (branch `orchestra/<session8>/*` in `git worktree
// list`, session8 = first 8 chars of the session id) -- with none there is
// nowhere else to write and the express lane is untouched; the target's
// realpath lies inside the main tree (git's first entry) and inside no linked
// worktree. Any linked worktree counts, whatever its branch: Claude Code's
// Agent isolation puts a worker's own tree at <main>/.claude/worktrees/agent-*
// on a `worktree-*` branch, and that is the worker's tree, not the user's.
//
// Target coverage is deliberately asymmetric. Edit/Write/MultiEdit/
// NotebookEdit: full (`file_path`, `notebook_path`; relative against cwd).
// Bash: only `> PATH`, `>> PATH`, `tee PATH...` and `sed -i ... PATH...`
// (paths that already exist, which is what separates a path from an unquoted
// `s/a/b/` script). Everything else in a Bash command -- cp, mv, rm, mkdir,
// interpreters -- FAILS OPEN BY DESIGN: parsing arbitrary shell for write
// targets costs too many false positives. Do not "fix" it. A token with an
// unexpanded `$VAR`, backtick or glob is skipped for the same reason.
//
// Escape hatches: [orchestra:allow-tree(: reason)] in a Bash command or
// description; ORCHESTRA_TREE_GUARD=off; enforcement.worker_tree: off; fail-open
// on everything unexpected, a failed config lookup included (and not kept, so
// a transient failure does not lock the session into blocking).

const STRUCTURED = new Set(['Edit', 'Write', 'MultiEdit', 'NotebookEdit'])

// `exists`: only counts when the path already exists (sed -i operands).
type Target = { path: string, exists: boolean }

const unexpanded = (tok: string): boolean => /[$`*?[\]{}]/.test(tok)

const bashTargets = (command: string): Target[] => {
    const out: Target[] = []
    for (const raw of segments(stripData(command))) {
        const seg = raw.trim()
        if (!seg) {
            continue
        }
        for (const tok of redirections(seg)) {
            out.push({ path: tok, exists: false })
        }
        const ws = words(seg)
        const tee = ws.findIndex(w => basename(w) === 'tee')
        if (tee >= 0) {
            for (const tok of ws.slice(tee + 1)) {
                if (!tok.startsWith('-')) {
                    out.push({ path: tok, exists: false })
                }
            }
        }
        const sed = ws.findIndex(w => basename(w) === 'sed')
        if (sed >= 0) {
            const rest = ws.slice(sed + 1)
            if (rest.some(t => t.startsWith('-i') || t.startsWith('--in-place'))) {
                for (const tok of rest) {
                    if (!tok.startsWith('-')) {
                        out.push({ path: tok, exists: true })
                    }
                }
            }
        }
    }

    return out.filter(t => !unexpanded(t.path))
}

type Worktree = { path: string, branch: string }

const parseWorktrees = (porcelain: string): Worktree[] => {
    const trees: Worktree[] = []
    for (const line of porcelain.split('\n')) {
        if (line.startsWith('worktree ')) {
            trees.push({ path: line.slice('worktree '.length), branch: '' })
        } else if (line.startsWith('branch ')) {
            const last = trees.at(-1)
            if (last) {
                last.branch = line.slice('branch '.length)
            }
        }
    }

    return trees
}

const reason = (offender: string): string =>
    `orchestra: this write targets the user's MAIN working tree ('${offender}'), and you are a worker with an isolated worktree of your own. Writing here is never a worker's job -- the instructor collects your diff and integrates it. Your prompt named a WORKING TREE path; cd there and redo this write against the same relative path inside it (git worktree list will show it if you lost it). If your task genuinely requires touching the main tree, hand that back to the instructor as part of your verdict instead. To override deliberately on a Bash call, include [orchestra:allow-tree: <reason>] in the command or its description.`

export const checkWorkerTree = async (io: Io, call: WorkerCall): Promise<string | undefined> => {
    let targets: Target[] = []
    if (STRUCTURED.has(call.tool)) {
        targets = ['file_path', 'notebook_path']
            .map(k => field(call.input, k))
            .filter(p => p !== '')
            .map(path => ({ path, exists: false }))
    } else if (call.tool === 'Bash') {
        targets = bashTargets(field(call.input, 'command'))
    }
    if (targets.length === 0) {
        return undefined
    }
    if (await io.env('ORCHESTRA_TREE_GUARD') === 'off') {
        return undefined
    }
    if (hasEscape('tree', field(call.input, 'command'), field(call.input, 'description')) || isSupervisor(call.agentType)) {
        return undefined
    }
    const session8 = call.sessionId.toLowerCase().slice(0, 8).replace(/[^a-z0-9-]/g, '')
    if (!call.cwd || !session8) {
        return undefined
    }
    const listed = await io.run(['git', 'worktree', 'list', '--porcelain'], { cwd: call.cwd, timeoutMs: 5000 })
        .catch(() => undefined)
    if (listed?.exitCode !== 0) {
        return undefined
    }
    const trees = parseWorktrees(listed.stdout)
    const main = trees[0]
    if (!main) {
        return undefined
    }
    const linked = trees.slice(1)
    const prefix = `refs/heads/orchestra/${session8}/`
    if (!linked.some(t => t.branch.startsWith(prefix))) {
        return undefined
    }
    // Every linked worktree shields a path, not just this session's orchestra
    // ones: one nested under the main tree must never read as a main-tree write.
    const mainReal = await realpath(io, main.path)
    if (mainReal === undefined) {
        return undefined
    }
    const shields: string[] = []
    for (const t of linked) {
        const real = await realpath(io, t.path)
        if (real !== undefined) {
            shields.push(real)
        }
    }
    let offender: string | undefined
    for (const target of targets) {
        const resolved = isAbsolute(target.path) ? target.path : join(call.cwd, target.path)
        if (target.exists && !await io.exists(resolved).catch(() => false)) {
            continue
        }
        const real = await realpath(io, resolved)
        if (real !== undefined && inside(real, mainReal) && !shields.some(w => inside(real, w))) {
            offender = real
            break
        }
    }
    if (offender === undefined) {
        return undefined
    }
    const lookup = await loadEnforcement(io, call.sessionId)
    if (lookup.kind === 'failed' || (lookup.kind === 'ok' && lookup.enforcement.worker_tree === 'off')) {
        return undefined
    }

    return reason(offender)
}
