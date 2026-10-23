import { describe, expect, test } from 'claude-code/testing'
import { callTool, fakeHost, freshSession, startAgent } from '../testing/host.ts'

// With pnpm's global virtual store a worktree's node_modules entries are
// links into one store every tree shares: a worker editing one changes it
// for everyone. Pinned: a worker's structured edit under a node_modules
// segment is denied, a lookalike name is not, and the instructor's main
// thread is never touched.

const TREE = '/work/task-a'

describe('dependency-edit', () => {
    test('denies a worker subagent editing under node_modules', async ($, on) => {
        fakeHost(on, { sessionId: freshSession(), dirs: [TREE] })
        await startAgent($, 'ag_1', 'orchestra:orchestra-light', TREE)
        const cases = [
            { tool: 'Edit', file_path: `${TREE}/node_modules/.pnpm/is-number@7.0.0/node_modules/is-number/index.js` },
            { tool: 'Write', file_path: 'packages/a/node_modules/b/c.js' },
            { tool: 'MultiEdit', file_path: '/store/v11/links/@/x/1.0.0/abc/node_modules/x/index.js' },
            { tool: 'NotebookEdit', notebook_path: `${TREE}/node_modules/n/a.ipynb` },
        ]
        for (const args of cases) {
            expect((await callTool($, 'ag_1', args)).deny, JSON.stringify(args)).toContain('installed dependency')
        }
    })

    test('lets lookalike names and paths that leave node_modules through', async ($, on) => {
        fakeHost(on, { sessionId: freshSession(), dirs: [TREE] })
        await startAgent($, 'ag_1', 'orchestra:orchestra-light', TREE)
        const paths = [
            `${TREE}/src/my_node_modules/x.ts`,
            `${TREE}/node_modules_shim.ts`,
            `${TREE}/types/node_modules.d.ts`,
            'node_modules/../src/a.ts',
        ]
        for (const file_path of paths) {
            expect(await callTool($, 'ag_1', { tool: 'Edit', file_path }), file_path).toEqual({ result: 'ran' })
        }
    })

    test('denies the main thread of a session a wave hosts', async ($, on) => {
        const sessionId = freshSession()
        fakeHost(on, {
            sessionId,
            cwd: TREE,
            repos: [{ main: '/repo', linked: [{ path: TREE, branch: 'orchestra/task-a' }] }],
            files: { '/repo/.git/worktrees/task-a/orchestra-baseline': 'base\n' },
        })
        await $.classic.SessionStart({ session_id: sessionId, source: 'startup', cwd: TREE })
        const r = await callTool($, undefined, { tool: 'Write', file_path: 'node_modules/x/index.js' })
        expect(r.deny).toContain('installed dependency')
    })

    test('never blocks the instructor\'s main thread', async ($, on) => {
        fakeHost(on, { sessionId: freshSession(), dirs: [TREE] })
        const r = await callTool($, undefined, { tool: 'Edit', file_path: `${TREE}/node_modules/x/index.js` })
        expect(r).toEqual({ result: 'ran' })
    })
})
