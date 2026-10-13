import { expect, test } from 'claude-code/testing'
import type { On } from 'claude-code'

// The bottom stands for the engine: it answers spawns with fresh ids and records
// what the HUD pins to the status line and toasts.
const setup = (on: On) => {
    const statuses: (string | undefined)[] = []
    const toasts: string[] = []
    let n = 0
    on('agent.spawn', (_$, e) => ({ model: e.model ?? 'claude-sonnet-5', agentId: `a${++n}` }))
    on('ui.status', (_$, e) => {
        statuses.push(e.text)

        return { value: undefined }
    })
    on('ui.toast', (_$, e) => {
        toasts.push(e.text)

        return { value: undefined }
    })

    on('turn.complete', () => ({ text: '' }))

    return { statuses, toasts }
}


// The kit types `$` strictly; these helpers pass only the fields the HUD reads.
const spawn = ($: any, args: { prompt: string, description: string, subagentType: string, model?: string }) =>
    $.agent.spawn(args)

const finish =($: any, agentId: string | undefined, answer = '', reason = 'answer') =>
    $.turn.complete({ agentId, answer, reason, durationMs: 1, isAborted: false, turnId: 't' })

test('tracks only orchestra agents, clears status and failures when idle', async ($, on) => {
    const { statuses, toasts } = setup(on)
    await spawn($, { prompt: 'x', description: 'plain', subagentType: 'Explore' })
    expect(statuses).toEqual([])

    const a = await spawn($, { prompt: 'x', description: 'impl', subagentType: 'orchestra:orchestra-light', model: 'haiku' })
    const r = await spawn($, { prompt: 'x', description: 'relay', subagentType: 'orchestra:orchestra-relay' })
    expect(statuses.at(-1)).toContain('2 running')
    expect(statuses.at(-1)).toContain('ext 1')

    await finish($, undefined)
    await finish($, 'unknown')
    expect(statuses.at(-1)).toContain('2 running')

    await finish($, a.agentId, '```json\n{"pass": false}\n```')
    expect(toasts.length).toBe(1)
    expect(statuses.at(-1)).toContain('✗1')
    await finish($, r.agentId, 'not json {')
    expect(statuses.at(-1)).toBeUndefined()

    await spawn($, { prompt: 'x', description: 'again', subagentType: 'orchestra:orchestra-light' })
    expect(statuses.at(-1)).not.toContain('✗')
})

test('hud: false emits nothing', { options: { hud: false } }, async ($, on) => {
    const { statuses, toasts } = setup(on)
    const a = await spawn($, { prompt: 'x', description: 'impl', subagentType: 'orchestra:orchestra-light' })
    await finish($, a.agentId, '{"pass": false}')
    expect(statuses).toEqual([])
    expect(toasts).toEqual([])
})
