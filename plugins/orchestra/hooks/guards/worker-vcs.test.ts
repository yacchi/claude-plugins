import { describe, expect, test } from 'claude-code/testing'
import type { On } from 'claude-code'
import { callTool, fakeHost, freshSession, startAgent, type HostOptions } from '../testing/host.ts'

// A worker running `git checkout --` / `git restore` / `git reset --hard`
// against the user's shared tree: 35 such commands in 7 weeks, from every
// tier, despite three generations of prose. Pinned: it catches the real
// accident, never touches the main thread, lets an isolated worker manage its
// own worktree (but never the shared stash stack), and its hatches work.

const SHARED = '/w/repo'
const ISO = '/w/iso'
const NON_REPO = '/w/plain'

const host = (on: On, extra: Partial<HostOptions> = {}) => fakeHost(on, {
    sessionId: freshSession(),
    dirs: [NON_REPO],
    repos: [{ main: SHARED, linked: [{ path: ISO, branch: 'orchestra/abcdefgh/t1' }] }],
    ...extra,
})

const bash = async ($: unknown, cwd: string, command: string, description = 'do work', agentType = 'orchestra:orchestra-light') => {
    await startAgent($, 'ag_1', agentType, cwd)

    return callTool($, 'ag_1', { tool: 'Bash', command, description })
}

describe('worker-vcs', () => {
    test('denies destructive git on the shared tree', async ($, on) => {
        host(on)
        const cases = [
            'git checkout -- src/app.ts',
            'git restore src/a.ts src/b.ts',
            'git reset --hard HEAD',
            'git checkout .',
            'git clean -fd',
            'git stash',
            'git checkout --ours -- pkg/go.mod',
            'cd /tmp && npm test; git checkout -- .',
            `git -C ${SHARED} restore src/a.ts`,
            'git reset --keep origin/main',
            'git stash pop',
        ]
        for (const command of cases) {
            expect((await bash($, SHARED, command)).deny, command).toContain('orchestra:')
        }
        const r = await bash($, SHARED, 'git checkout -- go.sum', 'tidy up', 'general-purpose')
        expect(r.deny, 'a generic subagent is still a worker').toContain('branch \'main\'')
    })

    test('lets non-destructive git and mere mentions through', async ($, on) => {
        host(on)
        const cases = [
            'git status --porcelain',
            'git diff HEAD',
            'git checkout -b feature/x',
            'git reset HEAD src/a.ts',
            'git restore --staged src/a.ts',
            'git stash list',
            'npm test -- --watch=false',
            'cat > /tmp/task.txt <<EOF\nNever run git checkout -- or git reset --hard.\nEOF',
            'echo "do not use git reset --hard"',
        ]
        for (const command of cases) {
            expect((await bash($, SHARED, command)).deny, command).toBeUndefined()
        }
    })

    test('scope: main thread, own worktree, supervisor, non-repo, other tools', async ($, on) => {
        host(on)
        await startAgent($, 'ag_1', 'orchestra:orchestra-light', SHARED)
        expect((await callTool($, undefined, { tool: 'Bash', command: 'git checkout -- src/app.ts', description: 'x' })).deny,
            'the main thread resolving a conflict is doing its job').toBeUndefined()
        expect((await bash($, ISO, 'git checkout -- README.md')).deny, 'own worktree').toBeUndefined()
        expect((await bash($, ISO, 'git reset --hard HEAD')).deny, 'own worktree').toBeUndefined()
        expect((await bash($, SHARED, 'git reset --hard HEAD', 'roll back', 'orchestra:orchestra-delegate')).deny,
            'orchestra-delegate owns rollbacks').toBeUndefined()
        expect((await bash($, NON_REPO, 'git reset --hard HEAD')).deny, 'outside a repository').toBeUndefined()
        await startAgent($, 'ag_1', 'orchestra:orchestra-light', SHARED)
        expect((await callTool($, 'ag_1', { tool: 'Read', file_path: `${SHARED}/a`, command: 'git reset --hard' })).deny,
            'non-Bash tools are out of scope').toBeUndefined()
    })

    test('the shared stash stack is denied even in an orchestra worktree', async ($, on) => {
        host(on)
        const push = await bash($, ISO, 'git stash push -u -m wip', 'set aside')
        expect(push.deny).toContain('shared by EVERY worktree')
        expect(push.deny).toContain('agent-exec shelf push')
        expect((await bash($, ISO, 'git stash pop')).deny).toBeDefined()
        expect((await bash($, ISO, 'git stash apply stash@{0}')).deny).toBeDefined()
        expect((await bash($, ISO, 'git stash list')).deny, 'list only reads').toBeUndefined()
        expect((await bash($, ISO, 'git stash drop', 'x [orchestra:allow-vcs: my own entry]')).deny).toBeUndefined()
    })

    test('escape markers in the command or the description', async ($, on) => {
        host(on)
        expect((await bash($, SHARED, 'git checkout -- src/app.ts [orchestra:allow-vcs]')).deny).toBeUndefined()
        expect((await bash($, SHARED, 'git reset --hard HEAD', 'cleanup [orchestra:allow-vcs: user asked]')).deny).toBeUndefined()
    })

    test('kill switch: ORCHESTRA_VCS_GUARD=off', async ($, on) => {
        host(on, { env: { ORCHESTRA_VCS_GUARD: 'off' } })
        expect((await bash($, SHARED, 'git reset --hard HEAD')).deny).toBeUndefined()
    })

    test('config: enforcement.worker_vcs=off', async ($, on) => {
        host(on, { agentExec: () => ({ exitCode: 0, stdout: '{"enforcement": {"worker_vcs": "off"}}' }) })
        expect((await bash($, SHARED, 'git reset --hard HEAD')).deny).toBeUndefined()
    })

    test('blocks by default when agent-exec is not installed', async ($, on) => {
        host(on, { agentExec: () => 'missing' })
        expect((await bash($, SHARED, 'git reset --hard HEAD')).deny).toBeDefined()
    })
})
