import type { FsStat, ProcessRunInit, ProcessRunResult } from 'claude-code'

// What the rule files may ask of the host. The engine follows `$` only within
// the module file, so orchestra.ts builds this from `$` per hook call and the
// rule files take it instead.

export type EnvName =
    | 'HOME'
    | 'ORCHESTRA_ENFORCEMENT'
    | 'ORCHESTRA_FANOUT_GUARD'
    | 'ORCHESTRA_LAUNCH_GUARD'
    | 'ORCHESTRA_LEASE_GUARD'
    | 'ORCHESTRA_LEASE_WINDOW'
    | 'ORCHESTRA_OPUS_NUDGE'
    | 'ORCHESTRA_RESUME_GUARD'
    | 'ORCHESTRA_SESSION_CLEANUP'
    | 'ORCHESTRA_TEST_GUARD'
    | 'ORCHESTRA_TREE_GUARD'
    | 'ORCHESTRA_TURN_GUARD'
    | 'ORCHESTRA_USAGE_GUARD'
    | 'ORCHESTRA_VCS_GUARD'

// One agent of the session as `$.agent.list()` reports it.
export type AgentInfo = {
    id: string,
    type: string,
    status: string,
    parentId?: string,
    name?: string,
}

// One rate-limit window of the Claude plan (`$.session.usage().rateLimits`).
export type RateLimit = {
    kind: string,
    percentUsed: number,
    resetsAt?: string,
}

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
    // The plugin's own root directory ($.plugin.root); '' when unknown.
    pluginRoot: () => string,
    // Every agent of the session, as $.agent.list() reports it.
    agents: () => Promise<readonly AgentInfo[]>,
    // The plan's rate-limit windows; [] off a subscription or before the first reading.
    rateLimits: () => Promise<readonly RateLimit[]>,
}
