// The SessionStart gate's verdict per session, reused by the hooks whose own
// input does not reliably carry the model (UserPromptSubmit, PostToolUse,
// Agent calls). Absent reads as `unknown`.

// `worker`: a session hosted in an orchestra task worktree, told to do its
// prompt itself rather than orchestrate.
export type Verdict = 'instructor' | 'cheap' | 'unknown' | 'worker'

const verdicts = new Map<string, Verdict>()

export const setVerdict = (sessionId: string, verdict: Verdict): void => {
    if (sessionId) {
        verdicts.set(sessionId, verdict)
    }
}

export const verdictOf = (sessionId: string): Verdict => verdicts.get(sessionId) ?? 'unknown'

// Opus or Fable is the expensive instructor tier the router is for.
export const isInstructorModel = (model: string): boolean => /opus|fable/.test(model.toLowerCase())
