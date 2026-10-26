import type { FsStat, ProcessRunInit, ProcessRunResult } from 'claude-code'

// What the rule files may ask of the host. The engine follows `$` only within
// the module file, so orchestra.ts builds this from `$` per hook call and the
// rule files take it instead.

export type EnvName =
    | 'HOME'
    | 'ORCHESTRA_ENFORCEMENT'
    | 'ORCHESTRA_LAUNCH_GUARD'
    | 'ORCHESTRA_LEASE_GUARD'
    | 'ORCHESTRA_LEASE_WINDOW'
    | 'ORCHESTRA_OPUS_NUDGE'
    | 'ORCHESTRA_SESSION_CLEANUP'
    | 'ORCHESTRA_TEST_GUARD'
    | 'ORCHESTRA_TREE_GUARD'
    | 'ORCHESTRA_TURN_GUARD'
    | 'ORCHESTRA_VCS_GUARD'

export type Io = {
    env: (name: EnvName) => Promise<string | undefined>,
    run: (argv: readonly string[], init?: ProcessRunInit) => Promise<ProcessRunResult>,
    stat: (path: string, resolve: boolean) => Promise<FsStat>,
    exists: (path: string) => Promise<boolean>,
    read: (path: string) => Promise<string>,
    write: (path: string, text: string) => Promise<void>,
    // Milliseconds since the epoch.
    now: () => Promise<number>,
    sessionId: () => Promise<string>,
    sessionCwd: () => Promise<string>,
    // The main loop's model, as /model shows it.
    model: () => Promise<string>,
    // The session's project root (not a subagent's).
    root: () => Promise<string>,
    // The main conversation, newest last.
    messages: () => Promise<readonly unknown[]>,
    agentType: (agentId: string) => Promise<string | undefined>,
    log: (text: string) => void,
}
