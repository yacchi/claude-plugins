import { describe, expect, test } from 'claude-code/testing'
import { callTool, fakeHost, freshSession, startAgent } from '../testing/host.ts'
import { bashOp } from './trace.ts'

describe('bashOp', () => {
    test('names the program and, for tool front-ends, the subcommand', () => {
        const cases: Readonly<Record<string, string>> = {
            'git -C /x status --porcelain': 'git status',
            'cd /a && pnpm --filter x test': 'pnpm test',
            'FOO=1 timeout 300 /opt/homebrew/bin/pnpm install --offline': 'pnpm install',
            'rm -rf node_modules': 'rm',
            'ls': 'ls',
            '': 'bash',
            'git log --oneline | head -5': 'git log',
            'cd "/a b"; sudo -u root env X=1 go test ./...': 'go test',
            'time nice -n 5 cargo build': 'cargo build',
        }
        for (const [command, op] of Object.entries(cases)) {
            expect(bashOp(command), command).toBe(op)
        }
    })
})

describe('worker-bash trace', () => {
    const records = (runs: readonly string[][]) => runs
        .filter(r => r[0] === 'sh' && r[4] === 'trace' && r[5] === 'record')
        .map(r => JSON.parse(r[7]!) as Record<string, unknown>)

    test('a Bash call records exactly one worker-bash entry and returns the tool result', async ($, on) => {
        const h = fakeHost(on, { sessionId: freshSession() })
        const r = await callTool($, undefined, { tool: 'Bash', command: 'git -C /x status', description: 'look' })
        expect(r).toEqual({ result: 'ran' })
        await new Promise(resolve => setTimeout(resolve, 20))
        const found = records(h.runs)
        expect(found).toHaveLength(1)
        expect(found[0]).toMatchObject({ kind: 'worker-bash', op: 'git status', exit: 0 })
        expect(found[0]!.end as number).toBeGreaterThanOrEqual(found[0]!.at as number)
    })

    test('ORCHESTRA_TRACE=off records nothing', async ($, on) => {
        const h = fakeHost(on, { sessionId: freshSession(), env: { ORCHESTRA_TRACE: 'off' } })
        await callTool($, undefined, { tool: 'Bash', command: 'ls', description: 'look' })
        await new Promise(resolve => setTimeout(resolve, 20))
        expect(records(h.runs)).toHaveLength(0)
    })

    test('a denied call is not recorded', async ($, on) => {
        const h = fakeHost(on, { sessionId: freshSession(), repos: [{ main: '/w/repo' }] })
        await startAgent($, 'ag_1', 'orchestra:orchestra-light', '/w/repo')
        const r = await callTool($, 'ag_1', { tool: 'Bash', command: 'git reset --hard HEAD', description: 'undo' })
        expect(r.deny).toBeDefined()
        await new Promise(resolve => setTimeout(resolve, 20))
        expect(records(h.runs)).toHaveLength(0)
    })
})
