import type { Io } from '../lib/io.ts'
import { isOff, loadEnforcement } from '../lib/config.ts'
import { verdictOf } from './verdict.ts'

// PostToolUse turn-size tripwire. Classification happens before acting, but
// the turns that went wrong were small prompts that turned out large
// ("作業を続けて", "y" -> 27-55 hand edits); the size is only knowable once
// the work is underway. So count the main thread's edits per turn and put a
// re-route request in front of the model past a threshold. It never blocks.
//
// Two stages: in a 30-day review the single nudge was answered "EXPRESS 継続
// (仕様は手元にあります)" and ignored, since the "I hold the spec" exemption
// is self-declared. Stage 2, at twice the threshold, says that exemption has
// expired. Each stage fires once per turn (exact-equality hits).
//
// Scope: main thread only (a subagent's edits are the delegation working);
// not on cheap sessions ("unknown" nudges, like the router's fallback); per
// turn keyed on `prompt_id`, and no prompt_id means no turn to count.
// Escape hatches: ORCHESTRA_TURN_GUARD=off; enforcement.turn_edits: off or a
// positive integer threshold (default 8).

const EDIT_TOOLS = new Set(['Edit', 'Write', 'MultiEdit', 'NotebookEdit'])
const DEFAULT_THRESHOLD = 8

const stage1 = (count: number): string =>
    `orchestra: this turn has now hand-edited ${count} files in the main thread. `
    + 'That is past the point where the EXPRESS lane was the right call -- the '
    + 'request looked small when you classified it, and it is not. Re-classify '
    + 'NOW rather than at the next user message: if the remaining work is more '
    + 'than finishing what is already open, stop and load the `orchestra:run` '
    + 'skill, then delegate the rest with contracts and adversarial review. If '
    + 'you are deliberately staying in EXPRESS -- you already hold the whole '
    + 'spec and delegating would mean re-authoring it -- say so in one line and '
    + 'carry on. This fires once per turn; a firmer request follows if the turn '
    + 'reaches twice this size.'

const stage2 = (count: number): string =>
    `orchestra: this turn has now hand-edited ${count} files in the main thread -- `
    + 'twice the re-route threshold, and you already saw the first request. '
    + 'The "I already hold the spec" exemption stops applying here: holding '
    + 'the spec makes a delegation packet cheap to write; it does not make this '
    + 'many hand edits cheap on the instructor model. Do not just declare '
    + 'EXPRESS again. Either (a) load the `orchestra:run` skill and hand the '
    + 'remaining edits to a worker, using the spec you hold as the packet, or '
    + '(b) stop at a clean point, say what is left, and let the user\'s next '
    + 'message start a fresh turn that is classified from scratch. This fires '
    + 'once more at most.'

// Main-thread turns are sequential, so one counter per session is enough:
// a new prompt_id starts it over.
const turns = new Map<string, { promptId: string, count: number }>()
const thresholds = new Map<string, number | 'off'>()

const thresholdFor = async (io: Io, sessionId: string): Promise<number | 'off'> => {
    const kept = thresholds.get(sessionId)
    if (kept !== undefined) {
        return kept
    }
    let threshold: number | 'off' = DEFAULT_THRESHOLD
    const lookup = await loadEnforcement(io, sessionId)
    if (lookup.kind === 'ok' && 'turn_edits' in lookup.enforcement) {
        const v = lookup.enforcement.turn_edits
        if (isOff(v)) {
            threshold = 'off'
        } else if (typeof v === 'number' && Number.isInteger(v) && v > 0) {
            threshold = v
        }
    }
    thresholds.set(sessionId, threshold)

    return threshold
}

type PostToolUse = { tool_name: string, agent_id?: string, prompt_id?: string, session_id: string }

// The re-route request to hand the model after this edit, if any.
export const turnEditsNudge = async (io: Io, e: PostToolUse): Promise<string | undefined> => {
    if (!EDIT_TOOLS.has(e.tool_name) || e.agent_id || !e.prompt_id) {
        return undefined
    }
    if (await io.env('ORCHESTRA_TURN_GUARD') === 'off') {
        return undefined
    }
    const sessionId = e.session_id
    const verdict = verdictOf(sessionId)
    if (verdict === 'cheap' || verdict === 'worker') {
        return undefined
    }
    const threshold = await thresholdFor(io, sessionId)
    if (threshold === 'off') {
        return undefined
    }
    const was = turns.get(sessionId)
    const count = was?.promptId === e.prompt_id ? was.count + 1 : 1
    turns.set(sessionId, { promptId: e.prompt_id, count })

    return count === threshold ? stage1(count) : count === threshold * 2 ? stage2(count) : undefined
}
