import { isInstructorModel, setVerdict } from './verdict.ts'

// SessionStart (startup, resume, clear, compact): inject the routing protocol.
//
// Hybrid gate on the input's `model` (documented as omittable):
//   Opus/Fable -> the router, no self-gate paragraph (already gated here)
//   any other  -> nothing: zero context cost on cheap sessions
//   missing    -> the full self-gated router (safe fallback)
// The verdict is kept per session for the hooks whose input lacks the model.

const BODY = `Classify EACH user request before acting.

EXPRESS — ALL of: one self-contained change (or conversational/read-only/a
question); no decomposition, no design decisions, no cross-task coordination;
small expected context bloat. File count is NOT a criterion.
→ Do it yourself, or via ONE cheap subagent (haiku/sonnet). No verifier
  pipeline, no workflow. Hand the test/build run to a cheap subagent and read
  back only its verdict. Name the tier: a subagent call that leaves \`model\`
  out inherits YOUR model (Opus/Fable), so pass \`model: "sonnet"\` (or
  "haiku") explicitly for investigation, search, and test runs.
The gate is context bloat, NOT your model tier. Before judging size, ask "do I
already hold this content?" — if writing the delegation prompt would mean
re-authoring a spec you just wrote, delegating buys nothing. That exemption is
about content you already hold, NOT about volume: it does not license an
open-ended build, and it expires the moment the work outgrows what you held.

ORCHESTRATED — anything else, and WHENEVER IN DOUBT.
→ Load the \`orchestra:run\` skill and follow it: decompose, write contracts,
  delegate to cheap workers with adversarial verifiers, take back only
  structured verdicts.

A lane never persists — re-classify every request and at every phase shift.
Forced ORCHESTRATED, even mid-conversation: you are about to hand-write or
edit substantive code in 2+ files; a design discussion just turned into
"implement it" / 「PRを出しましょう」/「直しておきましょう」; or you are planning your
own multi-step implement-then-verify sequence. Then STOP and load
\`orchestra:run\` first.

ABORT RULE — the lane is wrong more often mid-turn than at the start, because
the prompts that produce the biggest turns are the smallest ones ("作業を続けて",
"実装して", "y"). So re-judge while working, not only when asked: if a turn you
began as EXPRESS keeps sprouting new subtasks or is many files deep, STOP.
Finishing the edit already open is fine; starting the next chunk by hand is
not — re-route it through \`orchestra:run\` instead.
</orchestra-router>
`

const SELF_GATE = `<orchestra-router>
This protocol applies ONLY when this session's main model is Opus or Fable
(check the model named in your system prompt; on Sonnet/Haiku, ignore this
entire block).

`

const SOURCES = new Set(['startup', 'resume', 'clear', 'compact'])

// Records the session's verdict and answers the text to inject, if any.
export const routerContext = (source: string, sessionId: string, rawModel: unknown): string | undefined => {
    if (!SOURCES.has(source)) {
        return undefined
    }
    const model = typeof rawModel === 'string' ? rawModel.trim() : ''
    if (!model) {
        setVerdict(sessionId, 'unknown')

        return SELF_GATE + BODY
    }
    if (isInstructorModel(model)) {
        setVerdict(sessionId, 'instructor')

        return `<orchestra-router>\n${BODY}`
    }
    setVerdict(sessionId, 'cheap')

    return undefined
}
