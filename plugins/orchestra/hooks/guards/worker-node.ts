import { basename, hasEscape } from '../lib/shell.ts'

// Two fork bombs on one machine came from a worker running
// `NODE_OPTIONS=--no-experimental-strip-types node -e "...require(jest)"`.
// jest-worker forks its children with a copy of process.execArgv that keeps
// the `-e`, so every child re-runs the eval script and starts jest again: the
// process count grows exponentially until the machine stops. The NODE_OPTIONS
// override also dropped orchestra's preload (the Node CPU cap and the stop for
// forked eval children), so nothing else caught it.
//
// Denied, outside heredoc bodies and quoted data:
// 1. a NODE_OPTIONS assignment or removal that does not keep the existing
//    value (no `$NODE_OPTIONS` or `${NODE_OPTIONS` in the word). Appending
//    (`NODE_OPTIONS+=`) keeps the preload and is allowed.
// 2. a node/nodejs eval or print script (-e, -p, --eval=, --print=) that
//    names a test runner. Only the eval script counts, not other commands.
//
// Escape hatch: [orchestra:allow-node: <reason>] in the command or description.
// Every rule fails open: a throw here would disrupt every Bash call.

// Copied from lib/shell.ts, where HEREDOC is not exported: heredoc bodies are
// data, and the worker prompts that forbid these commands are written as
// heredocs themselves.
const HEREDOC = /<<-?\s*['"]?(\w+)['"]?[\s\S]*?(?:\n\1\b|$)/g

type Word = { raw: string, value: string }

const EMPTY_WORD: Word = { raw: '', value: '' }
const RESERVED = new Set(['!', '{', '}', 'if', 'then', 'elif', 'else', 'do', 'while', 'until'])
const ASSIGNMENT = /^[A-Za-z_][A-Za-z0-9_]*=/
const NODE_OPTIONS_REFERENCE = /\$NODE_OPTIONS|\$\{NODE_OPTIONS/
const DOUBLE_QUOTE_ESCAPES = new Set(['"', '\\', '$', '`'])
const EVAL_FLAGS = new Set(['-e', '--eval', '-p', '--print', '-pe', '-ep'])
const RUNNER = /\b(jest|vitest|mocha|ava|jasmine|karma|playwright)\b/
const WRAPPERS_WITH_FLAGS = new Set(['command', 'time', 'nohup'])
const MAX_WORD = 80

const at = (words: Word[], i: number): Word => words[i] ?? EMPTY_WORD

const cut = (word: string): string => word.slice(0, MAX_WORD)

// Quote-aware split of the text into simple commands, each a list of words.
// Unlike lib/shell.ts's stripping, it keeps quoted operands as values, so the
// guard reads `"$NODE_OPTIONS --x"` and the eval script as written.
const isRedirectAmpersand = (text: string, i: number): boolean =>
    text.charAt(i - 1) === '<' || text.charAt(i - 1) === '>' || text.charAt(i + 1) === '>'

const simpleCommands = (text: string): Word[][] => {
    const commands: Word[][] = []
    let current: Word[] = []
    let start = -1
    let value = ''

    const begin = (position: number): void => {
        if (start < 0) {
            start = position
        }
    }
    const endWord = (end: number): void => {
        if (start >= 0) {
            current.push({ raw: text.slice(start, end), value })
        }
        start = -1
        value = ''
    }
    const endCommand = (end: number): void => {
        endWord(end)
        if (current.length > 0) {
            commands.push(current)
        }
        current = []
    }

    let i = 0
    while (i < text.length) {
        const c = text.charAt(i)
        if (c === '\\' && text.charAt(i + 1) === '\n') {
            endWord(i)
            i += 2
        } else if (c === ' ' || c === '\t' || c === '\r') {
            endWord(i)
            i++
        } else if (c === '\n' || c === ';' || c === '|' || c === '(' || c === ')' || (c === '&' && !isRedirectAmpersand(text, i))) {
            endCommand(i)
            i++
        } else if (c === '\'') {
            begin(i)
            const close = text.indexOf('\'', i + 1)
            const stop = close < 0 ? text.length : close
            value += text.slice(i + 1, stop)
            i = stop + 1
        } else if (c === '"') {
            begin(i)
            i++
            while (i < text.length && text.charAt(i) !== '"') {
                const d = text.charAt(i)
                if (d === '\\' && DOUBLE_QUOTE_ESCAPES.has(text.charAt(i + 1))) {
                    value += text.charAt(i + 1)
                    i += 2
                } else {
                    value += d
                    i++
                }
            }
            i++
        } else if (c === '\\') {
            begin(i)
            value += text.charAt(i + 1)
            i += 2
        } else {
            begin(i)
            value += c
            i++
        }
    }
    endCommand(text.length)

    return commands
}

// `NODE_OPTIONS=` with a value that does not reference the existing one.
const replacesNodeOptions = (raw: string): boolean =>
    raw.startsWith('NODE_OPTIONS=') && !NODE_OPTIONS_REFERENCE.test(raw)

type Walk = { program: number, hit?: string }

// `env [options] [NAME=value]... program`: walks the options, checking the
// assignments and `-u`/`--unset` removals on the way. Returns the index of the
// program word and the first replacing or removing hit.
const walkEnv = (words: Word[], start: number): { next: number, hit?: string } => {
    let i = start
    let hit: string | undefined
    while (i < words.length) {
        const { raw, value } = at(words, i)
        if (ASSIGNMENT.test(raw)) {
            if (hit === undefined && replacesNodeOptions(raw)) {
                hit = cut(raw)
            }
            i++
        } else if (value === '-u' || value === '--unset') {
            if (hit === undefined && at(words, i + 1).value === 'NODE_OPTIONS') {
                hit = cut(`${raw} ${at(words, i + 1).raw}`)
            }
            i += 2
        } else if (value.startsWith('-u') || value.startsWith('--unset=')) {
            const name = value.startsWith('--unset=') ? value.slice('--unset='.length) : value.slice(2)
            if (hit === undefined && name === 'NODE_OPTIONS') {
                hit = cut(raw)
            }
            i++
        } else if (value === '-C' || value === '--chdir') {
            i += 2
        } else if (value.startsWith('-')) {
            i++
        } else {
            break
        }
    }

    return { next: i, hit }
}

// Skips the prefix assignments and wrappers in front of the program, checking
// each NODE_OPTIONS assignment on the way.
const walk = (words: Word[]): Walk => {
    let hit: string | undefined
    let i = 0
    for (;;) {
        if (i >= words.length) {
            return { program: -1, hit }
        }
        const { raw, value } = at(words, i)
        if (RESERVED.has(raw)) {
            i++
        } else if (ASSIGNMENT.test(raw)) {
            if (hit === undefined && replacesNodeOptions(raw)) {
                hit = cut(raw)
            }
            i++
        } else if (basename(value) === 'env') {
            const env = walkEnv(words, i + 1)
            hit = hit ?? env.hit
            i = env.next
        } else if (WRAPPERS_WITH_FLAGS.has(basename(value))) {
            i++
            while (i < words.length && at(words, i).value.startsWith('-')) {
                i++
            }
        } else if (basename(value) === 'exec') {
            i++
            for (;;) {
                const flag = at(words, i).value
                if (flag === '-a') {
                    i += 2
                } else if (flag === '-c' || flag === '-l') {
                    i++
                } else {
                    break
                }
            }
        } else {
            return { program: i, hit }
        }
    }
}

// Replacement or removal by the program itself: `export`, `declare -x`,
// `typeset -x` and `unset`, looking at the words after the program.
const programHit = (words: Word[], program: number): string | undefined => {
    const name = basename(at(words, program).value)
    const later = words.slice(program + 1)
    if (name === 'export' || ((name === 'declare' || name === 'typeset') && later.some(w => w.value.startsWith('-') && w.value.includes('x')))) {
        const assignment = later.find(w => ASSIGNMENT.test(w.raw) && replacesNodeOptions(w.raw))

        return assignment && cut(assignment.raw)
    }
    if (name === 'unset') {
        const removed = later.find(w => w.value === 'NODE_OPTIONS')

        return removed && cut(`unset ${removed.raw}`)
    }

    return undefined
}

const replaced = (words: Word[]): string | undefined => {
    const walked = walk(words)
    if (walked.hit !== undefined) {
        return walked.hit
    }
    if (walked.program < 0) {
        return undefined
    }

    return programHit(words, walked.program)
}

const evalRunner = (words: Word[]): string | undefined => {
    const { program } = walk(words)
    if (program < 0) {
        return undefined
    }
    const name = basename(at(words, program).value)
    if (name !== 'node' && name !== 'nodejs') {
        return undefined
    }
    const later = words.slice(program + 1)
    for (let k = 0; k < later.length; k++) {
        const { value } = at(later, k)
        let script: string | undefined
        if (EVAL_FLAGS.has(value)) {
            script = at(later, k + 1).value
        } else if (value.startsWith('--eval=') || value.startsWith('--print=')) {
            script = value.slice(value.indexOf('=') + 1)
        }
        const runner = script === undefined ? undefined : RUNNER.exec(script)?.[1]
        if (runner) {
            return runner
        }
    }

    return undefined
}

const reasonNodeOptions = (word: string): string =>
    `orchestra: this command replaces NODE_OPTIONS (${word}), which drops orchestra's preload from it (the Node CPU cap and the stop for forked eval children). Keep what is there and add yours: NODE_OPTIONS="$NODE_OPTIONS --no-experimental-strip-types" <command>. Never unset NODE_OPTIONS. If it really must be replaced, include [orchestra:allow-node: <reason>] in the command or its description.`

const reasonTestRunner = (runner: string): string =>
    `orchestra: do not start a test runner from node -e / node -p ('${runner}'): jest's workers re-run the eval script and multiply until the machine dies. Run the package's test script instead (e.g. 'pnpm --filter <pkg> test -- <path>', or 'pnpm exec jest <path>' in the package directory), or 'agent-exec check --task <id>', which runs the checks scoped to your changed files. If it really must run from an eval script, include [orchestra:allow-node: <reason>] in the command or its description.`

export const checkWorkerNode = (command: string, description: string): string | undefined => {
    if (hasEscape('node', command, description)) {
        return undefined
    }
    try {
        const commands = simpleCommands(command.replace(HEREDOC, ' '))
        for (const words of commands) {
            const word = replaced(words)
            if (word !== undefined) {
                return reasonNodeOptions(word)
            }
        }
        for (const words of commands) {
            const runner = evalRunner(words)
            if (runner !== undefined) {
                return reasonTestRunner(runner)
            }
        }

        return undefined
    } catch {
        return undefined
    }
}
