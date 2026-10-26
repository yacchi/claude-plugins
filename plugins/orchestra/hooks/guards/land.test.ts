import { describe, expect, test } from 'claude-code/testing'
import type { Io } from '../lib/io.ts'
import { checkLand } from './land.ts'

// The merge guard: an orchestra branch merged, rebased, reset, cherry-picked
// or pulled from the user's checkout is denied; the same verb inside an
// orchestra worktree, read-only verbs, quoted text and escape hatches are not.

type Options = {
    env?: Readonly<Record<string, string>>,
    enforcement?: Readonly<Record<string, unknown>>,
    configFails?: boolean,
    envThrows?: boolean,
}

// A repository at /repo (the user's checkout) and four linked worktrees:
// an orchestra runner tree, an adopted orchestra tree, and a plain feature tree.
const TREES = [
    { top: '/repo', gitDir: '/repo/.git', markers: [] },
    { top: '/repo-orchestra/a-prog', gitDir: '/repo/.git/worktrees/a-prog', markers: ['orchestra-baseline'] },
    { top: '/repo-adopted', gitDir: '/repo/.git/worktrees/adopted', markers: ['orchestra-adopted'] },
    { top: '/repo-feature', gitDir: '/repo/.git/worktrees/feature', markers: [] },
]

const fakeIo = (options: Options = {}): Io => {
    const dirs = new Set<string>(['/', '/repo/.git', '/repo/.git/worktrees'])
    const files = new Set<string>()
    for (const tree of TREES) {
        dirs.add(tree.top)
        dirs.add(tree.gitDir)
        for (const marker of tree.markers) {
            files.add(`${tree.gitDir}/${marker}`)
        }
    }

    const run = async (argv: readonly string[]) => {
        const out = (exitCode: number, stdout = '') => ({ exitCode, stdout, stderr: '', isStdoutTruncated: false, isStderrTruncated: false })
        if (argv[0] === 'git' && argv[3] === 'rev-parse') {
            const tree = TREES.find(t => t.top === argv[2])

            return tree ? out(0, `${tree.gitDir}\n/repo/.git\n${tree.top}\n`) : out(128)
        }
        if (argv[0] === 'sh' && argv[3] === 'agent-exec') {
            return options.configFails
                ? out(1)
                : out(0, JSON.stringify({ enforcement: options.enforcement ?? {} }))
        }

        return out(127)
    }

    return {
        env: async (name: string) => {
            if (options.envThrows) {
                throw new Error('env unavailable')
            }

            return options.env?.[name]
        },
        run,
        stat: async (path: string) => {
            if (!dirs.has(path)) {
                throw new Error(`ENOENT: ${path}`)
            }

            return { kind: 'dir', size: 0, mtimeMs: 0, isLink: false, realPath: path }
        },
        exists: async (path: string) => files.has(path),
    } as unknown as Io
}

const deny = (command: string, cwd = '/repo', description = 'x') =>
    checkLand(fakeIo(), { command, description }, cwd)

describe('merge-guard', () => {
    test('the table: orchestra branches are denied from the user checkout', async () => {
        expect(await deny('git merge --ff-only orchestra/52f1a299/prog-x')).toContain('(orchestra/52f1a299/prog-x) into /repo')
        expect(await deny('cd /repo && git merge orchestra/prog')).toBeDefined()
        expect(await deny('git -C /repo reset --hard refs/heads/orchestra/a/b', '/repo-feature')).toBeDefined()
        expect(await deny('git cherry-pick orchestra/a/b~3..orchestra/a/b')).toBeDefined()
    })

    test('the table: read-only verbs, other branches and quoted text are allowed', async () => {
        expect(await deny('git log orchestra/a/b')).toBeUndefined()
        expect(await deny('git diff main...orchestra/a/b')).toBeUndefined()
        expect(await deny('git merge feature/orchestra-ui')).toBeUndefined()
        expect(await deny('echo "git merge orchestra/x"')).toBeUndefined()
        expect(await deny('cat <<EOF\ngit merge orchestra/x\nEOF')).toBeUndefined()
        expect(await deny('git -C /repo-orchestra/a-prog merge orchestra/a/other')).toBeUndefined()
    })

    test('the other verbs and branch spellings are denied', async () => {
        expect(await deny('git rebase orchestra/a/b')).toBeDefined()
        expect(await deny('git pull origin orchestra/a/b')).toBeDefined()
        expect(await deny('git pull origin origin/orchestra/a/b')).toBeDefined()
        expect(await deny('git merge refs/heads/orchestra/a/b')).toBeDefined()
        expect(await deny('git checkout orchestra/a/b')).toBeUndefined()
    })

    test('the directory: cd into an orchestra worktree allows, cd back to the checkout denies', async () => {
        expect(await deny('cd /repo-orchestra/a-prog && git merge orchestra/a/other')).toBeUndefined()
        expect(await deny('cd /repo-orchestra/a-prog && cd /repo && git merge orchestra/x')).toBeDefined()
        expect(await deny('cd /repo-orchestra/a-prog && git -C /repo merge orchestra/x')).toBeDefined()
    })

    test('the directory: -C is relative to a preceding cd', async () => {
        expect(await deny('cd /repo-orchestra && git -C a-prog merge orchestra/x')).toBeUndefined()
        expect(await deny('cd /repo && git -C ../repo-orchestra/a-prog merge orchestra/x')).toBeUndefined()
    })

    test('the directory: a cd through a variable is not followed, so the cwd applies', async () => {
        expect(await deny('cd $WT && git merge orchestra/x', '/repo')).toBeDefined()
        expect(await deny('cd $WT && git merge orchestra/x', '/repo-orchestra/a-prog')).toBeUndefined()
    })

    test('the directory: a linked worktree needs an orchestra marker; an adopted one counts', async () => {
        expect(await deny('git merge orchestra/x', '/repo-feature')).toBeDefined()
        expect(await deny('git merge orchestra/x', '/repo-adopted')).toBeUndefined()
    })

    test('escape hatches: the marker in the command or its description', async () => {
        expect(await deny('git merge orchestra/x [orchestra:allow-orchestra-merge: user asked]')).toBeUndefined()
        expect(await deny('git merge orchestra/x', '/repo', 'land it [orchestra:allow-orchestra-merge: user asked]')).toBeUndefined()
    })

    test('config and env: enforcement.orchestra_merge off, ORCHESTRA_ENFORCEMENT=off, and a failed lookup allow', async () => {
        const command = 'git merge orchestra/x'
        const input = { command, description: 'x' }
        expect(await checkLand(fakeIo({ enforcement: { orchestra_merge: 'off' } }), input, '/repo'), 'config off').toBeUndefined()
        expect(await checkLand(fakeIo({ enforcement: { orchestra_merge: false } }), input, '/repo'), 'config false').toBeUndefined()
        expect(await checkLand(fakeIo({ env: { ORCHESTRA_ENFORCEMENT: 'off' } }), input, '/repo'), 'env off').toBeUndefined()
        expect(await checkLand(fakeIo({ configFails: true }), input, '/repo'), 'failed lookup fails open').toBeUndefined()
        expect(await checkLand(fakeIo({ enforcement: {} }), input, '/repo'), 'absent means on').toBeDefined()
        expect(await checkLand(fakeIo({ enforcement: { orchestra_merge: 'on' } }), input, '/repo'), 'explicit on').toBeDefined()
    })

    test('variable operands and xargs: the ref a loop, a quote or a substitution names is denied', async () => {
        expect(await deny('for b in worktree-agent-a1 worktree-agent-b2; do git merge -q --no-edit $b; done')).toContain('do not merge an Agent worktree branch')
        expect(await deny('while read b; do git merge "$b"; done <<< "worktree-wf_61f2-1"')).toBeDefined()
        expect(await deny("git branch --list 'worktree-*' | xargs -n 1 git merge")).toBeDefined()
        expect(await deny("for b in $(git branch --list 'orchestra/*'); do git merge --ff-only $b; done")).toContain('do not merge an orchestra branch')
    })

    test('Agent worktree branches: only --ff-only merges and cherry-pick, rebase and merge-base pass', async () => {
        expect(await deny('git merge worktree-agent-a1')).toBeDefined()
        expect(await deny('git merge --no-ff worktree-agent-a1')).toBeDefined()
        expect(await deny('git merge --squash worktree-agent-a1')).toBeDefined()
        expect(await deny('git pull . worktree-agent-a1')).toBeDefined()
        expect(await deny('git merge --ff-only worktree-agent-a1')).toBeUndefined()
        expect(await deny('git cherry-pick worktree-agent-a1')).toBeUndefined()
        expect(await deny('git rebase worktree-agent-a1')).toBeUndefined()
        expect(await deny('git merge-base HEAD worktree-agent-a1')).toBeUndefined()
        expect(await deny('git -C /repo-orchestra/a-prog merge worktree-agent-a1')).toBeUndefined()
    })

    test('moving verbs: push, update-ref, creating branch, checkout -b and switch -c name the ref', async () => {
        expect(await deny('git push origin orchestra/a/b')).toBeDefined()
        expect(await deny('git push origin orchestra/a/b:feature/y')).toBeDefined()
        expect(await deny('git push origin orchestra/a/b', '/repo-orchestra/a-prog')).toBeDefined()
        expect(await deny('git update-ref refs/heads/feature/x orchestra/a/b')).toBeDefined()
        expect(await deny('git branch pr orchestra/a/b')).toBeDefined()
        expect(await deny('git branch -f main orchestra/a/b')).toBeDefined()
        expect(await deny('git checkout -b pr orchestra/a/b')).toBeDefined()
        expect(await deny('git switch -c pr orchestra/a/b')).toBeDefined()
    })

    test('listings, deletions and pathspecs after -- are not moves', async () => {
        expect(await deny('git branch --list orchestra/*')).toBeUndefined()
        expect(await deny('git branch -D orchestra/a/b')).toBeUndefined()
        expect(await deny('git push origin --delete orchestra/a/b')).toBeUndefined()
        expect(await deny('git update-ref -d refs/heads/orchestra/a/b')).toBeUndefined()
        expect(await deny('git reset -- orchestra/notes.md')).toBeUndefined()
    })

    test('any error allows', async () => {
        expect(await checkLand(fakeIo({ envThrows: true }), { command: 'git merge orchestra/x' }, '/repo')).toBeUndefined()
    })
})
