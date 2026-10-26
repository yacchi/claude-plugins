import type { On } from 'claude-code'
import { mock } from 'claude-code/testing'

// A host in memory beneath the plugin, for the tests: a file system with
// symlinks, git repositories with linked worktrees, a scripted agent-exec,
// and the session's id and cwd. Every command the plugin runs is recorded.

export type Repo = {
    main: string,
    branch?: string,
    linked?: readonly { path: string, branch: string }[],
}

export type AgentExecAnswer = { exitCode: number, stdout?: string } | 'missing'

export type HostOptions = {
    sessionId: string,
    cwd?: string,
    dirs?: readonly string[],
    files?: Readonly<Record<string, string>>,
    links?: Readonly<Record<string, string>>,
    repos?: readonly Repo[],
    env?: Readonly<Record<string, string>>,
    now?: number,
    agentExec?: (args: readonly string[]) => AgentExecAnswer,
    model?: string,
    root?: string,
    messages?: readonly unknown[],
    rateLimits?: readonly { kind: string, percentUsed: number, resetsAt?: string }[],
}

type Tree = { path: string, branch: string, gitDir: string, common: string, repo: Repo }

// A subagent as the registry keeps it: its type, its working directory, the
// agent that spawned it (absent for the main loop), its status and its name.
type Registered = { type: string, cwd: string, parentId?: string, status: string, name?: string }

// Set around a `tool.call` or `session.end` so the stubs answer for the
// calling agent (cwd) and for the session being ended (root).
const during: { agent: string | undefined, root: string | undefined } = { agent: undefined, root: undefined }

const norm = (p: string): string => {
    const out: string[] = []
    for (const part of p.split('/')) {
        if (part === '..') {
            out.pop()
        } else if (part !== '' && part !== '.') {
            out.push(part)
        }
    }

    return `/${out.join('/')}`
}

const within = (child: string, parent: string): boolean =>
    child === parent || child.startsWith(parent === '/' ? '/' : `${parent}/`)

const ok = (stdout: string) => ({ exitCode: 0, stdout, stderr: '', isStdoutTruncated: false, isStderrTruncated: false })
const fail = (exitCode: number) => ({ exitCode, stdout: '', stderr: 'fatal', isStdoutTruncated: false, isStderrTruncated: false })

// The agents `startAgent` registered on the current host. Each host starts with
// an empty registry; a test builds its host before it starts agents.
let registry = new Map<string, Registered>()

export const fakeHost = (on: On, options: HostOptions) => {
    const dirs = new Set<string>(['/'])
    const files = new Map<string, string>(Object.entries(options.files ?? {}))
    const links = new Map<string, string>(Object.entries(options.links ?? {}))
    const runs: string[][] = []
    const logs: string[] = []
    const session = { model: options.model ?? '', root: options.root ?? options.cwd ?? '/' }
    const agents = new Map<string, Registered>()
    registry = agents
    const addDir = (d: string): void => {
        let p = norm(d)
        while (p !== '/') {
            dirs.add(p)
            p = p.slice(0, p.lastIndexOf('/')) || '/'
        }
    }
    for (const d of options.dirs ?? []) {
        addDir(d)
    }
    for (const f of files.keys()) {
        addDir(f.slice(0, f.lastIndexOf('/')) || '/')
    }

    const trees: Tree[] = []
    for (const repo of options.repos ?? []) {
        const common = `${repo.main}/.git`
        addDir(repo.main)
        addDir(common)
        trees.push({ path: repo.main, branch: repo.branch ?? 'main', gitDir: common, common, repo })
        for (const l of repo.linked ?? []) {
            const gitDir = `${common}/worktrees/${l.path.slice(l.path.lastIndexOf('/') + 1)}`
            addDir(l.path)
            addDir(gitDir)
            trees.push({ path: l.path, branch: l.branch, gitDir, common, repo })
        }
    }

    // Where a path lands, links followed component by component; undefined
    // when a component does not exist.
    const real = (path: string): string | undefined => {
        let cur = '/'
        const parts = path.split('/').filter(s => s !== '' && s !== '.')
        for (const [i, part] of parts.entries()) {
            if (part === '..') {
                cur = cur.slice(0, cur.lastIndexOf('/')) || '/'
                continue
            }
            cur = cur === '/' ? `/${part}` : `${cur}/${part}`
            const target = links.get(cur)
            if (target !== undefined) {
                const landed = real(target)
                if (landed === undefined) {
                    return undefined
                }
                cur = landed
            }
            const last = i === parts.length - 1
            if (!dirs.has(cur) && !(last && files.has(cur))) {
                return undefined
            }
        }

        return cur
    }

    const treeOf = (dir: string): Tree | undefined => {
        const r = real(dir)
        if (r === undefined) {
            return undefined
        }

        return trees.filter(t => within(r, t.path)).sort((a, b) => b.path.length - a.path.length)[0]
    }

    const git = (args: readonly string[], cwd: string) => {
        let dir = cwd
        let rest = args
        if (rest[0] === '-C' && rest[1] !== undefined) {
            dir = rest[1]
            rest = rest.slice(2)
        }
        const tree = treeOf(dir)
        if (!tree) {
            return fail(128)
        }
        const command = rest.join(' ')
        if (command === 'rev-parse --abbrev-ref HEAD') {
            return ok(`${tree.branch}\n`)
        }
        if (command === 'worktree list --porcelain') {
            const all = trees.filter(t => t.repo === tree.repo)

            return ok(all.map(t => `worktree ${t.path}\nHEAD 0123abcd\nbranch refs/heads/${t.branch}\n`).join('\n'))
        }
        if (command === 'rev-parse --path-format=absolute --git-dir --git-common-dir --show-toplevel') {
            return ok(`${tree.gitDir}\n${tree.common}\n${tree.path}\n`)
        }

        return fail(1)
    }

    const agentExec = options.agentExec ?? (() => ({ exitCode: 0, stdout: '{"enforcement": {}}' }))

    on('process.run', (_$, e) => {
        const argv = [...e.argv]
        runs.push(argv)
        const cwd = e.init?.cwd ?? options.cwd ?? '/'
        if (argv[0] === 'sh' && argv[3] === 'agent-exec') {
            const answer = agentExec(argv.slice(4))
            if (answer === 'missing') {
                return { value: fail(127) }
            }

            return { value: { ...fail(answer.exitCode), stdout: answer.stdout ?? '' } }
        }
        if (argv[0] === 'git') {
            return { value: git(argv.slice(1), cwd) }
        }
        if (argv[0] === 'mv' && argv[1] === '-f' && argv[2] !== undefined && argv[3] !== undefined) {
            const text = files.get(norm(argv[2]))
            if (text === undefined) {
                return { value: fail(1) }
            }
            files.delete(norm(argv[2]))
            files.set(norm(argv[3]), text)

            return { value: ok('') }
        }
        if (argv[0] === 'rm' && argv[1] === '-f' && argv[2] !== undefined) {
            files.delete(argv[2])

            return { value: ok('') }
        }

        return { value: fail(127) }
    })
    on('fs.stat', (_$, e) => {
        const landed = real(e.path)
        if (landed === undefined) {
            return { deny: `ENOENT: ${e.path}` }
        }
        const kind = dirs.has(landed) ? 'dir' as const : 'file' as const

        return { value: { kind, size: 0, mtimeMs: 0, isLink: links.has(norm(e.path)), ...(e.resolve ? { realPath: landed } : {}) } }
    })
    on('fs.exists', (_$, e) => ({ value: real(e.path) !== undefined }))
    on('fs.read', (_$, e) => {
        const landed = real(e.path)
        const text = landed === undefined ? undefined : files.get(landed)

        return text === undefined ? { deny: `ENOENT: ${e.path}` } : { value: text }
    })
    on('fs.write', (_$, e) => {
        addDir(e.path.slice(0, e.path.lastIndexOf('/')))
        files.set(norm(e.path), e.text)

        return { value: undefined }
    })
    on('session.id', () => ({ value: options.sessionId }))
    on('session.cwd', () => {
        const own = during.agent === undefined ? undefined : agents.get(during.agent)?.cwd

        return { value: own ?? options.cwd ?? '/' }
    })
    on('session.model', () => ({ value: session.model }))
    on('session.root', () => ({ value: during.root ?? session.root }))
    on('session.messages', () => ({ value: options.messages ?? [] }))
    on('agent.list', () => ({
        value: [...agents].map(([id, a]) => ({ id, type: a.type, description: '', status: a.status, parentId: a.parentId, name: a.name })),
    }))
    on('session.usage', () => ({
        value: { context: {}, rateLimits: options.rateLimits ?? [] },
    }))
    on('ui.log', (_$, e) => {
        logs.push(e.text)

        return { value: undefined }
    })
    on('tool.call', () => ({ result: 'ran' }))
    on('turn.start', (_$, e) => ({ turnId: e.turnId }))
    on('turn.complete', () => ({ text: '' }))
    on('session.end', (_$, e) => ({ sessionId: e.sessionId }))
    on('session.start', (_$, e) => ({ cwd: e.cwd }))
    on('prompt.submit', (_$, e) => ({ text: e.text, ...(e.context ? { context: e.context } : {}) }))

    // Emulation of the session and of its agents' directories, fed by the two
    // classic events the tests fire: `classic.SessionStart` describes the
    // session (model, root), and `classic.PostToolUse` moves an agent's cwd.
    // Orchestra itself listens to no classic event; the tests use these only to
    // describe a session and an agent's moving directory.
    on('classic.SessionStart', (_$, e) => {
        if (typeof e.model === 'string' && e.model !== '') {
            session.model = e.model
        }
        if (typeof e.cwd === 'string' && e.cwd !== '') {
            session.root = e.cwd
        }

        return {}
    })
    on('classic.PostToolUse', (_$, e) => {
        const agent = e.agent_id === undefined ? undefined : agents.get(e.agent_id)
        if (agent && typeof e.cwd === 'string' && e.cwd !== '') {
            agent.cwd = e.cwd
        }

        return {}
    })
    const clock = mock.clock(on, { now: options.now ?? 1_000_000 })
    mock.env(on, options.env ?? {})

    return { files, runs, logs, clock }
}

// The kit types `$` per tool and event; these pass only the fields the
// guards read.
export type CallResult = { deny?: string, result?: unknown }

// A tool call made inside subagent `agentId` (the main thread when undefined).
// The calling loop is recorded for the session.cwd stub while the call runs.
export const callTool = async ($: any, agentId: string | undefined, args: Record<string, unknown>): Promise<CallResult> => {
    during.agent = agentId
    try {
        return await $.tool.call({ ...args, ...(agentId ? { agentId } : {}) })
    } finally {
        during.agent = undefined
    }
}

// A subagent starts: its type and working directory are registered for the
// session, where the host's `agent.list` and `session.cwd` stubs read them.
// `extra` sets what a test needs of the agent's place in the tree.
export const startAgent = (
    _$: any,
    agentId: string,
    agentType: string,
    cwd: string,
    extra: { parentId?: string, status?: string, name?: string } = {},
): void => {
    registry.set(agentId, { type: agentType, cwd, parentId: extra.parentId, status: extra.status ?? 'running', name: extra.name })
}

// A subagent's status changes to a final one (or any other), as the engine's
// `agent.list` would report it.
export const stopAgent = (agentId: string, status = 'completed'): void => {
    const agent = registry.get(agentId)
    if (agent) {
        agent.status = status
    }
}

// A subagent's end: the turn it finished.
export const endAgent = ($: any, agentId: string): Promise<unknown> =>
    $.turn.complete({ agentId, turnId: 't', answer: '', reason: 'answer', durationMs: 1, isAborted: false })

// The session's end. Its cwd is the session root for this call's handling.
export const endSession = async ($: any, sessionId: string, cwd: string, reason = 'prompt_input_exit'): Promise<unknown> => {
    during.root = cwd
    try {
        return await $.session.end({ sessionId, reason, resume: { id: sessionId } })
    } finally {
        during.root = undefined
    }
}

export const beginTurn = ($: any, turnId: string, agentId?: string): Promise<unknown> =>
    $.turn.start({ text: '', turnId, ...(agentId ? { agentId } : {}) })

// The main loop's turn; its answer is the text the turn produced.
export const finishTurn = ($: any, turnId: string, answer: string): Promise<unknown> =>
    $.turn.complete({ turnId, answer, reason: 'answer', durationMs: 1, isAborted: false })

export const submitPrompt = ($: any, text: string, extra: Record<string, unknown> = {}): Promise<{ text?: string, context?: string[] }> =>
    $.prompt.submit({ text, wait: false, origin: { kind: 'composer' }, ...extra })

// Each test session gets its own id, so module-level per-session state never
// leaks from one test into the next.
let sessions = 0
export const freshSession = (): string => `abcdefgh-${String(++sessions).padStart(4, '0')}-test`
