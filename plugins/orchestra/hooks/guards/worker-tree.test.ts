import { describe, expect, test } from 'claude-code/testing'
import type { On } from 'claude-code'
import { callTool, fakeHost, freshSession, startAgent, type HostOptions } from '../testing/host.ts'

// A worker with its own orchestra worktree writing into the user's MAIN tree
// anyway (observed: a Sonnet worker told "cd there FIRST" edited the main
// tree's files). Pinned: structured tools are caught airtight, the main
// thread is never touched, a worker writes freely in its own worktree (one
// nested under the main tree, as Agent isolation places it, included), the
// path traps (symlink, `..`, sibling prefix) resolve, and doubt fails open.

const BASE = '/w'
const MAIN = `${BASE}/repo`
const ISO = `${BASE}/iso`
const SIBLING = `${BASE}/repo-worktrees/x`
const NESTED = `${MAIN}/.claude/worktrees/agent-x`
const SOLO = `${BASE}/solo`
const NON_REPO = `${BASE}/plain`

const host = (on: On, extra: Partial<HostOptions> = {}) => fakeHost(on, {
    sessionId: freshSession(),
    dirs: [`${MAIN}/src`, `${ISO}/src`, SIBLING, NON_REPO],
    files: { [`${MAIN}/src/app.ts`]: 'one', [`${ISO}/src/app.ts`]: 'one', [`${ISO}/a.ts`]: 'a' },
    links: { [`${BASE}/link`]: MAIN },
    repos: [
        { main: MAIN, linked: [
            { path: ISO, branch: 'orchestra/abcdefgh/t1' },
            { path: NESTED, branch: 'worktree-agent-x' },
        ] },
        { main: SOLO },
    ],
    ...extra,
})

const AGENT = 'orchestra:orchestra-standard'

const edit = async ($: unknown, cwd: string, path: string, tool = 'Edit', agentType = AGENT) => {
    await startAgent($, 'ag_1', agentType, cwd)

    return callTool($, 'ag_1', { tool, [tool === 'NotebookEdit' ? 'notebook_path' : 'file_path']: path })
}

const bash = async ($: unknown, command: string, description = 'do work') => {
    await startAgent($, 'ag_1', AGENT, ISO)

    return callTool($, 'ag_1', { tool: 'Bash', command, description })
}

describe('worker-tree', () => {
    test('the observed incident: structured writes into the main tree', async ($, on) => {
        host(on)
        const r = await edit($, ISO, `${MAIN}/src/app.ts`)
        expect(r.deny).toContain(`MAIN working tree ('${MAIN}/src/app.ts')`)
        expect((await edit($, ISO, `${MAIN}/src/new.ts`, 'Write')).deny).toBeDefined()
        expect((await edit($, ISO, `${MAIN}/src/app.ts`, 'MultiEdit')).deny).toBeDefined()
        expect((await edit($, ISO, `${MAIN}/nb.ipynb`, 'NotebookEdit')).deny, 'notebook_path').toBeDefined()
        expect((await edit($, MAIN, `${MAIN}/src/app.ts`)).deny, 'cwd in the main tree').toBeDefined()
        expect((await edit($, ISO, `${MAIN}/src/app.ts`, 'Edit', 'general-purpose')).deny, 'generic subagent').toBeDefined()
    })

    test('path traps resolve: relative, symlink, .., sibling prefix', async ($, on) => {
        host(on)
        expect((await edit($, MAIN, 'src/app.ts')).deny, 'relative against cwd').toBeDefined()
        expect((await edit($, ISO, `${BASE}/link/src/app.ts`)).deny, 'symlink into the main tree').toBeDefined()
        expect((await edit($, ISO, '../repo/src/app.ts')).deny, '.. escape').toBeDefined()
        expect((await edit($, ISO, `${SIBLING}/notes.md`)).deny, '/repo-worktrees/x is not inside /repo').toBeUndefined()
        expect((await edit($, ISO, 'src/app.ts')).deny, 'relative inside its own worktree').toBeUndefined()
        expect((await edit($, ISO, `${ISO}/src/app.ts`)).deny, 'its own worktree').toBeUndefined()
    })

    test('a worktree nested under the main tree is the worker\'s, whatever its branch', async ($, on) => {
        host(on)
        expect((await edit($, NESTED, `${NESTED}/file.ts`, 'Write')).deny, 'its own nested worktree').toBeUndefined()
        expect((await edit($, NESTED, 'file.ts', 'Write')).deny, 'relative inside it').toBeUndefined()
        expect((await callTool($, 'ag_1', { tool: 'Bash', command: `echo x > ${NESTED}/file.ts`, description: 'do work' })).deny, 'Bash redirect').toBeUndefined()
        expect((await edit($, NESTED, `${MAIN}/src/app.ts`, 'Write')).deny, 'the main checkout itself').toBeDefined()
        expect((await edit($, NESTED, '../../../src/app.ts', 'Write')).deny, '.. out of it into the main tree').toBeDefined()
    })

    test('scope: main thread, express lane, other sessions, supervisor', async ($, on) => {
        host(on)
        expect((await callTool($, undefined, { tool: 'Edit', file_path: `${MAIN}/src/app.ts` })).deny, 'main thread').toBeUndefined()
        expect((await callTool($, undefined, { tool: 'Bash', command: `echo x > ${MAIN}/src/app.ts` })).deny, 'main thread').toBeUndefined()
        expect((await edit($, SOLO, `${SOLO}/README.md`)).deny, 'no orchestra worktree').toBeUndefined()
        expect((await edit($, ISO, `${MAIN}/src/app.ts`, 'Edit', 'orchestra:orchestra-delegate')).deny, 'supervisor').toBeUndefined()
        expect((await edit($, NON_REPO, `${NON_REPO}/a.txt`)).deny, 'outside a repository').toBeUndefined()
        expect((await edit($, ISO, '', 'Write')).deny, 'no target').toBeUndefined()
        expect((await edit($, ISO, `${MAIN}/src/app.ts`, 'Read')).deny, 'reads').toBeUndefined()
    })

    test('worktrees of another session do not count', async ($, on) => {
        host(on, { sessionId: 'zzzzzzzz-9999' })
        expect((await edit($, ISO, `${MAIN}/src/app.ts`)).deny).toBeUndefined()
    })

    test('Bash: the three write shapes, and only those', async ($, on) => {
        host(on)
        const denied = [
            `echo x > ${MAIN}/src/app.ts`,
            `echo x >> ${MAIN}/src/app.ts`,
            `echo x | tee -a ${MAIN}/src/app.ts`,
            `sed -i -e s/one/two/ ${MAIN}/src/app.ts`,
            `sed -i s/one/two/ ${MAIN}/src/app.ts`,
            `npm test; echo done > ${MAIN}/log.txt`,
        ]
        for (const command of denied) {
            expect((await bash($, command)).deny, command).toBeDefined()
        }
        const allowed = [
            'echo x > $WORKTREE/file',
            `echo x > ${ISO}/src/app.ts`,
            `sed -i -e s/one/two/ ${ISO}/src/app.ts`,
            `cat ${MAIN}/src/app.ts | grep x`,
            `cd ${MAIN} && npm test 2>&1`,
            `cp ${ISO}/a.ts ${MAIN}/a.ts`,
            `cat > ${ISO}/task.md <<EOF\nWrite the result to ${MAIN}/out.txt when done.\nEOF`,
            `echo "never write > ${MAIN}/src/app.ts"`,
        ]
        for (const command of allowed) {
            expect((await bash($, command)).deny, command).toBeUndefined()
        }
        expect((await bash($, `echo x > ${MAIN}/src/app.ts [orchestra:allow-tree: user asked]`)).deny).toBeUndefined()
        expect((await bash($, `echo x > ${MAIN}/src/app.ts`, 'patch [orchestra:allow-tree: deliberate]')).deny).toBeUndefined()
    })

    test('follows the cwd each of the worker\'s tool calls reports', async ($, on) => {
        host(on)
        await startAgent($, 'ag_2', AGENT, SOLO)
        const write = { tool: 'Edit', file_path: `${MAIN}/src/app.ts` }
        expect((await callTool($, 'ag_2', write)).deny, 'from a repo with no orchestra worktree').toBeUndefined()
        await ($ as any).classic.PostToolUse({
            agent_id: 'ag_2', agent_type: AGENT, cwd: ISO,
            tool_name: 'Bash', tool_input: { command: `cd ${ISO}` }, tool_response: {}, tool_use_id: 't1',
        })
        expect((await callTool($, 'ag_2', write)).deny, 'after cd-ing into its worktree').toBeDefined()
    })

    test('kill switch: ORCHESTRA_TREE_GUARD=off', async ($, on) => {
        host(on, { env: { ORCHESTRA_TREE_GUARD: 'off' } })
        expect((await edit($, ISO, `${MAIN}/src/app.ts`)).deny).toBeUndefined()
    })

    test('config: enforcement.worker_tree=off', async ($, on) => {
        host(on, { agentExec: () => ({ exitCode: 0, stdout: '{"enforcement": {"worker_tree": "off"}}' }) })
        expect((await edit($, ISO, `${MAIN}/src/app.ts`)).deny).toBeUndefined()
    })

    test('a failed config lookup fails open and is asked again next time', async ($, on) => {
        let failing = true
        host(on, { agentExec: () => (failing ? { exitCode: 1 } : { exitCode: 0, stdout: '{}' }) })
        expect((await edit($, ISO, `${MAIN}/src/app.ts`)).deny).toBeUndefined()
        failing = false
        expect((await edit($, ISO, `${MAIN}/src/app.ts`)).deny).toBeDefined()
    })
})
