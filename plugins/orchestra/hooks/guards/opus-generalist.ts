import type { Io } from '../lib/io.ts'
import { field } from '../lib/agents.ts'
import { isOff, loadEnforcement } from '../lib/config.ts'
import { hasEscape } from '../lib/shell.ts'
import { verdictOf } from '../router/verdict.ts'

// A general-purpose subagent that would run on the instructor's own model.
// A 30-day, two-machine review (feedback/2026-10-03-usage-review.md) found 45
// generalist calls naming `model: opus` and 43 omitting `model` (which
// inherits Opus/Fable) against 72 Sonnet and 33 Haiku, ~1.58M output tokens:
// nothing said that an omitted model means Opus. This says it, once.
//
// A NUDGE, NEVER A WALL (about half were read-only investigation, where Opus
// is occasionally right): at most ONE deny per session, and that deny says
// how to proceed either way. Escape hatches: the cap;
// [orchestra:allow-opus(: reason)] in the prompt or description (does not
// spend the cap); carve-outs for a prompt mentioning `agent-exec`, a named
// subagent_type other than general-purpose/claude, or a non-Opus/Fable model;
// ORCHESTRA_OPUS_NUDGE=off or enforcement.opus_generalist: off; fail-open.
//
// Scope: the main thread of a session the SessionStart gate classified as
// "instructor". On a Sonnet session an omitted model is not an Opus spawn.

const denied = new Set<string>()
const disabled = new Map<string, boolean>()

const reason = (model: string): string => {
    const how = model === ''
        ? 'leaves `model` out, so it inherits this session\'s Opus/Fable model'
        : `names \`model: ${model}\``

    return `orchestra: this general-purpose subagent ${how}. Opus-class generalists were `
        + 'the largest avoidable cost in past sessions. Re-issue the call as: '
        + '(1) read-only investigation, search, test or build runs -> add '
        + '`model: "sonnet"` (or "haiku" for a mechanical lookup); '
        + '(2) writing or changing code -> do not use a generalist: load the '
        + '`orchestra:run` skill and send it to a worker (`agent-exec dispatch '
        + '--class light` / `standard`, or orchestra-deep for design-sensitive '
        + 'work). If this one really needs the instructor-class model, retry the '
        + 'same call with `[orchestra:allow-opus: <reason>]` in the description. '
        + 'This is denied once per session; the retry is never blocked.'
}

export const checkOpusGeneralist = async (
    io: Io,
    input: Readonly<Record<string, unknown>>,
    sessionId: string,
    agentId: string | undefined,
): Promise<string | undefined> => {
    if (agentId || !sessionId) {
        return undefined
    }
    const prompt = field(input, 'prompt')
    const description = field(input, 'description')
    const subagentType = field(input, 'subagent_type').trim().toLowerCase()
    const model = field(input, 'model').trim().toLowerCase()
    const isGeneric = ['', 'general-purpose', 'claude'].includes(subagentType)
    // An omitted model inherits the instructor's; naming Opus/Fable is the
    // same spawn on purpose; anything else is already a cheaper tier.
    const isInstructorClass = model === '' || model.includes('opus') || model.includes('fable')
    if (!isGeneric || !isInstructorClass || hasEscape('opus', prompt, description)
        || `${prompt}\n${description}`.toLowerCase().includes('agent-exec')) {
        return undefined
    }
    if (await io.env('ORCHESTRA_OPUS_NUDGE') === 'off') {
        return undefined
    }
    if (verdictOf(sessionId) !== 'instructor' || denied.has(sessionId)) {
        return undefined
    }
    let off = disabled.get(sessionId)
    if (off === undefined) {
        const lookup = await loadEnforcement(io, sessionId)
        off = lookup.kind === 'ok' && isOff(lookup.enforcement.opus_generalist)
        disabled.set(sessionId, off)
    }
    if (off) {
        return undefined
    }
    denied.add(sessionId)

    return reason(model)
}
