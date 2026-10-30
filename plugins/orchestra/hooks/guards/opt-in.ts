import type { Io } from '../lib/io.ts'
import { field } from '../lib/agents.ts'
import { isOff, loadEnforcement } from '../lib/config.ts'
import { verdictOf } from '../router/verdict.ts'

// ORCHESTRATED work runs only when the user opted in: typed `/orchestra:run`
// (or `/run`), or asked for a workflow. Without it the instructor asks ONCE
// and stops; a prose exception for "small" work let it skip the ask, so this
// guard is the mechanism. The main thread's orchestration agents are denied
// until the user opts in or answers the one ask (any next user prompt).
//
// Guarded: the orchestra worker agents, and a generic agent (general-purpose,
// claude, or no subagent_type) spawned in the turn that loaded the
// `orchestra:run` skill. Never guarded: read-only Explore and Plan, other
// plugins' agents, a subagent's own spawns, cheap and hosted-worker sessions.
//
// No marker in the prompt or description lifts it: the model is the party
// this constrains. Escape hatches: ORCHESTRA_ENFORCEMENT=off;
// enforcement.opt_in: off; fail-open when no user prompt reached this module.

const WORK = new Set([
    'orchestra-light',
    'orchestra-deep',
    'orchestra-delegate',
    'orchestra-review',
    'orchestra-relay',
    'orchestra-wave',
    'orchestra-planner',
    'orchestra-spec-writer',
])
const GENERIC = new Set(['', 'general-purpose', 'claude'])
const USER_ORIGINS = new Set(['composer', 'bridge', 'sdk', 'slack-ping'])
const RUN_COMMANDS = new Set(['orchestra:run', 'run'])
const OPT_IN_TEXT = [/\bultracode\b/i, /\bworkflows?\b/i, /ワークフロー/]
const TYPED_RUN = ['<command-name>/orchestra:run</command-name>', '<command-name>/run</command-name>']

type OptInState = {
    optedIn: boolean,
    prompts: number,
    askedAt?: number,
    skillTurn?: string,
    scanned: boolean,
}

const optInStates = new Map<string, OptInState>()

const optInState = (sessionId: string): OptInState => {
    let s = optInStates.get(sessionId)
    if (!s) {
        s = { optedIn: false, prompts: 0, scanned: false }
        optInStates.set(sessionId, s)
    }

    return s
}

const optInRecord = (v: unknown): Readonly<Record<string, unknown>> =>
    typeof v === 'object' && v !== null ? v as Record<string, unknown> : {}

// A prompt the user submitted (not a task notification or a meta prompt):
// counted, and checked for an opt-in.
export const noteUserPrompt = (sessionId: string, e: unknown): void => {
    try {
        if (!sessionId) {
            return
        }
        const input = optInRecord(e)
        const kind = String(optInRecord(input.origin).kind ?? '')
        if (!USER_ORIGINS.has(kind) || input.isMeta === true) {
            return
        }
        const s = optInState(sessionId)
        s.prompts++
        const command = String(optInRecord(input.command).name ?? '')
        const text = String(input.text ?? '')
        if (RUN_COMMANDS.has(command) || OPT_IN_TEXT.some(re => re.test(text))) {
            s.optedIn = true
        }
    } catch {
        // fail open
    }
}

// The main loop loaded the `orchestra:run` skill in this turn: a generic
// agent spawned in the same turn is orchestration.
export const noteSkill = (sessionId: string, skill: string, turnId: string | undefined): void => {
    if (sessionId && RUN_COMMANDS.has(skill)) {
        optInState(sessionId).skillTurn = turnId
    }
}

// The main loop's final answer asked the user to type `/orchestra:run`: that
// is the one ask.
export const noteAnswer = (sessionId: string, answer: string): void => {
    if (sessionId && answer.includes('/orchestra:run')) {
        const s = optInState(sessionId)
        s.askedAt ??= s.prompts
    }
}

// A process restarted on `--resume` has seen no prompt event for the
// earlier typed command; the conversation still holds it.
const optInScan = async (io: Io, s: OptInState): Promise<void> => {
    const messages = await io.messages()
    s.scanned = true
    for (const m of messages) {
        const msg = optInRecord(m)
        const text = String(msg.text ?? '')
        if (msg.role === 'user' && TYPED_RUN.some(t => text.includes(t))) {
            s.optedIn = true

            return
        }
    }
}

const reason = (subagentType: string): string =>
    'orchestra: no opt-in for orchestrated work in this session — the user has not typed '
    + '/orchestra:run (or /run) or asked for a workflow, so do not start '
    + `${subagentType || 'a generic agent'} or other orchestration agents yourself. `
    + 'Reply in one line asking the user to type `/orchestra:run <the request>` '
    + '(or to say how they want it done), then stop and wait. Once they answer, '
    + 'spawns are allowed whatever they said. Read-only Explore and Plan agents are never blocked.'

export const checkOptIn = async (
    io: Io,
    input: Readonly<Record<string, unknown>>,
    sessionId: string,
    agentId: string | undefined,
    turnId: string | undefined,
): Promise<string | undefined> => {
    try {
        if (agentId || !sessionId) {
            return undefined
        }
        const subagentType = field(input, 'subagent_type').trim()
        const bare = subagentType.toLowerCase().replace(/^orchestra:/, '')
        const s = optInState(sessionId)
        const guarded = WORK.has(bare)
            || (GENERIC.has(bare) && s.skillTurn !== undefined && s.skillTurn === turnId)
        // The allows that need no config come first, so an unguarded call
        // (or one this module cannot judge) never asks agent-exec.
        if (!guarded || s.prompts === 0 || await io.env('ORCHESTRA_ENFORCEMENT') === 'off') {
            return undefined
        }
        const verdict = await verdictOf(io, sessionId)
        if (verdict === 'cheap' || verdict === 'worker') {
            return undefined
        }
        const lookup = await loadEnforcement(io, sessionId)
        if (lookup.kind === 'ok' && isOff(lookup.enforcement.opt_in)) {
            return undefined
        }
        if (!s.optedIn && !s.scanned) {
            // A failed read leaves `scanned` false, so the next check retries.
            await optInScan(io, s).catch(() => undefined)
        }
        if (s.optedIn || (s.askedAt !== undefined && s.prompts > s.askedAt)) {
            return undefined
        }
        s.askedAt ??= s.prompts

        return reason(subagentType)
    } catch {
        return undefined
    }
}
