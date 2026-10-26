import type { Io } from '../lib/io.ts'
import { field } from '../lib/agents.ts'
import { enforcementOf, isOff, loadEnforcement } from '../lib/config.ts'

// An orchestra-planner fans out into at most two dedicated child types, each
// with a fixed budget: orchestra-scout (reads and answers one scoped question
// with file:line) and orchestra-checker (checks only cited evidence). Without a
// cap a planner once ran 23 Sonnet children, 14-19 at once, each re-reading a
// growing context on every request. Every denied call still costs the child a
// full-context request, so each deny says what to do instead and never hints
// that a retry might pass.
//
// Three caps, all configured under enforcement.fanout:
//   children    spawns per planner (total)
//   concurrent  live children (pending, running, waiting) per planner
//   scout_calls / checker_calls  tool calls per child
// `fanout: off` or ORCHESTRA_FANOUT_GUARD=off disables both checks. There is
// no per-call escape: a planner must not lift its own cap. Fails open.

export type FanoutConfig = {
    children: number,
    concurrent: number,
    scout_calls: number,
    checker_calls: number,
}

const DEFAULTS: FanoutConfig = { children: 6, concurrent: 2, scout_calls: 15, checker_calls: 8 }

const LIVE = new Set(['pending', 'running', 'waiting'])

const spawned = new Map<string, number>()
const childTypes = new Map<string, string>()
const childCalls = new Map<string, number>()

const block = (v: unknown): Record<string, unknown> =>
    typeof v === 'object' && v !== null && !Array.isArray(v) ? v as Record<string, unknown> : {}

const positive = (v: unknown, fallback: number): number =>
    typeof v === 'number' && Number.isInteger(v) && v > 0 ? v : fallback

// The effective caps, or undefined when the guard is off.
const fanoutConfig = async (io: Io, sessionId: string): Promise<FanoutConfig | undefined> => {
    if (isOff(await io.env('ORCHESTRA_FANOUT_GUARD'))) {
        return undefined
    }
    const enforcement = enforcementOf(await loadEnforcement(io, sessionId))
    if (isOff(enforcement.fanout)) {
        return undefined
    }
    const f = block(enforcement.fanout)

    return {
        children: positive(f.children, DEFAULTS.children),
        concurrent: positive(f.concurrent, DEFAULTS.concurrent),
        scout_calls: positive(f.scout_calls, DEFAULTS.scout_calls),
        checker_calls: positive(f.checker_calls, DEFAULTS.checker_calls),
    }
}

const ALLOWED_CHILD = ['orchestra-scout', 'orchestra-checker']

// Gates an Agent/Task call. Only a planner's spawns are checked; the main
// thread and every other subagent pass through.
export const checkFanout = async (
    io: Io,
    input: Readonly<Record<string, unknown>>,
    sessionId: string,
    agentId: string | undefined,
): Promise<string | undefined> => {
    try {
        if (!agentId) {
            return undefined
        }
        const callerType = (await io.agentType(agentId)) ?? ''
        if (!callerType.toLowerCase().endsWith('orchestra-planner')) {
            return undefined
        }
        const cfg = await fanoutConfig(io, sessionId)
        if (!cfg) {
            return undefined
        }
        const subagentType = field(input, 'subagent_type').trim().toLowerCase()
        if (!ALLOWED_CHILD.some(t => subagentType.endsWith(t))) {
            return 'orchestra: a planner spawns only orchestra:orchestra-scout (one scoped reading question, file:line answers) '
                + 'and orchestra:orchestra-checker (checks only the cited file:line evidence). '
                + 'Re-issue this call with one of them, or read it yourself.'
        }
        const count = spawned.get(agentId) ?? 0
        if (count >= cfg.children) {
            return `orchestra: fan-out cap reached: this planner already started ${count} children `
                + '(enforcement.fanout.children). Read the rest yourself with narrow greps, or put it in your report as an open question.'
        }
        const live = (await io.agents()).filter(a => a.parentId === agentId && LIVE.has(a.status)).length
        if (live >= cfg.concurrent) {
            return `orchestra: ${live} of this planner's children are still running (enforcement.fanout.concurrent = ${cfg.concurrent}); `
                + 'wait for one to return before starting another.'
        }
        spawned.set(agentId, count + 1)

        return undefined
    } catch {
        return undefined
    }
}

// Caps the tool calls of a scout or checker child. Once its budget is spent,
// every later call is denied too, so the child ends instead of retrying.
export const checkChildBudget = async (
    io: Io,
    sessionId: string,
    agentId: string,
): Promise<string | undefined> => {
    try {
        let type = childTypes.get(agentId)
        if (type === undefined) {
            type = ((await io.agentType(agentId)) ?? '').toLowerCase()
            childTypes.set(agentId, type)
        }
        const role = type.endsWith('orchestra-scout') ? 'scout' : type.endsWith('orchestra-checker') ? 'checker' : ''
        if (!role) {
            return undefined
        }
        const cfg = await fanoutConfig(io, sessionId)
        if (!cfg) {
            return undefined
        }
        const budget = role === 'scout' ? cfg.scout_calls : cfg.checker_calls
        const used = childCalls.get(agentId) ?? 0
        if (used < budget) {
            childCalls.set(agentId, used + 1)

            return undefined
        }

        return `orchestra: tool budget spent (${budget} tool calls for an orchestra-${role}). `
            + 'Stop now and answer with what you have: cite file:line for every claim and list what is left unchecked.'
    } catch {
        return undefined
    }
}
