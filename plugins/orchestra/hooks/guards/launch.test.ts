import { describe, expect, test } from 'claude-code/testing'
import type { On } from 'claude-code'
import { callTool, fakeHost, freshSession, startAgent, type HostOptions } from '../testing/host.ts'

// The instructor launched `agent-exec wave run` (backgrounded) and
// `claude -p "... spawn orchestra-planner ..."` from its own Bash in one
// session, invisible to Orca and remote viewers, though the skill forbade
// both. Pinned: those launches and a blocking dispatch are denied from any
// shell, the wave agent's detached run and read-only calls pass, and the
// hatches work.

const host = (on: On, extra: Partial<HostOptions> = {}) => fakeHost(on, { sessionId: freshSession(), ...extra })

const main = ($: unknown, command: string, extra: Record<string, unknown> = {}) =>
    callTool($, undefined, { tool: 'Bash', command, description: 'run it', ...extra })

const asAgent = async ($: unknown, agentType: string, command: string) => {
    await startAgent($, 'ag_1', agentType, '/w')

    return callTool($, 'ag_1', { tool: 'Bash', command, description: 'run it' })
}

describe('launch', () => {
    test('a wave run starts only from the wave agent, detached', async ($, on) => {
        host(on)
        const run = 'agent-exec wave run --plan p --state s'
        const denied = await main($, run)
        expect(denied.deny).toContain('orchestra:orchestra-wave')
        expect(denied.deny).toContain(run)
        expect((await main($, run, { run_in_background: true })).deny, 'backgrounded').toBeDefined()
        expect((await main($, 'cd x && /usr/local/bin/agent-exec wave run --plan p --state s')).deny, 'cd + abs path').toBeDefined()
        expect((await asAgent($, 'orchestra:orchestra-light', `${run} --detach`)).deny, 'another subagent').toBeDefined()
        expect((await asAgent($, 'orchestra:orchestra-wave', `${run} --detach`)).deny).toBeUndefined()
        expect((await main($, 'agent-exec wave status --state s')).deny).toBeUndefined()
    })

    test('claude -p is denied, other claude calls and mentions are not', async ($, on) => {
        host(on)
        expect((await main($, 'claude -p "x"')).deny).toContain('Agent tool')
        expect((await main($, 'FOO=1 claude --print x')).deny).toBeDefined()
        expect((await main($, 'claude --version')).deny).toBeUndefined()
        expect((await main($, 'echo claude -p')).deny).toBeUndefined()
    })

    test('dispatch must be detached', async ($, on) => {
        host(on)
        expect((await main($, 'agent-exec dispatch --token t --capture')).deny).toContain('dispatch prepare')
        expect((await main($, 'agent-exec dispatch --token t --capture --detach')).deny).toBeUndefined()
        expect((await main($, 'agent-exec dispatch wait --token t')).deny).toBeUndefined()
    })

    test('escape marker in the command or the description', async ($, on) => {
        host(on)
        expect((await main($, 'claude -p x # [orchestra:allow-shell-launch: user asked]')).deny).toBeUndefined()
        expect((await main($, 'agent-exec wave run --plan p', { description: 'x [orchestra:allow-shell-launch]' })).deny).toBeUndefined()
    })

    test('kill switch: ORCHESTRA_LAUNCH_GUARD=off', async ($, on) => {
        host(on, { env: { ORCHESTRA_LAUNCH_GUARD: 'off' } })
        expect((await main($, 'agent-exec wave run --plan p --state s')).deny).toBeUndefined()
    })
})
