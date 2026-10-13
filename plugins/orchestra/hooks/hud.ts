import type { Register } from 'claude-code'

// Live orchestra visibility: a status-line entry while orchestra agents run,
// a toast only on notable failures. Never blocks or rewrites anything.

type Tracked = { bucket: string, description: string }

const tracked = new Map<string, Tracked>()
let failures = 0

const familyOf = (model: string | undefined): string => {
    const m = (model ?? '').toLowerCase()
    for (const family of ['haiku', 'sonnet', 'opus', 'fable']) {
        if (m.includes(family)) {
            return family
        }
    }

    return 'other'
}

// Lenient: a ```json fence first, else the outermost {...}.
const parseVerdict = (answer: string | undefined): Record<string, unknown> | undefined => {
    if (typeof answer !== 'string') {
        return undefined
    }
    const fenced = answer.match(/```(?:json)?\s*([\s\S]*?)```/)
    const candidates = [fenced?.[1], answer.slice(answer.indexOf('{'), answer.lastIndexOf('}') + 1)]
    for (const text of candidates) {
        if (!text) {
            continue
        }
        try {
            const value: unknown = JSON.parse(text)
            if (value !== null && typeof value === 'object' && !Array.isArray(value)) {
                return value as Record<string, unknown>
            }
        } catch {
            // not JSON; try the next candidate
        }
    }

    return undefined
}

const statusText = (): string | undefined => {
    if (tracked.size === 0) {
        return undefined
    }
    const counts = new Map<string, number>()
    for (const { bucket } of tracked.values()) {
        counts.set(bucket, (counts.get(bucket) ?? 0) + 1)
    }
    const parts = [`orch ▸ ${tracked.size} running`]
    for (const [bucket, n] of counts) {
        parts.push(`${bucket === 'relay' ? 'ext' : bucket} ${n}`)
    }
    if (failures > 0) {
        parts.push(`✗${failures}`)
    }

    return parts.join(' · ')
}

export const register: Register = (on, options) => {
    if (options.hud === false) {
        return
    }

    on('agent.spawn', async ($, e, next) => {
        const result = await next(e)
        try {
            if (result.agentId && (e.subagentType.startsWith('orchestra:') || e.workflow)) {
                const bucket = e.subagentType === 'orchestra:orchestra-relay'
                    ? 'relay'
                    : familyOf(result.model || e.model)
                tracked.set(result.agentId, { bucket, description: (e.description ?? '').slice(0, 60) })
                $.ui.status(statusText())
            }
        } catch {
            // the HUD must never break a spawn
        }

        return result
    }).catch(($, e, next) => next(e))

    on('turn.complete', async ($, e, next) => {
        try {
            const agent = e.agentId ? tracked.get(e.agentId) : undefined
            if (e.agentId && agent) {
                tracked.delete(e.agentId)
                const verdict = parseVerdict(e.answer)
                const status = verdict?.status
                if (e.reason === 'aborted') {
                    // an interrupted agent's partial answer is not a verdict
                } else if (e.reason === 'error') {
                    $.ui.toast(`orchestra: agent error — ${agent.description}`)
                } else if (verdict?.pass === false) {
                    failures++
                    $.ui.toast(`orchestra: review FAIL — ${agent.description}`)
                } else if (status === 'needs-permission' || status === 'runaway' || status === 'unavailable') {
                    $.ui.toast(`orchestra: ${status} — ${agent.description}`)
                } else if (typeof e.answer === 'string' && e.answer.trim().startsWith('ESCALATE')) {
                    $.ui.toast(`orchestra: ESCALATE — ${agent.description}`)
                }
                if (tracked.size === 0) {
                    failures = 0
                    $.ui.status(undefined)
                } else {
                    $.ui.status(statusText())
                }
            }
        } catch {
            // the HUD must never break a turn
        }

        return next(e)
    }).catch(($, e, next) => next(e))
}
