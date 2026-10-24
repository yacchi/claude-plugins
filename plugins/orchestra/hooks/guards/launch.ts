import type { Io } from '../lib/io.ts'
import { field } from '../lib/agents.ts'
import { basename, hasEscape, segments, stripData, words } from '../lib/shell.ts'

// Long-running orchestra work must not start from a shell. A wave run or a
// `claude -p` planner launched from Bash (backgrounded or not) is invisible
// to Orca, to remote viewers and to the subagent list; a blocking dispatch
// dies at the Bash tool's 600 s limit with nothing to watch. The skill
// forbade all three in prose, and the instructor still launched two of them
// from its own Bash in one session.
//
// Deny, for Bash calls from the main thread and every subagent alike
// (`run_in_background` changes nothing):
//   - `agent-exec wave run`, except `--detach` from an `orchestra-wave`
//     subagent, the one place a wave is meant to start;
//   - `claude -p` / `--print`;
//   - `agent-exec dispatch` without `--detach` (its subcommands excepted).
// `--help` is always allowed. Quoted and heredoc text is data, not an
// invocation, so a prompt that mentions these commands passes.
//
// Escape hatches: [orchestra:allow-shell-launch(: reason)] in the command or
// description; ORCHESTRA_LAUNCH_GUARD=off; fail-open on anything unexpected.

type Hit = 'none' | 'wave' | 'wave-attached' | 'print' | 'dispatch'

// Prefixes that run the next word as the command.
const WRAPPERS = new Set(['env', 'exec', 'command', 'nohup', 'time'])

const DISPATCH_SUBCOMMANDS = new Set(['prepare', 'wait', 'status'])

// The command word and its arguments, past env assignments, wrappers and
// grouping punctuation.
const invocation = (segment: string): string[] => {
    const ws = words(segment.replace(/^[\s({!]+/, ''))
    let i = 0
    while (i < ws.length && (/^[A-Za-z_][A-Za-z0-9_]*=/.test(ws[i] ?? '') || WRAPPERS.has(ws[i] ?? ''))) {
        i++
    }

    return ws.slice(i)
}

const isWaveAgent = (agentType: string): boolean => {
    const t = agentType.toLowerCase()

    return t === 'orchestra-wave' || t.endsWith(':orchestra-wave')
}

const classify = (segment: string, waveAgent: boolean): Hit => {
    const [bin, ...args] = invocation(segment)
    const name = basename(bin ?? '')
    if (args.some(a => a === '--help' || a === '-h')) {
        return 'none'
    }
    if (name === 'claude') {
        return args.some(a => a === '-p' || a === '--print' || a.startsWith('--print=')) ? 'print' : 'none'
    }
    if (name !== 'agent-exec') {
        return 'none'
    }
    // The command is the first positional word, its subcommand the next one
    // after it. A flag value read as a positional only ever makes the call
    // look like something else, which lets it through.
    const next = (from: number): string | undefined => args.slice(from).find(a => !a.startsWith('-'))
    const cmd = next(0)
    const sub = cmd === undefined ? undefined : next(args.indexOf(cmd) + 1)
    const detached = args.some(a => a === '--detach' || a === '--detach=true')
    if (cmd === 'wave' && sub === 'run') {
        if (!waveAgent) {
            return 'wave'
        }

        return detached ? 'none' : 'wave-attached'
    }
    if (cmd === 'dispatch' && !DISPATCH_SUBCOMMANDS.has(sub ?? '')) {
        return detached ? 'none' : 'dispatch'
    }

    return 'none'
}

const ESCAPE = 'To override deliberately, include [orchestra:allow-shell-launch: <reason>] in the command or its description.'

const reason = (hit: Hit, command: string): string => {
    switch (hit) {
        case 'wave':
            return `orchestra: do not start a wave from a shell. A wave launched from Bash (in the background or not) is invisible in Orca, in remote sessions and in the subagent list. Spawn an 'orchestra:orchestra-wave' subagent with the Agent tool instead (description 'wave <run-id>: <N> packages') and give it this exact command line: ${command.trim()} ${ESCAPE}`
        case 'wave-attached':
            return `orchestra: run the wave detached: add --detach to 'agent-exec wave run' and follow it with 'agent-exec wave status'. An attached run blocks this agent and dies at the Bash tool's 600 s limit. ${ESCAPE}`
        case 'print':
            return `orchestra: do not run 'claude -p'/'--print' from a shell; a headless claude is invisible in Orca, in remote sessions and in the subagent list. Spawn the work as a subagent with the Agent tool. If the agent type is not installed yet, pass its definition body as the prompt to a general-purpose agent with the same model and effort. ${ESCAPE}`
        default:
            return `orchestra: 'agent-exec dispatch' without --detach blocks for minutes with nothing to watch and dies at the Bash tool's 600 s limit. Use the detached hops instead: 'agent-exec dispatch prepare' -> 'agent-exec dispatch --token <token> --capture --detach' -> 'agent-exec dispatch wait --token <token>'. ${ESCAPE}`
    }
}

// `agentType` is empty for the main thread.
export const checkLaunch = async (
    io: Io,
    input: Readonly<Record<string, unknown>>,
    agentType: string,
): Promise<string | undefined> => {
    const command = field(input, 'command')
    const waveAgent = isWaveAgent(agentType)
    const hit = segments(stripData(command)).map(s => classify(s, waveAgent)).find(h => h !== 'none')
    if (!hit) {
        return undefined
    }
    // An unreadable switch fails open like everything else here.
    if (await io.env('ORCHESTRA_LAUNCH_GUARD').catch(() => 'off') === 'off') {
        return undefined
    }
    if (hasEscape('shell-launch', command, field(input, 'description'))) {
        return undefined
    }

    return reason(hit, command)
}
