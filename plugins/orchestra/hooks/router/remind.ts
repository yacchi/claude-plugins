import { verdictOf } from './verdict.ts'

// UserPromptSubmit: a ~45-token reminder on every prompt. The SessionStart
// injection sits at the very top of the context and decays: transcripts show
// instructor sessions doing 200+ direct Edits with zero mention of the
// classification. Recency is the fix. Gated on the SessionStart verdict:
// instructor -> reminder, cheap -> nothing, unknown -> self-gated reminder.

const REMINDER = `<orchestra-router-reminder>Classify THIS request (see <orchestra-router>):
EXPRESS only if ONE small self-contained change, or if you already hold the
whole content in context. Otherwise hand-writing code in 2+ files yourself →
load \`orchestra:run\` first.</orchestra-router-reminder>
`

const SELF_GATE = '(Applies ONLY if this session\'s main model is Opus or Fable; on Sonnet/Haiku ignore this block.)\n'

export const reminderFor = (sessionId: string): string | undefined => {
    const verdict = verdictOf(sessionId)
    if (verdict === 'cheap' || verdict === 'worker') {
        return undefined
    }

    return verdict === 'instructor' ? REMINDER : SELF_GATE + REMINDER
}
