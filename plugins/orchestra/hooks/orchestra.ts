import type { EngineInterface, Register } from 'claude-code'
import { checkLightClass } from './guards/light-class.ts'
import { checkOpusGeneralist } from './guards/opus-generalist.ts'
import { checkWorkerTree } from './guards/worker-tree.ts'
import { checkWorkerVcs } from './guards/worker-vcs.ts'
import { checkWorktreeLease, releaseLeases } from './guards/worktree-lease.ts'
import { register as registerHud } from './hud.ts'
import { cleanupWorktrees } from './lifecycle/cleanup-worktrees.ts'
import { forgetAgent, noteAgent, workerCall } from './lib/agents.ts'
import type { EnvName, Io } from './lib/io.ts'
import { routerContext } from './router/inject.ts'
import { reminderFor } from './router/remind.ts'
import { turnEditsNudge } from './router/turn-edits.ts'

// The orchestra plugin's one hooks module (a plugin has one): each event is
// hooked once here and handed to the files that own its rules. Every guard is
// a nudge or a second layer, never a hard wall: each has its own escape
// hatches and fails open on anything unexpected (a guard that errors would
// disrupt every tool call of every session), hence each `.catch` passes on.

const WRITE_TOOLS = new Set(['Edit', 'Write', 'MultiEdit', 'NotebookEdit', 'Bash'])
const AGENT_TOOLS = new Set(['Agent', 'Task'])

// The host as the rule files see it. Each variable is read by its literal
// name, as the engine requires of `$.env.get`.
const ioOf = ($: EngineInterface): Io => ({
    env: (name: EnvName) => {
        switch (name) {
            case 'HOME': return $.env.get('HOME')
            case 'ORCHESTRA_ENFORCEMENT': return $.env.get('ORCHESTRA_ENFORCEMENT')
            case 'ORCHESTRA_LEASE_GUARD': return $.env.get('ORCHESTRA_LEASE_GUARD')
            case 'ORCHESTRA_LEASE_WINDOW': return $.env.get('ORCHESTRA_LEASE_WINDOW')
            case 'ORCHESTRA_OPUS_NUDGE': return $.env.get('ORCHESTRA_OPUS_NUDGE')
            case 'ORCHESTRA_SESSION_CLEANUP': return $.env.get('ORCHESTRA_SESSION_CLEANUP')
            case 'ORCHESTRA_TREE_GUARD': return $.env.get('ORCHESTRA_TREE_GUARD')
            case 'ORCHESTRA_TURN_GUARD': return $.env.get('ORCHESTRA_TURN_GUARD')
            case 'ORCHESTRA_VCS_GUARD': return $.env.get('ORCHESTRA_VCS_GUARD')
        }
    },
    run: (argv, init) => $.process.run(argv, init),
    stat: (path, resolve) => $.fs.stat(path, { resolve }),
    exists: path => $.fs.exists(path),
    read: path => $.fs.read(path),
    write: (path, text) => $.fs.write(path, text),
    now: () => $.clock.now(),
    sessionId: () => $.session.id(),
    sessionCwd: () => $.session.cwd(),
    agentType: async agentId => (await $.agent.list()).find(a => a.id === agentId)?.type,
    log: text => $.ui.log(text),
})

const withContext = <R extends { additionalContext?: string[] }>(r: R, text: string | undefined): R =>
    text ? { ...r, additionalContext: [...(r.additionalContext ?? []), text] } : r

export const register: Register = (on, options) => {
    on('classic.SessionStart', async ($, e, next) => withContext(await next(e), routerContext(e.source, e.session_id, e.model)))
        .catch(($, e, next) => next(e))

    on('classic.UserPromptSubmit', async ($, e, next) => withContext(await next(e), reminderFor(e.session_id)))
        .catch(($, e, next) => next(e))

    on('tool.call', async ($, e, next) => {
        const tool = String(e.tool)
        const io = ioOf($)
        if (WRITE_TOOLS.has(tool)) {
            // Only subagent calls are in scope; the main thread costs nothing.
            const call = await workerCall(io, e)
            if (call) {
                const deny = await checkWorkerVcs(io, call)
                    ?? await checkWorkerTree(io, call)
                    ?? await checkWorktreeLease(io, call)
                if (deny) {
                    return { deny }
                }
            }
        } else if (AGENT_TOOLS.has(tool)) {
            const input = e as unknown as Readonly<Record<string, unknown>>
            const sessionId = await io.sessionId().catch(() => '')
            const deny = await checkLightClass(io, input, sessionId)
                ?? await checkOpusGeneralist(io, input, sessionId, e.agentId)
            if (deny) {
                return { deny }
            }
        }

        return next(e)
    }).catch(($, e, next) => next(e))

    on('classic.SubagentStart', ($, e, next) => {
        noteAgent(e.agent_id, e.agent_type, e.cwd)

        return next(e)
    }).catch(($, e, next) => next(e))

    on('classic.PostToolUse', async ($, e, next) => {
        noteAgent(e.agent_id, e.agent_type, e.cwd)

        return withContext(await next(e), await turnEditsNudge(ioOf($), e))
    }).catch(($, e, next) => next(e))

    on('classic.PostToolUseFailure', ($, e, next) => {
        noteAgent(e.agent_id, e.agent_type, e.cwd)

        return next(e)
    }).catch(($, e, next) => next(e))

    on('classic.SubagentStop', async ($, e, next) => {
        await releaseLeases(ioOf($), e.agent_id)
        forgetAgent(e.agent_id)

        return next(e)
    }).catch(($, e, next) => next(e))

    on('classic.SessionEnd', async ($, e, next) => {
        await cleanupWorktrees(ioOf($), e.session_id, e.cwd)

        return next(e)
    }).catch(($, e, next) => next(e))

    registerHud(on, options)
}
