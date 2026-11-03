import { agentExec } from './config.ts'
import type { Io } from './io.ts'

// Called at a subagent's turn.complete; resolves whatever happens and never
// throws. Only orchestra's own agents get a handoff file: the resume guard
// reads it back, and a fresh agent of any other type has no use for one.
export const writeHandoff = async (io: Io, sessionId: string, agentId: string, answer: string): Promise<void> => {
    try {
        if (sessionId === '' || agentId === '') {
            return
        }
        const type = await io.agentType(agentId)
        if (typeof type !== 'string' || !type.startsWith('orchestra:')) {
            return
        }
        await agentExec(io, ['hook', 'handoff', '--session', sessionId, '--agent', agentId, '--type', type, `--answer=${answer.slice(0, 16000)}`], 10000)
    } catch {
        // Fail open: a missing handoff only costs a fresh start later.
    }
}
