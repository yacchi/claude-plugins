import { describe, expect, test } from 'claude-code/testing'
import type { On } from 'claude-code'
import { fakeHost, freshSession, type HostOptions } from '../testing/host.ts'

// The router costs nothing on cheap sessions and falls back to a self-gated
// text when the model is unknown; the turn tripwire re-asks for a route at
// the threshold and, firmer, at twice it, once each per turn.

const host = (on: On, extra: Partial<HostOptions> = {}) => fakeHost(on, { sessionId: freshSession(), ...extra })

type Context = { additionalContext?: string[] }

const begin = ($: any, sessionId: string, model?: string, source = 'startup'): Promise<Context> =>
    $.classic.SessionStart({ session_id: sessionId, source, ...(model ? { model } : {}) })

const prompt = ($: any, sessionId: string): Promise<Context> =>
    $.classic.UserPromptSubmit({ session_id: sessionId, prompt: 'y' })

// The nudges handed back over `n` edits of one turn.
const edits = async ($: any, n: number, sessionId: string, promptId: string | undefined, tool = 'Edit', agentId?: string) => {
    const said: string[] = []
    for (let i = 0; i < n; i++) {
        const r: Context = await $.classic.PostToolUse({
            session_id: sessionId,
            ...(promptId ? { prompt_id: promptId } : {}),
            ...(agentId ? { agent_id: agentId, agent_type: 'orchestra:orchestra-light' } : {}),
            tool_name: tool, tool_input: { file_path: '/tmp/x.ts' }, tool_response: {}, tool_use_id: `t${i}`,
        })
        said.push(...(r.additionalContext ?? []))
    }

    return said
}

describe('router', () => {
    test('session start gates on the model', async ($, on) => {
        host(on)
        const opus = (await begin($, 's-opus', 'claude-opus-5')).additionalContext ?? []
        expect(opus.length).toBe(1)
        expect(opus[0]).toContain('<orchestra-router>\nClassify EACH user request')
        expect(opus[0]).not.toContain('applies ONLY')
        expect((await begin($, 's-sonnet', 'claude-sonnet-5')).additionalContext ?? [], 'cheap: zero context').toEqual([])
        expect((await begin($, 's-none')).additionalContext?.[0], 'unknown: self-gated').toContain('applies ONLY when')
        for (const source of ['resume', 'clear', 'compact']) {
            expect((await begin($, 's-again', 'claude-fable-5', source)).additionalContext?.length, source).toBe(1)
        }
        expect((await begin($, 's-fork', 'claude-opus-5', 'fork')).additionalContext ?? []).toEqual([])
    })

    test('the per-prompt reminder follows the session verdict', async ($, on) => {
        host(on)
        await begin($, 's-opus', 'claude-opus-5')
        await begin($, 's-sonnet', 'claude-sonnet-5')
        const instructor = (await prompt($, 's-opus')).additionalContext ?? []
        expect(instructor[0]).toContain('<orchestra-router-reminder>')
        expect(instructor[0]).not.toContain('Applies ONLY')
        expect((await prompt($, 's-sonnet')).additionalContext ?? []).toEqual([])
        expect((await prompt($, 's-never-started')).additionalContext?.[0]).toContain('Applies ONLY')
    })

    test('turn edits: stage 1 at the threshold, stage 2 at twice it, once each per turn', async ($, on) => {
        host(on)
        await begin($, 's1', 'claude-opus-5')
        expect(await edits($, 7, 's1', 'p1'), 'below the threshold').toEqual([])
        const stage1 = await edits($, 1, 's1', 'p1')
        expect(stage1.length).toBe(1)
        expect(stage1[0]).toContain('hand-edited 8 files')
        expect(stage1[0]).toContain('a firmer request follows')
        expect(await edits($, 7, 's1', 'p1'), 'between the stages').toEqual([])
        const stage2 = await edits($, 1, 's1', 'p1')
        expect(stage2[0]).toContain('hand-edited 16 files')
        expect(stage2[0]).toContain('exemption stops applying')
        expect(await edits($, 5, 's1', 'p1'), 'stage 2 once per turn').toEqual([])
        expect(await edits($, 7, 's1', 'p2'), 'the counter resets on a new turn').toEqual([])
        expect((await edits($, 9, 's1', 'p2')).length, 'a new turn nudges again').toBe(2)
    })

    test('turn edits: what counts and where', async ($, on) => {
        host(on)
        await begin($, 's1', 'claude-opus-5')
        await begin($, 's-cheap', 'claude-haiku-5')
        await edits($, 4, 's1', 'p1')
        await edits($, 1, 's1', 'p1', 'Write')
        await edits($, 1, 's1', 'p1', 'MultiEdit')
        expect((await edits($, 2, 's1', 'p1', 'NotebookEdit')).length, 'every edit tool counts').toBe(1)
        expect(await edits($, 9, 's1', 'p3', 'Read'), 'reads').toEqual([])
        expect(await edits($, 9, 's1', 'p4', 'Edit', 'ag_1'), 'a subagent\'s edits').toEqual([])
        expect(await edits($, 9, 's1', undefined), 'no prompt_id, no turn').toEqual([])
        expect(await edits($, 9, 's-cheap', 'p1'), 'cheap session').toEqual([])
        expect((await edits($, 8, 's-unknown', 'p1')).length, 'unknown verdict still nudges').toBe(1)
        await begin($, 's2', 'claude-opus-5')
        await begin($, 's3', 'claude-opus-5')
        await edits($, 5, 's2', 'p1')
        expect(await edits($, 4, 's3', 'p1'), 'sessions do not share a counter').toEqual([])
    })

    test('turn edits: enforcement.turn_edits moves the threshold', async ($, on) => {
        host(on, { agentExec: () => ({ exitCode: 0, stdout: '{"enforcement": {"turn_edits": 3}}' }) })
        await begin($, 's1', 'claude-opus-5')
        expect((await edits($, 3, 's1', 'p1')).length).toBe(1)
    })

    test('turn edits: enforcement.turn_edits=off', async ($, on) => {
        host(on, { agentExec: () => ({ exitCode: 0, stdout: '{"enforcement": {"turn_edits": "off"}}' }) })
        await begin($, 's1', 'claude-opus-5')
        expect(await edits($, 10, 's1', 'p1')).toEqual([])
    })

    test('turn edits: kill switch ORCHESTRA_TURN_GUARD=off', async ($, on) => {
        host(on, { env: { ORCHESTRA_TURN_GUARD: 'off' } })
        await begin($, 's1', 'claude-opus-5')
        expect(await edits($, 10, 's1', 'p1')).toEqual([])
    })
})
