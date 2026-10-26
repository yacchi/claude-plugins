import { describe, expect, test } from 'claude-code/testing'
import type { On } from 'claude-code'
import { beginTurn, callTool, fakeHost, freshSession, type HostOptions } from '../testing/host.ts'

// The turn tripwire re-asks for a route at the threshold and, firmer, at
// twice it, once each per main-loop turn, on the edit's own result. It stays
// quiet on cheap sessions and in hosted task worktrees.

const host = (on: On, extra: Partial<HostOptions> = {}) => {
    const sessionId = freshSession()

    return { sessionId, ...fakeHost(on, { sessionId, model: 'claude-opus-5', ...extra }) }
}

// Every edit names a file of its own unless `file` is given.
let fileSeq = 0

// The nudges handed back over `n` edits.
const edits = async ($: any, n: number, tool = 'Edit', agentId?: string, file?: string) => {
    const said: string[] = []
    for (let i = 0; i < n; i++) {
        const path = tool === 'NotebookEdit' ? 'notebook_path' : 'file_path'
        const r = await callTool($, agentId, { tool, [path]: file ?? `/tmp/f${fileSeq++}.ts`, toolUseId: `t${i}` }) as { context?: string[] }
        said.push(...(r.context ?? []))
    }

    return said
}

describe('router: turn edits', () => {
    test('stage 1 at the threshold, stage 2 at twice it, once each per turn', async ($, on) => {
        host(on)
        await beginTurn($, 't1')
        expect(await edits($, 7), 'below the threshold').toEqual([])
        const stage1 = await edits($, 1)
        expect(stage1.length).toBe(1)
        expect(stage1[0]).toContain('hand-edited 8 files')
        expect(stage1[0]).toContain('a firmer request follows')
        expect(await edits($, 7), 'between the stages').toEqual([])
        const stage2 = await edits($, 1)
        expect(stage2[0]).toContain('hand-edited 16 files')
        expect(stage2[0]).toContain('exemption stops applying')
        expect(await edits($, 5), 'stage 2 once per turn').toEqual([])
        await beginTurn($, 't2')
        expect(await edits($, 7), 'the counter resets on a new turn').toEqual([])
        expect((await edits($, 9)).length, 'a new turn nudges again').toBe(2)
    })

    test('counts distinct files, not calls', async ($, on) => {
        host(on)
        await beginTurn($, 't1')
        expect(await edits($, 20, 'Edit', undefined, '/repo/a.ts'), 'one file, many edits').toEqual([])
        expect(await edits($, 6), 'six more files: seven in all').toEqual([])
        expect((await edits($, 1)).length, 'the eighth file').toBe(1)
        expect(await edits($, 1, 'Edit', undefined, '/repo/a.ts'), 'a repeat of a counted file').toEqual([])
    })

    test('every edit tool counts; reads and subagent turns do not', async ($, on) => {
        host(on)
        await beginTurn($, 't1')
        await beginTurn($, 'sub', 'ag_1')
        expect(await edits($, 9, 'Read'), 'reads').toEqual([])
        expect(await edits($, 9, 'Edit', 'ag_1'), 'a subagent\'s edits').toEqual([])
        await edits($, 4)
        await edits($, 1, 'Write')
        await edits($, 1, 'MultiEdit')
        expect((await edits($, 2, 'NotebookEdit')).length, 'every edit tool counts, in the main turn').toBe(1)
    })

    test('no turn.start seen, no turn to count', async ($, on) => {
        host(on)
        expect(await edits($, 9)).toEqual([])
    })

    test('enforcement.turn_edits moves the threshold', async ($, on) => {
        host(on, { agentExec: () => ({ exitCode: 0, stdout: '{"enforcement": {"turn_edits": 3}}' }) })
        await beginTurn($, 't1')
        expect((await edits($, 3)).length).toBe(1)
    })

    test('enforcement.turn_edits=off', async ($, on) => {
        host(on, { agentExec: () => ({ exitCode: 0, stdout: '{"enforcement": {"turn_edits": "off"}}' }) })
        await beginTurn($, 't1')
        expect(await edits($, 10)).toEqual([])
    })

    test('kill switch ORCHESTRA_TURN_GUARD=off', async ($, on) => {
        host(on, { env: { ORCHESTRA_TURN_GUARD: 'off' } })
        await beginTurn($, 't1')
        expect(await edits($, 10)).toEqual([])
    })

    test('the module heartbeat is stamped once per session', async ($, on) => {
        const { sessionId, runs } = host(on)
        await beginTurn($, 't1')
        await beginTurn($, 'sub', 'ag_1')
        await beginTurn($, 't2')
        await edits($, 1)
        const seen = runs.filter(argv => argv.slice(4, 6).join(' ') === 'hook seen')
        expect(seen.map(argv => argv.slice(4))).toEqual([['hook', 'seen', '--session', sessionId, '--event', 'module']])
    })
})

// Eight edits in one turn: whether the session's verdict lets the nudge out.
const nudged = async ($: any): Promise<boolean> => {
    await beginTurn($, 't1')

    return (await edits($, 8)).length === 1
}

describe('router: verdict', () => {
    test('a cheap model is not nudged', async ($, on) => {
        host(on, { model: 'claude-sonnet-5' })
        expect(await nudged($)).toBe(false)
    })

    test('an unknown model is nudged', async ($, on) => {
        host(on, { model: '' })
        expect(await nudged($)).toBe(true)
    })

    test('an unknown verdict is asked again once the model is known', async ($, on) => {
        const { sessionId } = host(on, { model: '' })
        await beginTurn($, 't1')
        await edits($, 1)
        await $.classic.SessionStart({ session_id: sessionId, source: 'startup', model: 'claude-haiku-5' })
        expect(await edits($, 9), 'cheap once announced').toEqual([])
    })
})

// A repository with one main tree and linked task trees; the task trees carry
// their orchestra markers in their own git dirs.
const TASKS = {
    '/repo/.git/worktrees/task-a/orchestra-baseline': 'base\n',
    '/repo/.git/worktrees/task-c/orchestra-adopted': 'adopted\n',
}
const tasks = (on: On, extra: Partial<HostOptions> = {}) => host(on, {
    repos: [{
        main: '/repo',
        linked: [
            { path: '/work/task-a', branch: 'orchestra/task-a' },
            { path: '/work/task-b', branch: 'orchestra/task-b' },
            { path: '/work/task-c', branch: 'orchestra/task-c' },
        ],
    }],
    files: TASKS,
    ...extra,
})

describe('router: hosted workers', () => {
    for (const model of ['claude-opus-5', 'claude-sonnet-5', '']) {
        test(`a task worktree is a worker on any model, and is not nudged: ${model || 'no model'}`, async ($, on) => {
            tasks(on, { cwd: '/work/task-a', model })
            expect(await nudged($)).toBe(false)
        })
    }

    test('the session root decides, falling back to the session cwd when blank', async ($, on) => {
        tasks(on, { cwd: '/work/task-a', root: '' })
        expect(await nudged($), 'blank root: session cwd').toBe(false)
    })

    test('a main-tree root wins over a task-tree cwd', async ($, on) => {
        tasks(on, { cwd: '/work/task-a', root: '/repo' })
        expect(await nudged($)).toBe(true)
    })

    test('the main worktree is never a worker, even with a stray marker', async ($, on) => {
        host(on, {
            repos: [{ main: '/repo' }],
            files: { '/repo/.git/orchestra-baseline': 'base\n' },
            cwd: '/repo',
        })
        expect(await nudged($)).toBe(true)
    })

    test('an integration tree is not a worker', async ($, on) => {
        tasks(on, {
            cwd: '/work/task-a',
            files: { ...TASKS, '/repo/.git/worktrees/task-a/orchestra-role': 'integration\n' },
        })
        expect(await nudged($)).toBe(true)
    })

    test('a linked tree without a task marker is not a worker', async ($, on) => {
        tasks(on, { cwd: '/work/task-b' })
        expect(await nudged($)).toBe(true)
    })

    test('a linked tree with the adopted marker is a worker', async ($, on) => {
        tasks(on, { cwd: '/work/task-c' })
        expect(await nudged($)).toBe(false)
    })

    test('outside any repository the session is not a worker', async ($, on) => {
        host(on, { cwd: '/nowhere' })
        expect(await nudged($)).toBe(true)
    })

    test('with git failing the session is not a worker', async ($, on) => {
        tasks(on, { cwd: '/nowhere/else' })
        expect(await nudged($)).toBe(true)
    })
})
