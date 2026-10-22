import type { Io } from '../lib/io.ts'
import { field } from '../lib/agents.ts'
import { isOff, loadEnforcement } from '../lib/config.ts'
import { hasEscape } from '../lib/shell.ts'
import { verdictOf } from '../router/verdict.ts'

// A general-purpose subagent that would run on the instructor's top tier: one
// that omits `model` (so it inherits the session's Opus/Fable model) or names
// Fable. Naming Opus is a deliberate choice and is never denied (SKILL §3).
//
// A NUDGE, NEVER A WALL (about half were read-only investigation, where Opus
// is occasionally right): at most ONE deny per session, and that deny says
// how to proceed either way. Escape hatches: the cap;
// [orchestra:allow-opus(: reason)] in the prompt or description (does not
// spend the cap); carve-outs for a prompt mentioning `agent-exec`, a named
// subagent_type other than general-purpose/claude, or a named model other
// than Fable;
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

    return `orchestra: this general-purpose subagent ${how}, the most expensive tier `
        + '(Fable 5x and Opus 2x Sonnet per token), usually by accident. Re-issue the call as: '
        + '(1) read-only investigation, search, test or build runs -> add '
        + '`model: "sonnet"` (or "haiku" for a mechanical lookup); '
        + '(2) writing or changing code -> do not use a generalist: load the '
        + '`orchestra:run` skill and route it through its Workflow, or through the '
        + 'orchestra-delegate subagent when Workflows are unavailable (both dispatch '
        + 'detached; a foreground `agent-exec dispatch` in your own Bash dies at the '
        + '600 s limit). If this one really needs the instructor-class model, retry the '
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
    // An omitted model inherits the instructor's (the accident this catches);
    // naming Fable is the top tier. Naming Opus is a deliberate choice -- SKILL §3
    // sanctions it for judgment-heavy review, and from a Fable session it is the cheaper tier.
    const isInstructorClass = model === '' || model.includes('fable')
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
