import type { EngineInterface, Register } from 'claude-code'
import { dashboardTurnStart, registerDashboard } from './dashboard/dashboard.ts'
import { checkChildBudget, checkFanout } from './guards/fanout.ts'
import { checkLand } from './guards/land.ts'
import { checkLaunch } from './guards/launch.ts'
import { checkLightClass } from './guards/light-class.ts'
import { checkOpusGeneralist } from './guards/opus-generalist.ts'
import { checkOptIn, noteAnswer, noteSkill, noteUserPrompt } from './guards/opt-in.ts'
import { checkResume } from './guards/resume.ts'
import { checkUsage, usageAdvice } from './guards/usage.ts'
import { checkWaveWatcher, WAVE_WATCHER, waveWatcherContext } from './guards/wave-watcher.ts'
import { checkWorkerNode } from './guards/worker-node.ts'
import { checkWorkerTests } from './guards/worker-tests.ts'
import { checkWorkerTree } from './guards/worker-tree.ts'
import { checkWorkerVcs } from './guards/worker-vcs.ts'
import { checkWorktreeLease, recordActivity, releaseLeases } from './guards/worktree-lease.ts'
import { hudTurnComplete, register as registerHud } from './hud.ts'
import { cleanupWorktrees } from './lifecycle/cleanup-worktrees.ts'
import { type WorkerCall, workerCall } from './lib/agents.ts'
import { agentExec } from './lib/config.ts'
import { checkDependencyEdit } from './lib/dependency-edit.ts'
import { writeHandoff } from './lib/handoff.ts'
import type { EnvName, Io } from './lib/io.ts'
import { type CommandClass, classifyCommand, ensureWatcher, guardResult, withHeavySlot } from './lib/slot.ts'
import { recordWorkerBash } from './lib/trace.ts'
import { noteTurn, turnOf } from './router/turns.ts'
import { turnEditsNudge } from './router/turn-edits.ts'
import { verdictOf } from './router/verdict.ts'

// The orchestra plugin's one hooks module (a plugin has one): each guard and
// router event is hooked once here and handed to the files that own its
// rules; the HUD (hud.ts) registers its own hooks, a Bash-only tool.call
// among them, and the board (dashboard/) mirrors status.json to claude.ai.
// Every guard is a nudge or a second layer, never a hard wall:
// each has its own escape hatches and fails open on anything unexpected (a
// guard that errors would disrupt every tool call of every session), hence
// each `.catch` passes on. No listener depends on a `classic.*` event, so all
// of them run where cc-plugin-sec-default is seated; the router text itself
// is a SessionStart command hook (`agent-exec hook session-start`).
//
// The tool.call listener also runs six guards, one line each:
//   usage:   holds Agent, Task and Workflow calls near the plan's 5-hour limit
//   fan-out: caps a planner's spawns, and a scout's, checker's or spec writer's tool calls
//   resume:  holds SendMessage to an agent whose context is already long
//   node:    denies a worker's Bash that replaces NODE_OPTIONS or runs a test runner from node -e
//   process: reports agent-exec's process-watcher kill on a heavy or failed foreground Bash result
//   wave:    holds the orchestra-wave watcher to its two commands and says when an answer is not final
// No guard changes a tool call's input: Claude Code's auto mode reviewed the call before this module runs, and a changed input voids that review.

const WRITE_TOOLS = new Set(['Edit', 'Write', 'MultiEdit', 'NotebookEdit', 'Bash'])
const AGENT_TOOLS = new Set(['Agent', 'Task'])
const EDIT_TOOLS = new Set(['Edit', 'Write', 'MultiEdit', 'NotebookEdit'])

// Sessions whose module heartbeat was stamped (once each, on the first
// main-loop turn), for `agent-exec doctor`.
const stamped = new Set<string>()

// The host as the rule files see it. Each variable is read by its literal
// name, as the engine requires of `$.env.get`.
const ioOf = ($: EngineInterface): Io => ({
    env: (name: EnvName) => {
        switch (name) {
            case 'HOME': return $.env.get('HOME')
            case 'ORCHESTRA_ENFORCEMENT': return $.env.get('ORCHESTRA_ENFORCEMENT')
            case 'ORCHESTRA_FANOUT_GUARD': return $.env.get('ORCHESTRA_FANOUT_GUARD')
            case 'ORCHESTRA_LAUNCH_GUARD': return $.env.get('ORCHESTRA_LAUNCH_GUARD')
            case 'ORCHESTRA_LEASE_GUARD': return $.env.get('ORCHESTRA_LEASE_GUARD')
            case 'ORCHESTRA_LEASE_WINDOW': return $.env.get('ORCHESTRA_LEASE_WINDOW')
            case 'ORCHESTRA_OPUS_NUDGE': return $.env.get('ORCHESTRA_OPUS_NUDGE')
            case 'ORCHESTRA_RESUME_GUARD': return $.env.get('ORCHESTRA_RESUME_GUARD')
            case 'ORCHESTRA_SESSION_CLEANUP': return $.env.get('ORCHESTRA_SESSION_CLEANUP')
            case 'ORCHESTRA_TEST_GUARD': return $.env.get('ORCHESTRA_TEST_GUARD')
            case 'ORCHESTRA_TREE_GUARD': return $.env.get('ORCHESTRA_TREE_GUARD')
            case 'ORCHESTRA_TURN_GUARD': return $.env.get('ORCHESTRA_TURN_GUARD')
            case 'ORCHESTRA_USAGE_GUARD': return $.env.get('ORCHESTRA_USAGE_GUARD')
            case 'ORCHESTRA_VCS_GUARD': return $.env.get('ORCHESTRA_VCS_GUARD')
            case 'XDG_STATE_HOME': return $.env.get('XDG_STATE_HOME')
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
    model: () => $.session.model(),
    root: () => $.session.root(),
    messages: () => $.session.messages(),
    agentType: async agentId => (await $.agent.list()).find(a => a.id === agentId)?.type,
    log: text => $.ui.log(text),
    pluginRoot: () => {
        try {
            return $.plugin.root
        } catch {
            return ''
        }
    },
    agents: async () => (await $.agent.list()).map(a => ({ id: a.id, type: a.type, status: a.status, parentId: a.parentId, name: a.name })),
    rateLimits: async () => ((await $.session.usage()).rateLimits ?? []).map(r => ({ kind: r.kind, percentUsed: r.percentUsed, resetsAt: r.resetsAt })),
})

export const register: Register = (on, options) => {
    // The main loop's turns key the turn-size nudge.
    on('turn.start', async ($, e, next) => {
        if (!e.agentId) {
            const io = ioOf($)
            // The process watcher (one per Claude Code process) is started on
            // every main-loop turn and returns at once when it already runs;
            // never awaited, so a slow agent-exec never holds up the turn.
            void ensureWatcher(io).catch(() => undefined)
            const sessionId = await io.sessionId().catch(() => '')
            noteTurn(sessionId, e.turnId)
            if (sessionId && !stamped.has(sessionId)) {
                stamped.add(sessionId)
                void agentExec(io, ['hook', 'seen', '--session', sessionId, '--event', 'module'], 3000)
            }
            dashboardTurnStart($)
        }

        return next(e)
    }).catch(($, e, next) => next(e))

    // User prompts are counted and checked for an opt-in; the prompt itself
    // passes on unchanged.
    on('prompt.submit', async ($, e, next) => {
        noteUserPrompt(await ioOf($).sessionId().catch(() => ''), e)

        return next(e)
    }).catch(($, e, next) => next(e))

    on('tool.call', async ($, e, next) => {
        const tool = String(e.tool)
        const io = ioOf($)
        const sessionId = await io.sessionId().catch(() => '')
        if (e.agentId) {
            const deny = await checkChildBudget(io, sessionId, e.agentId, tool).catch(() => undefined)
            if (deny) {
                return { deny }
            }
        }
        // The usage note of an allowed Agent, Task or Workflow call rides on
        // that call's own result.
        let advice: string | undefined
        let call: WorkerCall | undefined
        // Every Bash call is classified once: a worker's class feeds the
        // full-suite guard and slot admission, and a heavy foreground call's
        // class asks the process watcher for a kill report. A worker is a
        // subagent, or the main thread of a session a wave hosts.
        let isWorker = false
        let commandClass: CommandClass | undefined
        if (WRITE_TOOLS.has(tool)) {
            // Only subagent calls are guarded; the main thread is never denied
            // (except a wave-hosted worker session's, for dependency edits, full
            // test suites, NODE_OPTIONS replacement and node -e test runners,
            // and anyone's shell launch of long-running orchestra work or merge
            // of an orchestra branch).
            call = await workerCall(io, e)
            if (tool === 'Bash') {
                const input = e as unknown as Readonly<Record<string, unknown>>
                // The wave watcher's commands are held before any other Bash
                // check, launch's included. A throw counts as no deny: the guard fails open.
                if (call?.agentType === WAVE_WATCHER) {
                    let deny: string | undefined
                    try {
                        deny = checkWaveWatcher(call.agentId, input)
                    } catch {
                        deny = undefined
                    }
                    if (deny) {
                        return { deny }
                    }
                }
                // Long-running launches, and merging an orchestra branch
                // outside an orchestra worktree, are denied from every shell,
                // the main thread's included.
                const deny = await checkLaunch(io, input, call?.agentType ?? '')
                    ?? await checkLand(io, input, call?.cwd ?? await io.sessionCwd().catch(() => ''))
                if (deny) {
                    return { deny }
                }
            }
            if (tool !== 'Bash' && (call || await verdictOf(io, await io.sessionId().catch(() => '')) === 'worker')) {
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
                isWorker = call !== undefined || await verdictOf(io, await io.sessionId().catch(() => '')) === 'worker'
                if (isWorker) {
                    // A throw counts as no deny: the guard fails open.
                    let deny: string | undefined
                    try {
                        deny = checkWorkerNode(command, String(input.description ?? ''))
                    } catch {
                        deny = undefined
                    }
                    if (deny) {
                        return { deny }
                    }
                }
                commandClass = await classifyCommand(io, command).catch(() => undefined)
                if (isWorker) {
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
            const deny = await checkUsage(io, tool, input, sessionId, e.agentId)
                ?? await checkFanout(io, input, sessionId, e.agentId)
                ?? await checkLightClass(io, input, sessionId)
                ?? await checkOpusGeneralist(io, input, sessionId, e.agentId)
                ?? await checkOptIn(io, input, sessionId, e.agentId, turnOf(sessionId))
            if (deny) {
                return { deny }
            }
            advice = await usageAdvice(io, tool, input, sessionId).catch(() => undefined)
        } else if (tool === 'Workflow') {
            const input = e as unknown as Readonly<Record<string, unknown>>
            const deny = await checkUsage(io, tool, input, sessionId, e.agentId)
            if (deny) {
                return { deny }
            }
            advice = await usageAdvice(io, tool, input, sessionId).catch(() => undefined)
        } else if (tool === 'SendMessage') {
            const input = e as unknown as Readonly<Record<string, unknown>>
            const deny = await checkResume(io, input, sessionId)
            if (deny) {
                return { deny }
            }
        } else if (tool === 'Skill' && !e.agentId) {
            const input = e as unknown as Readonly<Record<string, unknown>>
            noteSkill(sessionId, String(input.skill ?? ''), turnOf(sessionId))
        }

        if (EDIT_TOOLS.has(tool) && !e.agentId) {
            // The turn-size nudge rides on the edit's own result.
            const result = await next(e)
            if (result.deny !== undefined || result.isError === true) {
                return result
            }
            const nudge = await turnEditsNudge(io, {
                sessionId,
                turnId: turnOf(sessionId),
                tool,
                input: e as unknown as Readonly<Record<string, unknown>>,
                toolUseId: e.toolUseId,
                cwd: await io.sessionCwd().catch(() => ''),
            }).catch(() => undefined)

            return nudge ? { ...result, context: [...(result.context ?? []), nudge] } : result
        }
        if (tool !== 'Bash') {
            if (advice === undefined) {
                return next(e)
            }
            const result = await next(e)

            return { ...result, context: [...(result.context ?? []), advice] }
        }
        const input = e as unknown as Readonly<Record<string, unknown>>
        const command = String(input.command ?? '')
        const background = input.run_in_background === true
        // The tool runs exactly the call auto mode reviewed: `e` is handed on
        // as received. Time the command itself; the record is written without
        // holding up the result, which is returned exactly as the tool produced it.
        const run = async () => {
            const startMs = await io.now().catch(() => Date.now())
            const result = await next(e)
            if (await $.env.get('ORCHESTRA_TRACE').catch(() => undefined) !== 'off') {
                const endMs = await io.now().catch(() => Date.now())
                const cwd = await io.sessionCwd().catch(() => '')
                const isError = (result as { isError?: boolean } | undefined)?.isError === true
                void recordWorkerBash(io, { startMs, endMs, command, cwd, isError, sessionId })
            }

            return { result, startMs }
        }
        // A worker's heavy command waits for a slot of its machine-wide pool
        // before it starts; one agent-exec classified as not heavy skips the
        // acquire (an unclassified one still asks). The instructor's own main
        // thread never waits, and a background command is not admitted: it
        // would release at once while it runs on.
        const admit = isWorker && !background && commandClass?.heavy !== false
        const { result, startMs } = await (admit ? withHeavySlot(io, command, run) : run())
        const failed = (result as { isError?: boolean } | undefined)?.isError === true
        let out = result
        if (!background && (commandClass?.heavy === true || failed)) {
            // Asked after the command has run, and also when it failed: a command
            // the watcher killed usually is an error. Only kills since this command
            // started count.
            const text = await guardResult(io, startMs).catch(() => undefined)
            if (text) {
                out = { ...out, context: [...(out.context ?? []), text] }
            }
        }
        // The wave watcher's answer is named on its own result, after any other context.
        if (call?.agentType === WAVE_WATCHER) {
            let note: string | undefined
            try {
                note = waveWatcherContext(call.agentId, input, result)
            } catch {
                note = undefined
            }
            if (note) {
                out = { ...out, context: [...(out.context ?? []), note] }
            }
        }

        return out
    }).catch(($, e, next) => next(e))

    // A subagent's every turn ends here; its lease is retaken on its next write,
    // and the turn that ends it also leaves its handoff file (not awaited, so a
    // hung agent-exec never delays the turn's end).
    on('turn.complete', async ($, e, next) => {
        const io = ioOf($)
        if (e.agentId) {
            await releaseLeases(io, e.agentId)
            void writeHandoff(io, await io.sessionId().catch(() => ''), e.agentId, String(e.answer ?? '')).catch(() => undefined)
        } else {
            noteAnswer(await io.sessionId().catch(() => ''), String(e.answer ?? ''))
        }
        hudTurnComplete($, e)

        return next(e)
    }).catch(($, e, next) => next(e))

    on('session.end', async ($, e, next) => {
        const io = ioOf($)
        await cleanupWorktrees(io, e.sessionId, await io.root().catch(() => ''))

        return next(e)
    }).catch(($, e, next) => next(e))

    registerHud(on, options)
    registerDashboard(on)
}
