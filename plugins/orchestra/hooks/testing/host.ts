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
    // The base answer of a tool call, when it gives one; undefined answers `{ result: 'ran' }`.
    toolCall?: (call: Readonly<Record<string, unknown>>) => { result?: unknown, context?: string[] } | undefined,
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

// Windows when this file was loaded from a drive path (the kit has no
// platform API). There the engine hands the `fs.*` hooks `C:\w\repo` for a
// `/w/repo` or `C:/w/repo` the plugin asked about, and a real `realPath` comes
// back with `\`; git prints `C:/w/repo`. The host takes either separator,
// keeps `C:/w/repo` itself, and answers `realPath` with `\` as the engine does.
export const WINDOWS = /^file:\/\/\/[A-Za-z]:/.test(import.meta.url)

// A test's absolute path on this platform: `/w/repo` as given on POSIX,
// `C:/w/repo` on Windows.
export const abs = (p: string): string => (WINDOWS ? `C:${p}` : p)

// Where the host's Git for Windows lives: `git --exec-path` answers under it,
// and its bin/sh.exe, usr/bin/mv.exe and usr/bin/rm.exe exist.
const GIT_ROOT = 'C:/Program Files/Git'
const BINARIES = new Set(WINDOWS ? [`${GIT_ROOT}/bin/sh.exe`, `${GIT_ROOT}/usr/bin/mv.exe`, `${GIT_ROOT}/usr/bin/rm.exe`] : [])

// The program the plugin runs for a POSIX tool on this platform: the bare
// name on POSIX, Git's copy on Windows (lib/platform.ts toolOf).
export const toolPath = (name: string): string =>
    !WINDOWS ? name : name === 'sh' ? `${GIT_ROOT}/bin/sh.exe` : `${GIT_ROOT}/usr/bin/${name}.exe`

// Whether argv[0] is the shell the plugin runs agent-exec through.
export const isSh = (argv0: string | undefined): boolean => argv0 === toolPath('sh')

// `C:/` for a drive path, `/` otherwise.
const rootOf = (p: string): string => (WINDOWS && /^[A-Za-z]:/.test(p) ? `${p.slice(0, 2).toUpperCase()}/` : '/')

// A path an `fs.*` hook receives, as the test spelled it: on Windows the
// engine's `\` back to `/`; as is on POSIX.
export const slashed = (p: string): string => (WINDOWS ? p.replace(/\\/g, '/') : p)

const norm = (p: string): string => {
    const s = slashed(p)
    const root = rootOf(s)
    const out: string[] = []
    for (const part of (root === '/' ? s : s.slice(2)).split('/')) {
        if (part === '..') {
            out.pop()
        } else if (part !== '' && part !== '.') {
            out.push(part)
        }
    }

    return `${root}${out.join('/')}`
}

const parentOf = (p: string): string => {
    const root = rootOf(p)
    const cut = p.lastIndexOf('/')

    return cut < root.length ? root : p.slice(0, cut)
}

const within = (child: string, parent: string): boolean =>
    child === parent || child.startsWith(parent.endsWith('/') ? parent : `${parent}/`)

const ok = (stdout: string) => ({ exitCode: 0, stdout, stderr: '', isStdoutTruncated: false, isStderrTruncated: false })
const fail = (exitCode: number) => ({ exitCode, stdout: '', stderr: 'fatal', isStdoutTruncated: false, isStderrTruncated: false })

// The agents `startAgent` registered on the current host. Each host starts with
// an empty registry; a test builds its host before it starts agents.
let registry = new Map<string, Registered>()

export const fakeHost = (on: On, options: HostOptions) => {
    const dirs = new Set<string>(['/'])
    const files = new Map<string, string>(Object.entries(options.files ?? {}).map(([k, v]) => [norm(k), v]))
    const links = new Map<string, string>(Object.entries(options.links ?? {}).map(([k, v]) => [norm(k), v]))
    const runs: string[][] = []
    const logs: string[] = []
    const commands: string[] = []
    const inputs: Readonly<Record<string, unknown>>[] = []
    const session ={ model: options.model ?? '', root: options.root ?? options.cwd ?? '/' }
    const agents = new Map<string, Registered>()
    registry = agents
    const addDir = (d: string): void => {
        let p = norm(d)
        const root = rootOf(p)
        dirs.add(root)
        while (p !== root) {
            dirs.add(p)
            p = parentOf(p)
        }
    }
    for (const d of options.dirs ?? []) {
        addDir(d)
    }
    for (const f of files.keys()) {
        addDir(parentOf(f))
    }

    const trees: Tree[] = []
    for (const repo of options.repos ?? []) {
        const main = norm(repo.main)
        const common = `${main}/.git`
        addDir(main)
        addDir(common)
        trees.push({ path: main, branch: repo.branch ?? 'main', gitDir: common, common, repo })
        for (const l of repo.linked ?? []) {
            const path = norm(l.path)
            const gitDir = `${common}/worktrees/${path.slice(path.lastIndexOf('/') + 1)}`
            addDir(path)
            addDir(gitDir)
            trees.push({ path, branch: l.branch, gitDir, common, repo })
        }
    }

    // Where a path lands, links followed component by component; undefined
    // when a component does not exist.
    const real = (path: string): string | undefined => {
        const s = slashed(path)
        const root = rootOf(s)
        let cur = root
        const parts = (root === '/' ? s : s.slice(2)).split('/').filter(s => s !== '' && s !== '.')
        for (const [i, part] of parts.entries()) {
            if (part === '..') {
                cur = parentOf(cur)
                continue
            }
            cur = cur === root ? `${root}${part}` : `${cur}/${part}`
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
        if (isSh(argv[0]) && argv[3] === 'agent-exec') {
            const answer = agentExec(argv.slice(4))
            if (answer === 'missing') {
                return { value: fail(127) }
            }

            return { value: { ...fail(answer.exitCode), stdout: answer.stdout ?? '' } }
        }
        if (argv[0] === 'git') {
            if (argv[1] === '--exec-path') {
                return { value: WINDOWS ? ok(`${GIT_ROOT}/mingw64/libexec/git-core\n`) : fail(1) }
            }

            return { value: git(argv.slice(1), cwd) }
        }
        if (argv[0] === toolPath('mv') && argv[1] === '-f' && argv[2] !== undefined && argv[3] !== undefined) {
            const text = files.get(norm(argv[2]))
            if (text === undefined) {
                return { value: fail(1) }
            }
            files.delete(norm(argv[2]))
            files.set(norm(argv[3]), text)

            return { value: ok('') }
        }
        if (argv[0] === toolPath('rm') && argv[1] === '-f' && argv[2] !== undefined) {
            files.delete(norm(argv[2]))

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

        const realPath = WINDOWS ? landed.replace(/\//g, '\\') : landed

        return { value: { kind, size: 0, mtimeMs: 0, isLink: links.has(norm(e.path)), ...(e.resolve ? { realPath } : {}) } }
    })
    on('fs.exists', (_$, e) => ({ value: BINARIES.has(norm(e.path)) || real(e.path) !== undefined }))
    on('fs.read', (_$, e) => {
        const landed = real(e.path)
        const text = landed === undefined ? undefined : files.get(landed)

        return text === undefined ? { deny: `ENOENT: ${e.path}` } : { value: text }
    })
    on('fs.write', (_$, e) => {
        addDir(parentOf(norm(e.path)))
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
    on('tool.call', (_$, e) => {
        inputs.push(e as unknown as Readonly<Record<string, unknown>>)
        const command = (e as unknown as Readonly<Record<string, unknown>>).command
        if (typeof command === 'string') {
            commands.push(command)
        }

        return options.toolCall?.(e as unknown as Readonly<Record<string, unknown>>) ?? { result: 'ran' }
    })
    on('turn.start', (_$, e) => ({ turnId: e.turnId }))
    on('turn.complete', () => ({ text: '' }))
    on('turn.step', async function* (_$, e) {
        return { turnId: e.turnId, index: e.index, answer: '', toolUses: [], stopReason: 'end_turn', usage: null } as never
    })
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

    return { files, runs, logs, clock, commands, inputs }
}

// The kit types `$` per tool and event; these pass only the fields the
// guards read.
export type CallResult = { deny?: string, result?: unknown, context?: string[] }

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

// One model request of an agent's loop, as the engine fires `turn.step` before it
// is sent; the stream is read to its end, as the engine does.
export const requestStep = async ($: any, agentId: string | undefined, index = 0): Promise<unknown> => {
    const stream = $.turn.step({ turnId: 't', index, model: 'sonnet', messageCount: 1, ...(agentId ? { agentId } : {}) })
    let result: IteratorResult<unknown, unknown>
    do {
        result = await stream.next()
    } while (!result.done)

    return result.value
}

// The main loop's turn; its answer is the text the turn produced.
export const finishTurn = ($: any, turnId: string, answer: string): Promise<unknown> =>
    $.turn.complete({ turnId, answer, reason: 'answer', durationMs: 1, isAborted: false })

export const submitPrompt = ($: any, text: string, extra: Record<string, unknown> = {}): Promise<{ text?: string, context?: string[] }> =>
    $.prompt.submit({ text, wait: false, origin: { kind: 'composer' }, ...extra })

// Each test session gets its own id, so module-level per-session state never
// leaks from one test into the next.
let sessions = 0
export const freshSession = (): string => `abcdefgh-${String(++sessions).padStart(4, '0')}-test`
