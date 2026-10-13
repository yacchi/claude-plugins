import type { Io } from './io.ts'

// POSIX path handling for the guards. Comparison is by path components after
// realpath, so a symlink, a `..` escape and the sibling-prefix trap
// (`/repo-worktrees/x` is NOT inside `/repo`) all resolve correctly.

export const isAbsolute = (p: string): boolean => p.startsWith('/')

// Lexical: folds `.`, `..` and repeated separators; never touches the disk.
export const normalize = (p: string): string => {
    const out: string[] = []
    for (const part of p.split('/')) {
        if (part === '' || part === '.') {
            continue
        }
        if (part === '..') {
            out.pop()
        } else {
            out.push(part)
        }
    }

    return `/${out.join('/')}`
}

export const join = (base: string, p: string): string => (isAbsolute(p) ? p : `${base}/${p}`)

export const dirname = (p: string): string => {
    const n = normalize(p)
    const cut = n.lastIndexOf('/')

    return cut <= 0 ? '/' : n.slice(0, cut)
}

// What `os.path.realpath` gives: every existing prefix resolved through its
// symlinks, the part that does not exist yet appended lexically. Undefined
// when not even `/` resolves (a hook above withheld it): callers fail open.
export const realpath = async (io: Io, p: string): Promise<string | undefined> => {
    const stat = await io.stat(p, true).catch(() => undefined)
    if (stat?.realPath !== undefined) {
        return stat.realPath
    }
    const cut = p.replace(/\/+$/, '').lastIndexOf('/')
    if (cut < 0 || p === '/') {
        return undefined
    }
    const parent = await realpath(io, cut === 0 ? '/' : p.slice(0, cut))
    if (parent === undefined) {
        return undefined
    }
    const name = p.slice(cut + 1).replace(/\/+$/, '')

    return normalize(`${parent}/${name}`)
}

const parts = (p: string): string[] => p.split('/').filter(s => s !== '')

// Strictly inside: `parent` itself is not inside `parent`. Both already real.
export const inside = (child: string, parent: string): boolean => {
    const c = parts(child)
    const p = parts(parent)

    return c.length > p.length && p.every((s, i) => c[i] === s)
}
