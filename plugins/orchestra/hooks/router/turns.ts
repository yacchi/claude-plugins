// The main loop's current turn per session, as `turn.start` announces it:
// the hooks that count per turn key on it. Main-loop turns are sequential,
// so one id per session is enough.

const turns = new Map<string, string>()

export const noteTurn = (sessionId: string, turnId: string): void => {
    if (sessionId && turnId) {
        turns.set(sessionId, turnId)
    }
}

export const turnOf = (sessionId: string): string | undefined => turns.get(sessionId)
