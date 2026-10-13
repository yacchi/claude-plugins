import { describe, expect, test } from 'claude-code/testing'
import type { On } from 'claude-code'
import { callTool, fakeHost, freshSession, startAgent, type HostOptions } from '../testing/host.ts'

// One writer per worktree: a message to a running workflow agent resumed a
// COPY of it, and both committed the same files seconds apart. Pinned: the
// second writer is denied (edits, commits, `cd <tree> && git ...`), reads and
// tests never need the lease, SubagentStop and the window both free it, the
// main worktree is never leased, and the lease stays out of the tree.

const REPO = '/w/repo'
const WT1 = '/w/wt1'
const WT2 = '/w/wt2'
const LEASE = `${REPO}/.git/worktrees/wt1/orchestra-lease.json`

const host = (on: On, extra: Partial<HostOptions> = {}) => fakeHost(on, {
    sessionId: freshSession(),
    dirs: ['/w/scratch'],
    repos: [{ main: REPO, linked: [{ path: WT1, branch: 'orchestra/s1/t1' }, { path: WT2, branch: 'feature/any' }] }],
    ...extra,
})

const as = async ($: unknown, agentId: string, agentType: string, cwd: string, args: Record<string, unknown>) => {
    await startAgent($, agentId, agentType, cwd)

    return callTool($, agentId, args)
}

const light = 'orchestra:orchestra-light'
const editIn = (path: string) => ({ tool: 'Edit', file_path: path })
const run = (command: string, description = 'x') => ({ tool: 'Bash', command, description })
const stop = ($: any, agentId: string) =>
    $.classic.SubagentStop({ agent_id: agentId, agent_type: 'x', stop_hook_active: false, agent_transcript_path: '' })

describe('worktree-lease', () => {
    test('a second writer is denied until the holder stops', async ($, on) => {
        const h = host(on)
        expect((await as($, 'ag_A', light, WT1, editIn(`${WT1}/a.ts`))).deny, 'first writer takes the lease').toBeUndefined()
        expect(JSON.parse(h.files.get(LEASE) ?? '{}').agent_id).toBe('ag_A')
        expect(h.files.has(`${LEASE}.tmp`), 'written via a temp file, then renamed').toBe(false)
        const dup = await as($, 'ag_B', light, WT1, editIn(`${WT1}/a.ts`))
        expect(dup.deny).toContain('DUPLICATE')
        expect(dup.deny).toContain(`the worktree ${WT1} is leased by ${light}`)
        expect((await as($, 'ag_B', light, WT1, run('git commit -qm dup'))).deny, 'its commit').toBeDefined()
        expect((await as($, 'ag_B', 'general-purpose', REPO, run(`cd ${WT1} && git add -A`))).deny, 'cd then git add').toBeDefined()
        expect((await as($, 'ag_A', light, WT1, editIn(`${WT1}/b.ts`))).deny, 'the holder keeps writing').toBeUndefined()
        expect((await as($, 'ag_B', 'orchestra:orchestra-review', WT1, run('git diff HEAD~1 && npm test'))).deny, 'reading and testing').toBeUndefined()
        expect((await as($, 'ag_B', light, WT1, run(`git -C ${WT1} status`))).deny, 'read-only git').toBeUndefined()

        await stop($, 'ag_A')
        expect(h.files.has(LEASE), 'SubagentStop drops the lease file').toBe(false)
        expect((await as($, 'ag_B', light, WT1, editIn(`${WT1}/a.ts`))).deny, 'the next round writes freely').toBeUndefined()
        await stop($, 'ag_nobody')
        expect(h.files.has(LEASE), 'releasing an agent with no leases touches nothing').toBe(true)
    })

    test('the window frees a lease whose holder died without SubagentStop', async ($, on) => {
        const h = host(on, { env: { ORCHESTRA_LEASE_WINDOW: '1' } })
        await as($, 'ag_B', light, WT1, editIn(`${WT1}/a.ts`))
        expect((await as($, 'ag_C', light, WT1, editIn(`${WT1}/a.ts`))).deny, 'active holder').toBeDefined()
        await h.clock.advance(2000)
        expect((await as($, 'ag_C', light, WT1, editIn(`${WT1}/a.ts`))).deny, 'silent past the window').toBeUndefined()
        expect((await as($, 'ag_B', light, WT1, editIn(`${WT1}/a.ts`))).deny, 'the old holder is now the duplicate').toBeDefined()
    })

    test('config: the window and the switch, read only when about to deny', async ($, on) => {
        let config = '{"enforcement": {"worktree_lease_window": 1}}'
        const h = host(on, { agentExec: () => ({ exitCode: 0, stdout: config }) })
        await as($, 'ag_A', light, WT1, editIn(`${WT1}/a.ts`))
        expect(h.runs.some(argv => argv.includes('config')), 'the common path asks nothing').toBe(false)
        await h.clock.advance(2000)
        expect((await as($, 'ag_B', light, WT1, editIn(`${WT1}/a.ts`))).deny, 'configured window').toBeUndefined()
        config = '{"enforcement": {"worktree_lease": "off"}}'
        expect((await as($, 'ag_C', light, WT1, editIn(`${WT1}/a.ts`))).deny, 'enforcement.worktree_lease=off').toBeUndefined()
    })

    test('scope: other worktrees, the main worktree, main thread, supervisor, escape', async ($, on) => {
        host(on)
        await as($, 'ag_A', light, WT1, editIn(`${WT1}/a.ts`))
        expect((await as($, 'ag_D', light, WT2, editIn(`${WT2}/x.ts`))).deny, 'each worktree has its own lease').toBeUndefined()
        expect((await as($, 'ag_E', light, REPO, editIn(`${REPO}/a.ts`))).deny).toBeUndefined()
        expect((await as($, 'ag_F', light, REPO, editIn(`${REPO}/b.ts`))).deny, 'the main worktree is never leased').toBeUndefined()
        expect((await callTool($, undefined, editIn(`${WT1}/a.ts`))).deny, 'main thread').toBeUndefined()
        expect((await as($, 'ag_G', 'orchestra:orchestra-delegate', WT1, run('git commit -qm integrate'))).deny, 'supervisor').toBeUndefined()
        expect((await as($, 'ag_G', light, WT1, editIn(`${WT1}/a.ts`))).deny, 'another worker').toBeDefined()
        expect((await as($, 'ag_G', light, WT1, run('git commit -qm x', 'take over [orchestra:allow-lease: holder confirmed dead]'))).deny).toBeUndefined()
        expect((await as($, 'ag_H', light, '/w/scratch', editIn('/w/scratch/notrepo/x.ts'))).deny, 'outside a repository').toBeUndefined()
    })

    test('kill switch: ORCHESTRA_LEASE_GUARD=off', async ($, on) => {
        host(on, { env: { ORCHESTRA_LEASE_GUARD: 'off' } })
        await as($, 'ag_A', light, WT1, editIn(`${WT1}/a.ts`))
        expect((await as($, 'ag_H', light, WT1, editIn(`${WT1}/a.ts`))).deny).toBeUndefined()
    })
})
