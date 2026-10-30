import type { Io } from '../lib/io.ts'
import { field, isSupervisor, type WorkerCall } from '../lib/agents.ts'
import { loadEnforcement } from '../lib/config.ts'
import { inside, isAbsolute, join, normalize, realpath } from '../lib/paths.ts'
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
// Links: a Bash `ln` with a symbolic flag (-s, -sf, -sfn, --symbolic) is
// denied when any of its operands lies under an orchestra state root's
// requests/<id>/.work/ or .integration/ outside the worker's own tree (the
// worktree holding its cwd). A worker's tree must hold its own dependencies;
// a link into another task's tree was observed to stall the worker reading
// through it. Relative operands follow a plain `cd` earlier in the command.
// The state root is $HOME/.local/state/claude-orchestra: the hooks cannot read
// XDG_STATE_HOME. This rule does not need an orchestra worktree in the session.
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

const LINK_REASON = 'Do not link into another task\'s worktree: each orchestra tree holds its own dependencies. Install them in your own tree instead (e.g. pnpm install --frozen-lockfile in the package directory), or ESCALATE if that fails.'

// Words that run the command after them, so the command word follows them.
const PREFIX_WORDS = new Set(['command', 'exec', 'nohup', 'time', 'env', 'do', 'then', 'else', 'elif'])
const ASSIGNMENT = /^[A-Za-z_][A-Za-z0-9_]*=/
// A redirection word (`>f`, `2>&1`, `&>f`, or a bare `>`): the operands end before it.
const REDIRECTION = /^(?:\d*|&)[<>]/

// One step of a Bash command that matters to a link: a plain `cd`, or an `ln`
// with its operands (flags and redirections dropped) and whether it is symbolic.
type LinkStep = { kind: 'cd', target: string } | { kind: 'ln', symbolic: boolean, operands: string[] }

const isSymbolicLink = (step: LinkStep): boolean => step.kind === 'ln' && step.symbolic

// The words of one simple command, past any leading punctuation, assignments and prefix words.
const commandWords = (seg: string): string[] => {
    const ws = words(seg.trim().replace(/^[({!\s]+/, ''))
    while (ws[0] !== undefined && (ASSIGNMENT.test(ws[0]) || PREFIX_WORDS.has(ws[0]))) {
        ws.shift()
    }

    return ws
}

const linkSteps = (command: string): LinkStep[] => {
    const out: LinkStep[] = []
    for (const seg of segments(stripData(command))) {
        const ws = commandWords(seg)
        const first = ws[0] ?? ''
        const target = ws[1] ?? ''
        if (first === 'cd' && ws.length === 2 && target !== '-' && !unexpanded(target)) {
            out.push({ kind: 'cd', target })
        } else if (basename(first) === 'ln') {
            const args = ws.slice(1)
            const end = args.findIndex(w => REDIRECTION.test(w))
            const plain = end < 0 ? args : args.slice(0, end)
            out.push({
                kind: 'ln',
                symbolic: plain.some(w => w === '--symbolic' || /^-[^-]*s/.test(w)),
                operands: plain.filter(w => !w.startsWith('-') && !unexpanded(w)),
            })
        }
    }

    return out
}

// Strictly inside, or the same path.
const within = (child: string, parent: string): boolean => child === parent || inside(child, parent)

// Strictly under an orchestra state root's requests/<id>/.work/ or .integration/.
// `requests` is already real.
const inOrchestraState = (real: string, requests: string): boolean => {
    if (!inside(real, requests)) {
        return false
    }
    const rel = real.slice(requests.length + 1).split('/')

    return rel.length > 2 && (rel[1] === '.work' || rel[1] === '.integration')
}

// Whether a symbolic `ln` in the command links into an orchestra tree other
// than the worker's own. Fails open whenever the state root, the worktree
// listing or a path cannot be resolved.
const linkIntoOtherTree = async (io: Io, call: WorkerCall, steps: readonly LinkStep[]): Promise<boolean> => {
    if (!steps.some(isSymbolicLink) || !call.cwd) {
        return false
    }
    const home = await io.env('HOME') ?? ''
    if (home === '') {
        return false
    }
    const requests = await realpath(io, `${home}/.local/state/claude-orchestra/requests`)
    const cwd = await realpath(io, call.cwd)
    if (requests === undefined || cwd === undefined) {
        return false
    }
    const listed = await io.run(['git', 'worktree', 'list', '--porcelain'], { cwd: call.cwd, timeoutMs: 5000 })
        .catch(() => undefined)
    if (listed?.exitCode !== 0) {
        return false
    }
    // The own tree is the innermost worktree holding the call's cwd.
    let own: string | undefined
    for (const t of parseWorktrees(listed.stdout)) {
        const real = await realpath(io, t.path)
        if (real !== undefined && within(cwd, real) && (own === undefined || real.length > own.length)) {
            own = real
        }
    }
    own ??= cwd
    const at = (p: string): string => (p === '~' || p.startsWith('~/') ? home + p.slice(1) : p)
    let here = call.cwd
    for (const step of steps) {
        if (step.kind === 'cd') {
            here = normalize(join(here, at(step.target)))
        } else if (step.symbolic) {
            for (const op of step.operands) {
                const real = await realpath(io, normalize(join(here, at(op))))
                if (real !== undefined && inOrchestraState(real, requests) && !within(real, own)) {
                    return true
                }
            }
        }
    }

    return false
}

// Whether enforcement applies: false when the config switches the guard off, or
// the lookup failed (fail open, not kept).
const enforced = async (io: Io, sessionId: string): Promise<boolean> => {
    const lookup = await loadEnforcement(io, sessionId)

    return lookup.kind !== 'failed' && !(lookup.kind === 'ok' && lookup.enforcement.worker_tree === 'off')
}

export const checkWorkerTree = async (io: Io, call: WorkerCall): Promise<string | undefined> => {
    const command = field(call.input, 'command')
    let targets: Target[] = []
    if (STRUCTURED.has(call.tool)) {
        targets = ['file_path', 'notebook_path']
            .map(k => field(call.input, k))
            .filter(p => p !== '')
            .map(path => ({ path, exists: false }))
    } else if (call.tool === 'Bash') {
        targets = bashTargets(command)
    }
    const steps = call.tool === 'Bash' ? linkSteps(command) : []
    if (targets.length === 0 && !steps.some(isSymbolicLink)) {
        return undefined
    }
    if (await io.env('ORCHESTRA_TREE_GUARD') === 'off') {
        return undefined
    }
    if (hasEscape('tree', command, field(call.input, 'description')) || isSupervisor(call.agentType)) {
        return undefined
    }
    if (await linkIntoOtherTree(io, call, steps)) {
        return await enforced(io, call.sessionId) ? LINK_REASON : undefined
    }
    if (targets.length === 0) {
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
    if (!await enforced(io, call.sessionId)) {
        return undefined
    }

    return reason(offender)
}
