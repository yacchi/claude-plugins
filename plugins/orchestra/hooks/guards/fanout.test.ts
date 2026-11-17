import { describe, expect, test } from 'claude-code/testing'
import type { ProcessRunResult } from 'claude-code'
import type { AgentInfo, Io } from '../lib/io.ts'
import { checkChildBudget, checkFanout, noteChildRequest } from './fanout.ts'

// A planner may spawn only scouts and checkers, a bounded number of them and
// a bounded number at once; each child has a tool-call budget. Module maps are
// global, so every test takes fresh ids.

let seq = 0
const fresh = (prefix: string): string => `${prefix}-${++seq}`

type Script = {
    types?: Record<string, string>,
    agents?: AgentInfo[],
    enforcement?: Record<string, unknown>,
    env?: Record<string, string>,
}

const out = (exitCode: number, stdout = ''): ProcessRunResult =>
    ({ exitCode, stdout, stderr: '', isStdoutTruncated: false, isStderrTruncated: false })

const host = (script: Script): Io => {
    const run = async (argv: readonly string[]) => {
        if (argv[0] === 'sh' && argv[3] === 'agent-exec') {
            return out(0, JSON.stringify({ enforcement: script.enforcement ?? {} }))
        }

        return out(127)
    }

    return {
        env: async (name: string) => script.env?.[name],
        run,
        agentType: async (id: string) => script.types?.[id],
        agents: async () => script.agents ?? [],
    } as unknown as Io
}

const spawn = (io: Io, subagentType: string, planner: string, sessionId = fresh('s')) =>
    checkFanout(io, { subagent_type: subagentType, prompt: 'look', description: 'look' }, sessionId, planner)

const SCOUT = 'orchestra:orchestra-scout'
const CHECKER = 'orchestra:orchestra-checker'

describe('fanout: spawn types', () => {
    test('a planner spawns only scouts and checkers; other spawns are denied', async () => {
        const planner = fresh('pl')
        const io = host({ types: { [planner]: 'orchestra:orchestra-planner' } })
        expect(await spawn(io, 'general-purpose', planner)).toContain('a planner spawns only')
        expect(await spawn(io, 'Explore', planner)).toContain('a planner spawns only')
        expect(await spawn(io, SCOUT, planner)).toBeUndefined()
        expect(await spawn(io, CHECKER, planner)).toBeUndefined()
    })

    test('the main thread and non-planner subagents are never checked', async () => {
        const worker = fresh('wk')
        const io = host({ types: { [worker]: 'orchestra:orchestra-delegate' } })
        expect(await checkFanout(io, { subagent_type: 'general-purpose' }, fresh('s'), undefined)).toBeUndefined()
        expect(await spawn(io, 'general-purpose', worker)).toBeUndefined()
    })
})

describe('fanout: spawn count and concurrency', () => {
    test('with defaults the 7th spawn of one planner is denied; a second planner has its own count', async () => {
        const planner = fresh('pl')
        const io = host({ types: { [planner]: 'orchestra:orchestra-planner' } })
        const sessionId = fresh('s')
        for (let i = 0; i < 6; i++) {
            expect(await spawn(io, SCOUT, planner, sessionId)).toBeUndefined()
        }
        expect(await spawn(io, SCOUT, planner, sessionId)).toContain('fan-out cap reached')

        const other = fresh('pl')
        const io2 = host({ types: { [other]: 'orchestra:orchestra-planner' } })
        expect(await spawn(io2, SCOUT, other, sessionId)).toBeUndefined()
    })

    test('two live children block a third; a finished child frees its slot', async () => {
        const planner = fresh('pl')
        const kids = (statuses: AgentInfo['status'][]): AgentInfo[] => statuses.map((status, i) => ({
            id: `${planner}-kid${i}`,
            type: SCOUT,
            status,
            parentId: planner,
        }))
        const types = { [planner]: 'orchestra:orchestra-planner' }

        const busy = host({ types, agents: kids(['pending', 'running']) })
        expect(await spawn(busy, SCOUT, planner)).toContain('2 of this planner\'s children are still running')

        const mixed = host({ types, agents: kids(['completed', 'running']) })
        expect(await spawn(mixed, SCOUT, planner)).toBeUndefined()
    })
})

// The model's requests of a child, as the turn.step hook counts them.
const requests = (agentId: string, n: number): void => {
    for (let i = 0; i < n; i++) {
        noteChildRequest(agentId)
    }
}

describe('fanout: child request budget', () => {
    test('a scout may act on its first 15 requests; from the 16th every call is denied, and the denial persists', async () => {
        const scout = fresh('sc')
        const io = host({ types: { [scout]: SCOUT } })
        const sessionId = fresh('s')
        requests(scout, 15)
        expect(await checkChildBudget(io, sessionId, scout)).toBeUndefined()
        expect(await checkChildBudget(io, sessionId, scout)).toBeUndefined()
        requests(scout, 1)
        expect(await checkChildBudget(io, sessionId, scout)).toContain('request budget spent (15 model requests for an orchestra-scout)')
        expect(await checkChildBudget(io, sessionId, scout)).toContain('request budget spent')
    })

    test('parallel tool calls of one request count once', async () => {
        const scout = fresh('sc')
        const io = host({ types: { [scout]: SCOUT } })
        const sessionId = fresh('s')
        requests(scout, 15)
        for (let i = 0; i < 30; i++) {
            expect(await checkChildBudget(io, sessionId, scout, 'Read'), `call ${i + 1}`).toBeUndefined()
        }
    })

    test('a checker may act on 8 requests and the 9th is denied', async () => {
        const checker = fresh('ck')
        const io = host({ types: { [checker]: CHECKER } })
        const sessionId = fresh('s')
        requests(checker, 8)
        expect(await checkChildBudget(io, sessionId, checker)).toBeUndefined()
        requests(checker, 1)
        expect(await checkChildBudget(io, sessionId, checker)).toContain('request budget spent (8 model requests for an orchestra-checker)')
    })

    test('a spent spec writer is denied reads but never Write or Edit', async () => {
        const writer = fresh('sw')
        const io = host({ types: { [writer]: 'orchestra:orchestra-spec-writer' } })
        const sessionId = fresh('s')
        requests(writer, 41)
        expect(await checkChildBudget(io, sessionId, writer, 'Read')).toContain('request budget spent (40 model requests for an orchestra-spec-writer)')
        expect(await checkChildBudget(io, sessionId, writer, 'Write')).toBeUndefined()
        expect(await checkChildBudget(io, sessionId, writer, 'Edit')).toBeUndefined()
    })

    test('other child types are unlimited', async () => {
        const other = fresh('gp')
        const io = host({ types: { [other]: 'general-purpose' } })
        const sessionId = fresh('s')
        requests(other, 40)
        expect(await checkChildBudget(io, sessionId, other)).toBeUndefined()
    })
})

describe('fanout: configuration', () => {
    test('a partial block takes defaults for the other fields', async () => {
        const planner = fresh('pl')
        const io = host({ types: { [planner]: 'orchestra:orchestra-planner' }, enforcement: { fanout: { children: 2 } } })
        const sessionId = fresh('s')
        expect(await spawn(io, SCOUT, planner, sessionId)).toBeUndefined()
        expect(await spawn(io, SCOUT, planner, sessionId)).toBeUndefined()
        expect(await spawn(io, SCOUT, planner, sessionId)).toContain('fan-out cap reached: this planner already started 2 children')
    })

    test('"off" in config and ORCHESTRA_FANOUT_GUARD=off disable both checks', async () => {
        const planner = fresh('pl')
        const kid = fresh('sc')
        const types = { [planner]: 'orchestra:orchestra-planner', [kid]: SCOUT }

        const cfgOff = host({ types, enforcement: { fanout: 'off' } })
        expect(await spawn(cfgOff, 'general-purpose', planner)).toBeUndefined()
        const sessionA = fresh('s')
        requests(kid, 20)
        expect(await checkChildBudget(cfgOff, sessionA, kid)).toBeUndefined()

        const envOff = host({ types, env: { ORCHESTRA_FANOUT_GUARD: 'off' } })
        expect(await spawn(envOff, 'general-purpose', planner)).toBeUndefined()
        const sessionB = fresh('s')
        expect(await checkChildBudget(envOff, sessionB, kid)).toBeUndefined()
    })
})

describe('fanout: ORCHESTRA_ENFORCEMENT=off and late registration', () => {
    test('ORCHESTRA_ENFORCEMENT=off disables the spawn cap and the child budget', async () => {
        const planner = fresh('pl')
        const scout = fresh('sc')
        const io = host({ types: { [planner]: 'orchestra:orchestra-planner', [scout]: SCOUT }, env: { ORCHESTRA_ENFORCEMENT: 'off' } })
        expect(await spawn(io, 'general-purpose', planner)).toBeUndefined()
        requests(scout, 20)
        expect(await checkChildBudget(io, fresh('s'), scout)).toBeUndefined()
    })

    test('a child whose type is registered after its first call is still budgeted', async () => {
        const scout = fresh('sc')
        const types: Record<string, string> = {}
        const io = host({ types, enforcement: { fanout: { scout_requests: 1 } } })
        const sessionId = fresh('s')
        requests(scout, 2)
        expect(await checkChildBudget(io, sessionId, scout)).toBeUndefined()
        types[scout] = SCOUT
        expect(await checkChildBudget(io, sessionId, scout)).toContain('request budget spent')
    })
})
