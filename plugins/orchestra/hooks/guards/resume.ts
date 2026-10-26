import type { Io } from '../lib/io.ts'
import { field } from '../lib/agents.ts'
import { agentExec, enforcementOf, isOff, loadEnforcement } from '../lib/config.ts'
import { hasEscape } from '../lib/shell.ts'

// SendMessage-resuming an agent that already holds a long context re-reads all
// of it on every turn, and cache reads count against the plan limits. Such an
// agent is replaced by a fresh one with a compact handoff instead.
//
// Deny when the target agent's context has reached enforcement.resume_max_tokens
// (default 100000). Only SendMessage calls to a named or id'd agent of this
// session are checked; a teammate, another session, `main` or an unknown name
// passes.
//
// Escape hatches: [orchestra:allow-resume(: reason)] in the message lets the
// first resume of each agent through (a second one for the same agent is checked
// normally); enforcement.resume_max_tokens: off or 0; ORCHESTRA_RESUME_GUARD=off;
// fail-open on anything unexpected.

const DEFAULT_LIMIT = 100000

// Agents whose one-time escape has been spent this session.
const resumeEscaped = new Set<string>()

const limitOf = (raw: unknown): number | undefined => {
    if (isOff(raw) || raw === 0) {
        return undefined
    }

    return typeof raw === 'number' && Number.isFinite(raw) && raw > 0 ? raw : DEFAULT_LIMIT
}

// `name [ref]` names the agent `name`; the `[ref]` is the teammate-ref suffix.
const targetName = (to: string): string => to.replace(/ \[[^\]]*\]$/, '')

const reason = (tokens: number, limit: number): string => {
    const k = Math.round(tokens / 1000)
    const l = Math.round(limit / 1000)

    return `orchestra: this agent already holds ~${k}k tokens of context (limit ${l}k, enforcement.resume_max_tokens); every resumed turn re-reads all of it, and cache reads count against the plan. Spawn a fresh agent instead with a compact handoff: the goal, what was done and verified, what failed and why, the exact next step, and the files to read. Resume only when the history itself is the value, with [orchestra:allow-resume: <reason>] in the message (once per agent).`
}

// The `to` of a SendMessage, or undefined when the call is not checked.
const checkedTarget = (input: Readonly<Record<string, unknown>>): string | undefined => {
    const to = input.to
    if (typeof to !== 'string' || to === '') {
        return undefined
    }
    const name = targetName(to)

    return name === '' || name === 'main' ? undefined : name
}

// `undefined` allows the call; a string is the deny text.
export const checkResume = async (
    io: Io,
    input: Readonly<Record<string, unknown>>,
    sessionId: string,
): Promise<string | undefined> => {
    try {
        const name = checkedTarget(input)
        if (name === undefined) {
            return undefined
        }
        if (await io.env('ORCHESTRA_RESUME_GUARD').catch(() => undefined) === 'off') {
            return undefined
        }
        const limit = limitOf(enforcementOf(await loadEnforcement(io, sessionId)).resume_max_tokens)
        if (limit === undefined) {
            return undefined
        }
        const agents = await io.agents()
        const target = agents.find(a => a.id === name) ?? agents.find(a => a.name === name)
        if (!target) {
            return undefined
        }
        if (!resumeEscaped.has(target.id) && hasEscape('resume', field(input, 'message'))) {
            resumeEscaped.add(target.id)

            return undefined
        }
        const r = await agentExec(io, ['hook', 'context', '--session', sessionId, '--agent', target.id], 5000)
        if (r.kind !== 'ok') {
            return undefined
        }
        const data: unknown = JSON.parse(r.stdout)
        const tokens = typeof data === 'object' && data !== null ? (data as Record<string, unknown>).context_tokens : undefined
        if (typeof tokens !== 'number' || !Number.isFinite(tokens) || tokens < limit) {
            return undefined
        }

        return reason(tokens, limit)
    } catch {
        // an unexpected failure allows the call: a hook must never block the session
        return undefined
    }
}
