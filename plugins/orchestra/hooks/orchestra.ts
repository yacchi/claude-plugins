import type { EngineInterface, Register } from 'claude-code'
import { checkLightClass } from './guards/light-class.ts'
import { checkOpusGeneralist } from './guards/opus-generalist.ts'
import { checkWorkerTests } from './guards/worker-tests.ts'
import { checkWorkerTree } from './guards/worker-tree.ts'
import { checkWorkerVcs } from './guards/worker-vcs.ts'
import { checkWorktreeLease, recordActivity, releaseLeases } from './guards/worktree-lease.ts'
import { register as registerHud } from './hud.ts'
import { cleanupWorktrees } from './lifecycle/cleanup-worktrees.ts'
import { forgetAgent, noteAgent, type WorkerCall, workerCall } from './lib/agents.ts'
import { checkDependencyEdit } from './lib/dependency-edit.ts'
import type { EnvName, Io } from './lib/io.ts'
import { type CommandClass, classifyCommand, withHeavySlot } from './lib/slot.ts'
import { recordWorkerBash } from './lib/trace.ts'
import { routerContext } from './router/inject.ts'
import { turnEditsNudge } from './router/turn-edits.ts'
import { verdictOf } from './router/verdict.ts'

// The orchestra plugin's one hooks module (a plugin has one): each guard and
// router event is hooked once here and handed to the files that own its
// rules; the HUD (hud.ts) registers its own hooks, a Bash-only tool.call
// among them. Every guard is a nudge or a second layer, never a hard wall:
// each has its own escape hatches and fails open on anything unexpected (a
// guard that errors would disrupt every tool call of every session), hence
// each `.catch` passes on.

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
            case 'ORCHESTRA_TEST_GUARD': return $.env.get('ORCHESTRA_TEST_GUARD')
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
    on('classic.SessionStart', async ($, e, next) => withContext(
        await next(e),
        await routerContext(ioOf($), e.source, e.session_id, e.model, e.cwd),
    ))
        .catch(($, e, next) => next(e))

    on('tool.call', async ($, e, next) => {
        const tool = String(e.tool)
        const io = ioOf($)
        let call: WorkerCall | undefined
        // A worker's Bash call: a subagent's, or the main thread of a session
        // a wave hosts. Its command is classified once, for the full-suite
        // guard and for slot admission.
        let isWorker = false
        let commandClass: CommandClass | undefined
        if (WRITE_TOOLS.has(tool)) {
            // Only subagent calls are guarded; the main thread is never denied
            // (except a wave-hosted worker session's, for dependency edits and
            // full test suites).
            call = await workerCall(io, e)
            if (tool !== 'Bash' && (call || verdictOf(await io.sessionId().catch(() => '')) === 'worker')) {
                const cwd = call?.cwd ?? await io.sessionCwd().catch(() => '')
                const deny = checkDependencyEdit(tool, e as unknown as Readonly<Record<string, unknown>>, cwd)
                if (deny) {
                    return { deny }
                }
            }
            if (call) {
                const deny = await checkWorkerVcs(io, call)
                    ?? await checkWorkerTree(io, call)
                    ?? await checkWorktreeLease(io, call)
                if (deny) {
                    return { deny }
                }
            }
            if (tool === 'Bash') {
                const input = e as unknown as Readonly<Record<string, unknown>>
                const command = String(input.command ?? '')
                isWorker = call !== undefined || verdictOf(await io.sessionId().catch(() => '')) === 'worker'
                if (isWorker) {
                    commandClass = await classifyCommand(io, command).catch(() => undefined)
                    const deny = await checkWorkerTests(io, input, call?.agentType ?? '', commandClass)
                    if (deny) {
                        return { deny }
                    }
                }
                // Every Bash call, the main thread's included, marks the
                // linked worktree it runs in as in use.
                const by = call ?? {
                    agentId: '',
                    agentType: 'main thread',
                    cwd: await io.sessionCwd().catch(() => ''),
                    sessionId: await io.sessionId().catch(() => ''),
                }
                await recordActivity(io, { ...by, command }).catch(() => undefined)
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

        if (tool !== 'Bash') {
            return next(e)
        }
        const input = e as unknown as Readonly<Record<string, unknown>>
        const command = String(input.command ?? '')
        const sessionId = await io.sessionId().catch(() => '')
        // Time the command itself; the record is written without holding up
        // the result, which is returned exactly as the tool produced it.
        const run = async () => {
            const startMs = await io.now().catch(() => Date.now())
            const result = await next(e)
            if (await $.env.get('ORCHESTRA_TRACE').catch(() => undefined) !== 'off') {
                const endMs = await io.now().catch(() => Date.now())
                const cwd = await io.sessionCwd().catch(() => '')
                const isError = (result as { isError?: boolean } | undefined)?.isError === true
                void recordWorkerBash(io, { startMs, endMs, command, cwd, isError, sessionId })
            }

            return result
        }
        // A worker's heavy command waits for a slot of its machine-wide pool
        // before it starts; one agent-exec classified as not heavy skips the
        // acquire (an unclassified one still asks). The instructor's own main
        // thread never waits, and a background command is not admitted: it
        // would release at once while it runs on.
        const admit = isWorker && input.run_in_background !== true && commandClass?.heavy !== false

        return admit ? withHeavySlot(io, command, run) : run()
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
