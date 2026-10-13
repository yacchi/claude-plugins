import type { Io } from '../lib/io.ts'
import { field } from '../lib/agents.ts'
import { agentExec, loadEnforcement } from '../lib/config.ts'
import { hasEscape } from '../lib/shell.ts'

// enforcement.light_class=block (opt-in, default off): nudge a direct Claude
// Haiku spawn toward `agent-exec dispatch` when the user's priority puts an
// external executor ahead of Claude for the light class. With the default
// priority `route --class light` names claude, so this is inert.
//
// A NUDGE, NEVER A WALL -- independent escape hatches:
//   1. Per-SESSION deny cap: at most ONE deny per session, full stop. Not
//      per fingerprint: orchestra's retry convention appends new feedback
//      every round ("This is retry N of MAX ... apply this feedback
//      exactly: ..."), so a prompt fingerprint changes each round and a
//      fingerprint-only design burns a whole retry budget on DENY/DENY/DENY.
//      The SHA-256 fingerprint of the denied call is kept as a secondary
//      fast path only.
//   2. [orchestra:allow-claude(: reason)] in the prompt or description.
//   3. Carve-outs: the prompt mentions `agent-exec` (the relay that runs the
//      dispatch -- denying it would deadlock the mechanism); an
//      orchestra-review subagent_type (Claude-only by design); ANY named
//      subagent_type other than general-purpose/claude, which IS the
//      tool-control / specialized-prompt mechanism an external executor has
//      no equivalent for -- except orchestra-light, the one named type
//      redirecting is the point of; and (vestigial) a tools/allowed_tools/...
//      key in the call.
//   4. ORCHESTRA_ENFORCEMENT=off, and fail-open on anything unexpected.
// Deny only when route names a non-claude executor: otherwise there is
// nothing better to redirect to, for the rest of the session.

type SessionState = {
    lightClass: 'off' | 'block',
    executor: string,
    model: string,
    effort: string,
    denied: boolean,
    fingerprints: Set<string>,
}

const sessions = new Map<string, SessionState>()

const RESTRICTION_KEYS = ['tools', 'allowed_tools', 'allowedTools', 'disallowed_tools', 'disallowedTools']

const norm = (s: string): string => s.replace(/\s+/g, ' ').trim()

const sha256 = async (text: string): Promise<string> => {
    const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(text))

    return [...new Uint8Array(digest)].map(b => b.toString(16).padStart(2, '0')).join('')
}

const resolveState = async (io: Io, sessionId: string): Promise<SessionState> => {
    const state: SessionState = { lightClass: 'off', executor: '', model: '', effort: '', denied: false, fingerprints: new Set() }
    const lookup = await loadEnforcement(io, sessionId)
    if (lookup.kind === 'ok' && lookup.enforcement.light_class === 'block') {
        const route = await agentExec(io, ['route', '--class', 'light', '--json'], 5000)
        if (route.kind === 'ok') {
            try {
                const data: unknown = JSON.parse(route.stdout)
                if (typeof data === 'object' && data !== null) {
                    const d = data as Record<string, unknown>
                    state.executor = typeof d.executor === 'string' ? d.executor : ''
                    state.model = typeof d.model === 'string' ? d.model : ''
                    state.effort = typeof d.effort === 'string' ? d.effort : ''
                }
            } catch {
                // unparseable route: nothing to redirect to
            }
        }
        state.lightClass = state.executor && state.executor !== 'claude' ? 'block' : 'off'
    }

    return state
}

const reason = (s: SessionState): string => {
    const target = s.executor || 'an external executor'
    const detail = s.model ? ` (model ${s.model}${s.effort ? `, effort ${s.effort}` : ''})` : ''

    return `orchestra: enforcement.light_class=block prefers ${target}${detail} over a direct Claude Haiku subagent for light-class work. Use: agent-exec dispatch --class light --prompt-file <prompt-file> --workdir <workdir> --capture -- one call, replaces the manual multi-step relay. This nudge fires at most ONCE per session: every further orchestra-light/Haiku call this session -- this same task, a retry with feedback, or an unrelated task -- will be allowed automatically from here on. To bypass permanently for a legitimate reason (e.g. you need a named subagent for fine-grained tool control or a specialized system prompt, which an external executor has no equivalent for), add [orchestra:allow-claude: <reason>] to the prompt.`
}

export const checkLightClass = async (
    io: Io,
    input: Readonly<Record<string, unknown>>,
    sessionId: string,
): Promise<string | undefined> => {
    if (await io.env('ORCHESTRA_ENFORCEMENT') === 'off' || !sessionId) {
        return undefined
    }
    const prompt = field(input, 'prompt')
    const description = field(input, 'description')
    const subagentType = field(input, 'subagent_type')
    const model = field(input, 'model')
    const st = subagentType.trim().toLowerCase()
    if (hasEscape('claude', prompt, description)
        || `${prompt}\n${description}`.toLowerCase().includes('agent-exec')
        || st.includes('orchestra-review')
        || RESTRICTION_KEYS.some(k => k in input)) {
        return undefined
    }
    const isGeneric = ['', 'general-purpose', 'claude'].includes(st)
    const isOrchestraLight = st === 'orchestra-light' || st === 'orchestra:orchestra-light'
    if (!isOrchestraLight && !(isGeneric && model.trim().toLowerCase().includes('haiku'))) {
        return undefined
    }

    // The cap is checked before agent-exec is ever asked again.
    let state = sessions.get(sessionId)
    if (state?.denied) {
        return undefined
    }
    if (!state) {
        state = await resolveState(io, sessionId)
        sessions.set(sessionId, state)
    }
    if (state.lightClass !== 'block') {
        return undefined
    }
    const fingerprint = await sha256([norm(prompt), norm(subagentType), norm(model)].join('\x1f'))
    if (state.fingerprints.has(fingerprint)) {
        return undefined
    }
    state.denied = true
    state.fingerprints.add(fingerprint)

    return reason(state)
}
