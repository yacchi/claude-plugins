import type { Io } from '../lib/io.ts'
import { dirname } from '../lib/paths.ts'
import { leaseFile } from '../guards/worktree-lease.ts'
import { isInstructorModel, setVerdict } from './verdict.ts'

// SessionStart (startup, resume, clear, compact): inject the routing protocol.
//
// A hosted worker (a task worktree launched by `agent-exec wave`) gets the
// worker notice instead, whatever its model: the instructor already routed it.
//
// Hybrid gate on the input's `model` (documented as omittable):
//   Opus/Fable -> the router, no self-gate paragraph (already gated here)
//   any other  -> nothing: zero context cost on cheap sessions
//   missing    -> the full self-gated router (safe fallback)
// The verdict is kept per session for the hooks whose input lacks the model.

const BODY = `Classify EACH user request before acting.

EXPRESS — ALL of: one self-contained change (or conversational/read-only/a
question); no decomposition, no design decisions, no cross-task coordination;
small expected context bloat. File count is NOT a criterion.
→ Do it yourself, or via cheap subagents (haiku/sonnet), in parallel when the
  parts are independent. No verifier pipeline, no workflow. Hand the test/build run to a cheap subagent and read
  back only its verdict. Name the tier: a subagent call that leaves \`model\`
  out inherits YOUR model (Opus/Fable), so pass \`model: "sonnet"\` (or
  "haiku") explicitly for investigation, search, and test runs.
The gate is context bloat, NOT your model tier. Before judging size, ask "do I
already hold this content?" — if writing the delegation prompt would mean
re-authoring a spec you just wrote, delegating buys nothing. That exemption is
about content you already hold, NOT about volume: it does not license an
open-ended build, and it expires the moment the work outgrows what you held.

ORCHESTRATED — anything else, and WHENEVER IN DOUBT.
→ Load the \`orchestra:run\` skill and follow it: decompose, write contracts,
  delegate to cheap workers with adversarial verifiers, take back only
  structured verdicts.

WORKFLOW OPT-IN — judge it from the context alone; never read settings files.
The Workflow tool is usable this turn only when the context shows an opt-in: a
system-reminder that ultracode is on or that the user's prompt carried the
ultracode keyword; the user asking for a workflow in their own words; or the
user having typed /orchestra:run (or /run) themselves. A skill the router
loaded does not count. EXPRESS work never becomes a workflow, even with
ultracode on. ORCHESTRATED work a Workflow fits, with no opt-in visible: do not
fall back to orchestra-delegate silently — reply in one line asking the user
to type \`/orchestra:run <the request>\` or add "workflow" to the request, then
stop. Small ORCHESTRATED work may still go to Agent-tool workers directly.
Never inject or rewrite a prompt to manufacture an opt-in.

A lane never persists — re-classify every request and at every phase shift.
Forced ORCHESTRATED, even mid-conversation: you are about to hand-write
substantive code you have not already worked out in this conversation; a
design discussion just turned into
"implement it" / 「PRを出しましょう」/「直しておきましょう」; or you are planning your
own multi-step implement-then-verify sequence. Then STOP and load
\`orchestra:run\` first.

ABORT RULE — the lane is wrong more often mid-turn than at the start, because
the prompts that produce the biggest turns are the smallest ones ("作業を続けて",
"実装して", "y"). So re-judge while working, not only when asked: if a turn you
began as EXPRESS keeps sprouting new subtasks or is many files deep, STOP.
Finishing the edit already open is fine; starting the next chunk by hand is
not — re-route it through \`orchestra:run\` instead.
</orchestra-router>
`

const SELF_GATE = `<orchestra-router>
This protocol applies ONLY when this session's main model is Opus or Fable
(check the model named in your system prompt; on Sonnet/Haiku, ignore this
entire block).

`

const SOURCES = new Set(['startup', 'resume', 'clear', 'compact'])

const workerNotice = (tree: string): string => `<orchestra-worker>
This session is an orchestra worker hosted in the task worktree ${tree}.
Do the work your prompt names yourself, in this tree. Do not load
\`orchestra:run\`, call the Workflow tool, spawn implementation subagents, or run
\`agent-exec dispatch\` / \`agent-exec wave\`: the instructor that sent the prompt
is already orchestrating. When the task cannot be done as specified, report
ESCALATE the way your prompt says instead.
</orchestra-worker>
`

// The task worktree a worker session is hosted in: a LINKED worktree whose git
// dir carries the orchestra baseline or adopted marker, and whose role is not
// `integration`. Anything unexpected reads as not a worker.
const workerTree = async (io: Io, cwd: string): Promise<string | undefined> => {
    try {
        const found = cwd ? await leaseFile(io, cwd) : undefined
        if (!found) {
            return undefined
        }
        const gitDir = dirname(found.file)
        const marked = await io.exists(`${gitDir}/orchestra-baseline`) || await io.exists(`${gitDir}/orchestra-adopted`)
        if (!marked) {
            return undefined
        }
        const role = `${gitDir}/orchestra-role`
        if (await io.exists(role) && (await io.read(role)).trim() === 'integration') {
            return undefined
        }

        return found.top
    } catch {
        return undefined
    }
}

// Records the session's verdict and answers the text to inject, if any.
export const routerContext = async (
    io: Io,
    source: string,
    sessionId: string,
    rawModel: unknown,
    rawCwd: unknown,
): Promise<string | undefined> => {
    if (!SOURCES.has(source)) {
        return undefined
    }
    const cwd = typeof rawCwd === 'string' && rawCwd ? rawCwd : await io.sessionCwd().catch(() => '')
    const tree = await workerTree(io, cwd)
    if (tree !== undefined) {
        setVerdict(sessionId, 'worker')

        return workerNotice(tree)
    }
    const model = typeof rawModel === 'string' ? rawModel.trim() : ''
    if (!model) {
        setVerdict(sessionId, 'unknown')

        return SELF_GATE + BODY
    }
    if (isInstructorModel(model)) {
        setVerdict(sessionId, 'instructor')

        return `<orchestra-router>\n${BODY}`
    }
    setVerdict(sessionId, 'cheap')

    return undefined
}
