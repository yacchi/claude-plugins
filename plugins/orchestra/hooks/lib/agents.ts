import type { ToolCallInput } from 'claude-code'
import type { Io } from './io.ts'

// `tool.call` names the loop (`agentId`) but not the agent's type or its
// working directory, which the guards scope on. The classic events carry both
// (`agent_type`, `cwd`), so they are recorded here as each subagent starts
// and after each of its tool calls: the cwd after one call is the cwd the
// next one starts in, the value the PreToolUse hooks used to read.

type Seen = { type: string, cwd: string }

const seen = new Map<string, Seen>()

const str = (v: unknown): string => (typeof v === 'string' ? v : '')

// Record what a classic event says about a subagent; ignores the main thread.
export const noteAgent = (agentId: unknown, type: unknown, cwd: unknown): void => {
    const id = str(agentId)
    if (!id) {
        return
    }
    const was = seen.get(id)
    seen.set(id, { type: str(type) || was?.type || '', cwd: str(cwd) || was?.cwd || '' })
}

export const forgetAgent = (agentId: string): void => {
    seen.delete(agentId)
}

// A tool call made inside a subagent, with what the guards need to scope it.
export type WorkerCall = {
    tool: string,
    input: Readonly<Record<string, unknown>>,
    agentId: string,
    agentType: string,
    cwd: string,
    sessionId: string,
}

export const field = (input: Readonly<Record<string, unknown>>, key: string): string => str(input[key])

export const workerCall = async (io: Io, e: ToolCallInput): Promise<WorkerCall | undefined> => {
    const agentId = e.agentId ?? ''
    if (!agentId) {
        return undefined
    }
    const known = seen.get(agentId)
    const agentType = known?.type || await io.agentType(agentId).catch(() => undefined) || ''
    const cwd = known?.cwd || await io.sessionCwd().catch(() => '')
    const sessionId = await io.sessionId().catch(() => '')

    return {
        tool: String(e.tool),
        input: e as unknown as Readonly<Record<string, unknown>>,
        agentId,
        agentType,
        cwd,
        sessionId,
    }
}

// The supervising layer owns snapshots, rollbacks, collection and integration
// into the main tree by design (references/isolation.md).
export const isSupervisor = (agentType: string): boolean =>
    agentType.toLowerCase().includes('orchestra-delegate')
