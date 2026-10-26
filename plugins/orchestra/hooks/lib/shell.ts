// Just enough shell reading for the guards. Heredoc bodies and quoted strings
// are data, not invocations: the worker prompts that FORBID these commands are
// themselves written with `cat > task.txt <<EOF ... EOF`, and a naive scan
// flags every one of them.

const HEREDOC = /<<-?\s*['"]?(\w+)['"]?[\s\S]*?(?:\n\1\b|$)/g
const QUOTED = /'[^']*'|"[^"]*"/g

export const stripData = (command: string): string =>
    command.replace(HEREDOC, ' ').replace(QUOTED, ' ')

// A double-quoted string with `$` or a backtick is a variable operand, not
// data, so it survives as a word. One pass over the alternation keeps a
// single-quoted string that holds a `"` as data.
const QUOTED_VAR = /'[^']*'|"[^"]*"/g

export const stripDataKeepingVars = (command: string): string =>
    command.replace(HEREDOC, ' ').replace(QUOTED_VAR, q => q.startsWith('"') && /[$`]/.test(q) ? '$QUOTED' : ' ')

// One simple command per segment, split on && || ; & | and newlines.
// The `&` of a redirection (`2>&1`, `>&2`, `<&3`, `&> f`) does not end a
// command.
export const segments = (body: string): string[] =>
    body.split(/(?:\|\||&&|[;|\n]|(?<![<>])&(?!>))/)

export const words = (text: string): string[] =>
    text.trim().split(/\s+/).filter(w => w !== '')

// `git`, possibly with global options (-C <path>, --git-dir=..., -c k=v), then
// the verb and the rest.
const GIT = /^git\b((?:\s+(?:-C\s+\S+|--git-dir=\S+|--work-tree=\S+|-c\s+\S+))*)\s+(\S+)(.*)$/

export type GitCall = { globals: string, verb: string, rest: string, viaXargs: boolean }

// Words that run another command as their argument, so the git call follows them.
const WRAPPER_WORDS = new Set(['do', 'then', 'else', 'elif', 'time', 'nohup', 'command', 'exec', 'env'])
const PUNCTUATION = /^[({!]\s*/
const ASSIGNMENT = /^[A-Za-z_][A-Za-z0-9_]*=\S*$/
// xargs options that take a separate value word; the value must not read as the verb.
const XARGS_VALUE_OPTIONS = new Set(['-n', '-I', '-L', '-P', '-d', '-E', '-s'])

// The first word and the text after it.
const splitWord = (text: string): [string, string] => {
    const m = /^(\S*)\s*([\s\S]*)$/.exec(text)

    return [m?.[1] ?? '', m?.[2] ?? '']
}

const skipXargsOptions = (text: string): string => {
    let rest = text
    for (;;) {
        const [word, after] = splitWord(rest)
        if (!word.startsWith('-')) {
            return rest
        }
        rest = XARGS_VALUE_OPTIONS.has(word) ? splitWord(after)[1] : after
    }
}

export const gitCall = (segment: string): GitCall | undefined => {
    let text = segment.trim()
    let viaXargs = false
    for (;;) {
        const punctuation = PUNCTUATION.exec(text)
        if (punctuation) {
            text = text.slice(punctuation[0].length)
            continue
        }
        const [word, after] = splitWord(text)
        if (WRAPPER_WORDS.has(word) || ASSIGNMENT.test(word)) {
            text = after
            continue
        }
        if (word === 'xargs') {
            viaXargs = true
            text = skipXargsOptions(after)
            continue
        }
        break
    }

    const m = GIT.exec(text)
    if (!m) {
        return undefined
    }

    return { globals: m[1] ?? '', verb: m[2] ?? '', rest: (m[3] ?? '').replace(/[)}]$/, ''), viaXargs }
}

// Output redirection targets (`> PATH`, `>> PATH`, `2> PATH`, `&> PATH`),
// `>&2` excluded.
export const redirections = (segment: string): string[] => {
    const out: string[] = []
    for (const m of segment.matchAll(/(?:^|\s)(?:[0-9]*|&)>>?\s*([^\s;&|<>()]+)/g)) {
        const tok = m[1]
        if (tok && !tok.startsWith('&')) {
            out.push(tok)
        }
    }

    return out
}

export const basename = (p: string): string => p.slice(p.lastIndexOf('/') + 1)

// `[orchestra:allow-<name>]` or `[orchestra:allow-<name>: <reason>]`.
export const hasEscape = (name: string, ...texts: string[]): boolean =>
    new RegExp(`\\[orchestra:allow-${name}(?::[^\\]]*)?\\]`).test(texts.join('\n').toLowerCase())
