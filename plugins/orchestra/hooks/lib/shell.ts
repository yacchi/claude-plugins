// Just enough shell reading for the guards. Heredoc bodies and quoted strings
// are data, not invocations: the worker prompts that FORBID these commands are
// themselves written with `cat > task.txt <<EOF ... EOF`, and a naive scan
// flags every one of them.

const HEREDOC = /<<-?\s*['"]?(\w+)['"]?[\s\S]*?(?:\n\1\b|$)/g
const QUOTED = /'[^']*'|"[^"]*"/g

export const stripData = (command: string): string =>
    command.replace(HEREDOC, ' ').replace(QUOTED, ' ')

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

export type GitCall = { globals: string, verb: string, rest: string }

export const gitCall = (segment: string): GitCall | undefined => {
    const m = GIT.exec(segment.trim())

    return m ? { globals: m[1] ?? '', verb: m[2] ?? '', rest: m[3] ?? '' } : undefined
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
