import type { Io, RateLimit } from '../lib/io.ts'
import { field } from '../lib/agents.ts'
import { enforcementOf, isOff, loadEnforcement } from '../lib/config.ts'
import type { Enforcement } from '../lib/config.ts'
import { hasEscape } from '../lib/shell.ts'

// A planner fanned out 23 children and spent the plan's five-hour window in
// under an hour, with nothing to slow the spawns as it filled. Both levels read
// the same window, `$.session.usage().rateLimits`, the engine's figure from the
// last API response (subagents run in this process, so they count too):
//   - advise at enforcement.usage_advise (default 75): the call is allowed and
//     carries one note, at most once per 5-point step per session. A note is
//     context every later turn re-reads, so it is never repeated per call.
//   - stop at enforcement.usage_guard (default 95): Agent, Task and Workflow
//     calls are denied. A main-thread Agent call may pass with
//     [orchestra:allow-usage: <reason>], twice per session.
// The figure can lag, so every doubt allows: a missing window, an unreadable
// switch or config, or any exception.

const AGENT_TOOLS = new Set(['Agent', 'Task'])
const USAGE_TOOLS = new Set(['Agent', 'Task', 'Workflow'])

// The control plane must keep running while the window is full: the watcher
// and the relay are what report progress and carry a dispatch through.
const EXEMPT_SUFFIXES = ['orchestra-wave', 'orchestra-relay']

const STOP_DEFAULT = 95
const ADVISE_DEFAULT = 75
const ADVISE_STEP = 5
const ESCAPE_LIMIT = 2

const escapes = new Map<string, number>()
const advisedStep = new Map<string, number>()

// A number in (0, 100]; anything else reads as absent.
const percentOf = (v: unknown): number | undefined =>
    typeof v === 'number' && Number.isFinite(v) && v > 0 && v <= 100 ? v : undefined

const stopOf = (enforcement: Enforcement): number => {
    const v = percentOf(enforcement.usage_guard)

    return v ?? STOP_DEFAULT
}

// `undefined` means advice is off (`usage_advise: "off"`).
const adviseOf = (enforcement: Enforcement): number | undefined => {
    if (isOff(enforcement.usage_advise)) {
        return undefined
    }

    return percentOf(enforcement.usage_advise) ?? ADVISE_DEFAULT
}

const isExempt = (tool: string, input: Readonly<Record<string, unknown>>): boolean => {
    if (!AGENT_TOOLS.has(tool)) {
        return false
    }
    const type = field(input, 'subagent_type').trim().toLowerCase()

    return EXEMPT_SUFFIXES.some(s => type.endsWith(s))
}

type Reading = {
    percent: number,
    resetsAt: string,
    stop: number,
    advise: number | undefined,
}

// The window's reading for a call the guard judges; undefined when the call is
// not the guard's business.
const readingOf = async (
    io: Io,
    tool: string,
    input: Readonly<Record<string, unknown>>,
    sessionId: string,
): Promise<Reading | undefined> => {
    if (!USAGE_TOOLS.has(tool) || isExempt(tool, input)) {
        return undefined
    }
    if (await io.env('ORCHESTRA_ENFORCEMENT') === 'off') {
        return undefined
    }
    const fiveHour = (await io.rateLimits()).find((l: RateLimit) => l.kind === 'five_hour')
    if (!fiveHour) {
        return undefined
    }
    if (await io.env('ORCHESTRA_USAGE_GUARD').catch(() => 'off') === 'off') {
        return undefined
    }
    const enforcement = enforcementOf(await loadEnforcement(io, sessionId))
    if (isOff(enforcement.usage_guard)) {
        return undefined
    }
    if (!Number.isFinite(fiveHour.percentUsed)) {
        return undefined
    }

    return {
        percent: fiveHour.percentUsed,
        resetsAt: fiveHour.resetsAt ?? 'unknown',
        stop: stopOf(enforcement),
        advise: adviseOf(enforcement),
    }
}

const denyText = (r: Reading, exhausted: boolean): string => {
    const text = `orchestra: Claude plan usage is ${r.percent}% of the 5-hour window (threshold ${r.stop}%, resets ${r.resetsAt}); starting new agents is held so the window is not exhausted. Do not do the delegated work in this thread instead (a long thread costs more than the agent would): stop, tell the user the usage and the reset time, and wait for their decision; inside a subagent, return what you have now. A main-thread Agent call may pass with [orchestra:allow-usage: <reason>] (twice per session). Turn the guard off with enforcement.usage_guard: "off" or ORCHESTRA_USAGE_GUARD=off.`

    return exhausted ? `${text} (the ${ESCAPE_LIMIT} escapes of this session are used up)` : text
}

const adviceText = (r: Reading): string =>
    `orchestra: Claude plan usage is ${r.percent}% of the 5-hour window (resets ${r.resetsAt}). Pace down: start fewer agents at once, give each a narrower scope and a small budget, prefer finishing what is running; or pause until the reset. Tell the user if the remaining work will not fit.`

// The deny for a call at or above the stop level, or undefined to allow it.
export const checkUsage = async (
    io: Io,
    tool: string,
    input: Readonly<Record<string, unknown>>,
    sessionId: string,
    agentId: string | undefined,
): Promise<string | undefined> => {
    try {
        const r = await readingOf(io, tool, input, sessionId)
        if (!r || r.percent < r.stop) {
            return undefined
        }
        if (!agentId && AGENT_TOOLS.has(tool) && hasEscape('usage', field(input, 'prompt'), field(input, 'description'))) {
            const used = escapes.get(sessionId) ?? 0
            if (used < ESCAPE_LIMIT) {
                escapes.set(sessionId, used + 1)

                return undefined
            }

            return denyText(r, true)
        }

        return denyText(r, false)
    } catch {
        return undefined
    }
}

// The advisory note for a call the guard allows, or undefined when there is
// nothing new to say. Call it only after checkUsage allowed the call: it records
// the step it reports, so a second call in the same step gets no note.
export const usageAdvice = async (
    io: Io,
    tool: string,
    input: Readonly<Record<string, unknown>>,
    sessionId: string,
): Promise<string | undefined> => {
    try {
        const r = await readingOf(io, tool, input, sessionId)
        if (!r || r.advise === undefined || r.percent < r.advise || r.percent >= r.stop) {
            return undefined
        }
        const step = Math.max(r.advise, Math.floor(r.percent / ADVISE_STEP) * ADVISE_STEP)
        if (step <= (advisedStep.get(sessionId) ?? 0)) {
            return undefined
        }
        advisedStep.set(sessionId, step)

        return adviceText(r)
    } catch {
        return undefined
    }
}
