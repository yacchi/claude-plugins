import { describe, expect, test } from 'claude-code/testing'
import type { On } from 'claude-code'
import {
    beginTurn,
    callTool,
    fakeHost,
    finishTurn,
    freshSession,
    type HostOptions,
    submitPrompt,
} from '../testing/host.ts'

// Orchestration agents need the user's opt-in (a typed /orchestra:run, or a
// request for a workflow) or an answer to the one ask. Pinned through the
// engine: what is guarded, what opts in, what counts as the answer, and the
// fail-open paths.

const optInHost = (on: On, extra: Partial<HostOptions> = {}) => {
    const sessionId = freshSession()

    return { sessionId, ...fakeHost(on, { sessionId, model: 'claude-opus-5', ...extra }) }
}

const optInSpawn = ($: unknown, subagentType: string | undefined, agentId?: string) =>
    callTool($, agentId, {
        tool: 'Agent',
        description: 'do it',
        prompt: 'implement the fix',
        model: 'sonnet',
        ...(subagentType === undefined ? {} : { subagent_type: subagentType }),
    })

const optInSkill = ($: unknown) => callTool($, undefined, { tool: 'Skill', skill: 'orchestra:run' })

describe('opt-in', () => {
    test('no opt-in: work agents denied, Explore allowed, a generic agent only in the skill turn', async ($, on) => {
        optInHost(on)
        await submitPrompt($, 'fix these three bugs')
        await beginTurn($, 't1')
        const planner = await optInSpawn($, 'orchestra:orchestra-planner')
        expect(planner.deny).toContain('no opt-in for orchestrated work')
        expect(planner.deny).toContain('orchestra:orchestra-planner')
        expect((await optInSpawn($, 'orchestra-light')).deny, 'repeats within the turn').toBeDefined()
        expect((await optInSpawn($, 'Explore')).deny).toBeUndefined()
        expect((await optInSpawn($, 'general-purpose')).deny, 'generic before the skill').toBeUndefined()
        await optInSkill($)
        expect((await optInSpawn($, undefined)).deny, 'generic in the skill turn').toContain('a generic agent')
        await beginTurn($, 't2')
        expect((await optInSpawn($, 'general-purpose')).deny, 'generic in the next turn').toBeUndefined()
    })

    test('a typed /orchestra:run opts in for the session', async ($, on) => {
        optInHost(on)
        await submitPrompt($, '/orchestra:run x', { command: { name: 'orchestra:run', args: 'x' } })
        await beginTurn($, 't1')
        await optInSkill($)
        expect((await optInSpawn($, 'orchestra:orchestra-planner')).deny).toBeUndefined()
        expect((await optInSpawn($, 'general-purpose')).deny).toBeUndefined()
        await submitPrompt($, 'and another thing')
        await beginTurn($, 't2')
        expect((await optInSpawn($, 'orchestra-deep')).deny, 'sticky').toBeUndefined()
    })

    test('asking for a workflow opts in', async ($, on) => {
        optInHost(on)
        await submitPrompt($, 'use a workflow for this')
        expect((await optInSpawn($, 'orchestra-light')).deny).toBeUndefined()
    })

    test('a notification or meta prompt neither opts in nor answers the ask', async ($, on) => {
        optInHost(on)
        await submitPrompt($, 'fix it')
        expect((await optInSpawn($, 'orchestra-light')).deny).toBeDefined()
        await submitPrompt($, 'use a workflow', { origin: { kind: 'task-notification' } })
        expect((await optInSpawn($, 'orchestra-light')).deny, 'task-notification').toBeDefined()
        await submitPrompt($, 'use a workflow', { isMeta: true })
        expect((await optInSpawn($, 'orchestra-light')).deny, 'isMeta').toBeDefined()
    })

    test('the next user prompt after a deny unlocks, whatever it says', async ($, on) => {
        optInHost(on)
        await submitPrompt($, 'fix it')
        expect((await optInSpawn($, 'orchestra-light')).deny).toBeDefined()
        await submitPrompt($, 'no, just do it yourself')
        expect((await optInSpawn($, 'orchestra-light')).deny).toBeUndefined()
    })

    test('an answer naming /orchestra:run counts as the ask', async ($, on) => {
        optInHost(on)
        await submitPrompt($, 'fix it')
        await beginTurn($, 't1')
        await finishTurn($, 't1', 'Type `/orchestra:run fix it` to orchestrate this.')
        await submitPrompt($, 'ok go')
        expect((await optInSpawn($, 'orchestra-light')).deny).toBeUndefined()
    })

    test('a typed command in the conversation opts in on resume', async ($, on) => {
        optInHost(on, {
            messages: [{ role: 'user', text: '<command-name>/orchestra:run</command-name>\nfix it', toolUses: [] }],
        })
        await submitPrompt($, 'continue')
        expect((await optInSpawn($, 'orchestra-light')).deny).toBeUndefined()
    })

    const allowed: [string, Partial<HostOptions>, boolean][] = [
        ['cheap verdict', { model: 'claude-sonnet-5' }, true],
        ['enforcement.opt_in off', { agentExec: () => ({ exitCode: 0, stdout: '{"enforcement": {"opt_in": "off"}}' }) }, true],
        ['ORCHESTRA_ENFORCEMENT=off', { env: { ORCHESTRA_ENFORCEMENT: 'off' } }, true],
        ['zero prompts seen', {}, false],
    ]
    for (const [name, extra, prompt] of allowed) {
        test(`allowed: ${name}`, async ($, on) => {
            optInHost(on, extra)
            if (prompt) {
                await submitPrompt($, 'fix it')
            }
            expect((await optInSpawn($, 'orchestra-light')).deny).toBeUndefined()
        })
    }

    test('allowed: a subagent caller', async ($, on) => {
        optInHost(on)
        await submitPrompt($, 'fix it')
        expect((await optInSpawn($, 'orchestra-light', 'ag_1')).deny).toBeUndefined()
    })

    test('the merge guard denies a main-thread fast-forward of an orchestra branch', async ($, on) => {
        optInHost(on, { cwd: '/repo', repos: [{ main: '/repo' }] })
        const r = await callTool($, undefined, { tool: 'Bash', command: 'git merge --ff-only orchestra/x/prog' })
        expect(r.deny).toContain('agent-exec isolate land')
    })
})
