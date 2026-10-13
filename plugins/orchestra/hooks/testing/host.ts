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
}

type Tree = { path: string, branch: string, gitDir: string, common: string, repo: Repo }

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

export const fakeHost = (on: On, options: HostOptions) => {
    const dirs = new Set<string>(['/'])
    const files = new Map<string, string>(Object.entries(options.files ?? {}))
    const links = new Map<string, string>(Object.entries(options.links ?? {}))
    const runs: string[][] = []
    const logs: string[] = []
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
    on('session.cwd', () => ({ value: options.cwd ?? '/' }))
    on('agent.list', () => ({ value: [] }))
    on('ui.log', (_$, e) => {
        logs.push(e.text)

        return { value: undefined }
    })
    on('tool.call', () => ({ result: 'ran' }))
    for (const event of ['SessionStart', 'UserPromptSubmit', 'SubagentStart', 'SubagentStop', 'PostToolUse', 'PostToolUseFailure', 'SessionEnd'] as const) {
        on(`classic.${event}`, () => ({}))
    }
    const clock = mock.clock(on, { now: options.now ?? 1_000_000 })
    mock.env(on, options.env ?? {})

    return { files, runs, logs, clock }
}

// The kit types `$` per tool and event; these pass only the fields the
// guards read.
export type CallResult = { deny?: string, result?: unknown }

// A tool call made inside subagent `agentId` (the main thread when undefined).
export const callTool = ($: any, agentId: string | undefined, args: Record<string, unknown>): Promise<CallResult> =>
    $.tool.call({ ...args, ...(agentId ? { agentId } : {}) })

// What the engine reports as a subagent starts: its type and working directory.
export const startAgent = ($: any, agentId: string, agentType: string, cwd: string): Promise<unknown> =>
    $.classic.SubagentStart({ agent_id: agentId, agent_type: agentType, cwd })

// Each test session gets its own id, so module-level per-session state never
// leaks from one test into the next.
let sessions = 0
export const freshSession = (): string => `abcdefgh-${String(++sessions).padStart(4, '0')}-test`
