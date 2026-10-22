import { agentExec } from './config.ts'
import type { Io } from './io.ts'

// Every Bash command a Claude worker runs, as one record in agent-exec's
// trace (kind worker-bash), so shell load can be read next to the git, fs and
// herdr operations agent-exec itself records. Fire-and-forget: a hook that
// fails or stalls would disrupt every tool call of every session.

const SUBCOMMAND_PROGRAMS = new Set(['git', 'pnpm', 'npm', 'yarn', 'npx', 'go', 'cargo', 'make', 'docker', 'mise'])

// Flags that take their value as the next word, so the value is never read as
// the subcommand (`git -C /x status`, `pnpm --filter x test`).
const VALUE_FLAGS = new Set([
    '-C', '-c', '--git-dir', '--work-tree', '--namespace', '--filter', '-F', '--dir', '--prefix',
    '--cwd', '--manifest-path', '--file', '-f', '--package', '-p', '--project', '--host', '-H',
    '--context', '--directory', '--workspace', '-w',
])

const ASSIGNMENT = /^[A-Za-z_][A-Za-z0-9_]*=/
const CLIP = 300
const TIMEOUT_MS = 3000

// The first simple command of a line: `cd <dir> &&` / `;` prefixes dropped,
// cut at the first pipe, `&&`, `||` or `;` after that.
const firstCommand = (command: string): string[] => {
    let rest = command.trim()
    for (;;) {
        const m = /^cd\s+(?:"[^"]*"|'[^']*'|\S+)\s*(?:&&|;)\s*/.exec(rest)
        if (!m) {
            break
        }
        rest = rest.slice(m[0].length)
    }

    return rest.split(/\|\||&&|[|;\n]/)[0]!.trim().split(/\s+/).filter(w => w !== '')
}

const WRAPPER_VALUE_FLAGS: Readonly<Record<string, readonly string[]>> = {
    sudo: ['-u', '-g', '-h', '-p', '-C', '-D', '-R', '-T'],
    nice: ['-n'],
    timeout: ['-s', '-k', '--signal', '--kill-after'],
    env: ['-u', '-C', '-S', '--unset', '--chdir'],
}

const stripWrappers = (words: readonly string[]): string[] => {
    let w = [...words]
    for (;;) {
        const head = w[0]
        if (head === undefined) {
            return w
        }
        if (ASSIGNMENT.test(head)) {
            w = w.slice(1)
            continue
        }
        const base = head.slice(head.lastIndexOf('/') + 1)
        const valueFlags = WRAPPER_VALUE_FLAGS[base]
        if (base === 'time' || base === 'exec') {
            w = w.slice(1)
        } else if (valueFlags) {
            // Drop the wrapper, its flags (and their values) and, for
            // timeout, its duration; env's assignments fall out in the loop.
            let i = 1
            while (w[i] !== undefined && w[i]!.startsWith('-')) {
                i += valueFlags.includes(w[i]!) ? 2 : 1
            }
            if (base === 'timeout') {
                i += 1
            }
            w = w.slice(i)
        } else {
            return w
        }
    }
}

// "git -C /x status --porcelain" -> "git status"; "rm -rf x" -> "rm"; "" -> "bash".
export const bashOp = (command: string): string => {
    const words = stripWrappers(firstCommand(command))
    const head = words[0]
    if (head === undefined) {
        return 'bash'
    }
    const program = head.slice(head.lastIndexOf('/') + 1) || head
    if (!SUBCOMMAND_PROGRAMS.has(program)) {
        return program
    }
    for (let i = 1; i < words.length; i++) {
        const word = words[i]!
        if (!word.startsWith('-')) {
            return `${program} ${word}`
        }
        if (VALUE_FLAGS.has(word)) {
            i++
        }
    }

    return program
}

export type WorkerBash = {
    startMs: number,
    endMs: number,
    command: string,
    cwd?: string,
    isError: boolean,
    sessionId?: string,
}

// Never throws; resolves once agent-exec has answered (or timed out).
export const recordWorkerBash = async (io: Io, b: WorkerBash): Promise<void> => {
    try {
        const at = b.startMs / 1000
        const record = {
            at,
            end: Math.max(at, b.endMs / 1000),
            ms: Math.max(0, Math.round(b.endMs - b.startMs)),
            kind: 'worker-bash',
            op: bashOp(b.command),
            pid: 0,
            ...(b.sessionId ? { pkg: b.sessionId } : {}),
            ...(b.cwd ? { cwd: b.cwd } : {}),
            exit: b.isError ? 1 : 0,
            load1: 0,
            detail: [...b.command].slice(0, CLIP).join(''),
        }
        await agentExec(io, ['trace', 'record', '--json', JSON.stringify(record)], TIMEOUT_MS)
    } catch {
        // Tracing must never disturb the call it measures.
    }
}
