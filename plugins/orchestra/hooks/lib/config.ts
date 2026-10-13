import type { Io } from './io.ts'

// One way to ask agent-exec anything, and one reader for its `enforcement`
// block. Every caller fails open: a hook that errors would disrupt every tool
// call of every session, so nothing here throws.

export type AgentExecResult =
    | { kind: 'ok', stdout: string }
    | { kind: 'missing' }
    | { kind: 'failed', stdout?: string }

// `command -v` first, so "agent-exec is not installed" (each guard's default
// applies) is told apart from "we could not ask" (a nonzero exit or a timeout).
const PROBE = 'command -v agent-exec >/dev/null 2>&1 || exit 127; exec agent-exec "$@"'

export const agentExec = async (
    io: Io,
    args: readonly string[],
    timeoutMs: number,
): Promise<AgentExecResult> => {
    try {
        const r = await io.run(['sh', '-c', PROBE, 'agent-exec', ...args], { timeoutMs })
        if (r.exitCode === 127) {
            return { kind: 'missing' }
        }

        return r.exitCode === 0 ? { kind: 'ok', stdout: r.stdout } : { kind: 'failed', stdout: r.stdout }
    } catch {
        return { kind: 'failed' }
    }
}

export type Enforcement = Readonly<Record<string, unknown>>

export type EnforcementLookup =
    | { kind: 'ok', enforcement: Enforcement }
    | { kind: 'missing' }
    | { kind: 'failed' }

const isRecord = (v: unknown): v is Record<string, unknown> =>
    typeof v === 'object' && v !== null && !Array.isArray(v)

// Successful (and "not installed") lookups are kept per session, so the
// common path costs one agent-exec call per session, not one per tool call.
// A failed lookup is never kept: a transient failure must not lock a session
// into one answer for its whole life.
const perSession = new Map<string, EnforcementLookup>()

export const loadEnforcement = async (io: Io, sessionId?: string): Promise<EnforcementLookup> => {
    if (sessionId !== undefined) {
        const kept = perSession.get(sessionId)
        if (kept) {
            return kept
        }
    }
    const r = await agentExec(io, ['config', '--json'], 5000)
    let lookup: EnforcementLookup
    if (r.kind === 'ok') {
        let enforcement: Enforcement = {}
        try {
            const data: unknown = JSON.parse(r.stdout)
            if (isRecord(data) && isRecord(data.enforcement)) {
                enforcement = data.enforcement
            }
        } catch {
            // unparseable config reads as "nothing set"
        }
        lookup = { kind: 'ok', enforcement }
    } else {
        lookup = { kind: r.kind }
    }
    if (sessionId !== undefined && lookup.kind !== 'failed') {
        perSession.set(sessionId, lookup)
    }

    return lookup
}

// The enforcement block, or {} when agent-exec is missing or failed.
export const enforcementOf = (lookup: EnforcementLookup): Enforcement =>
    lookup.kind === 'ok' ? lookup.enforcement : {}

// `off` written as the string, or as the YAML 1.1 bareword that loads as false.
export const isOff = (v: unknown): boolean =>
    v === false || (typeof v === 'string' && v.trim().toLowerCase() === 'off')
