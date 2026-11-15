import type { Io } from './io.ts'

// Path handling for the guards. Comparison is by path components after
// realpath, so a symlink, a `..` escape and the sibling-prefix trap
// (`/repo-worktrees/x` is NOT inside `/repo`) all resolve correctly.
//
// Two shapes, told apart by the path itself (the engine has no platform API):
// a POSIX path (`/a/b`) keeps exactly the POSIX rules -- `/` the only
// separator, case-sensitive; a Windows path (`C:\a\b`, `C:/a/b`) takes `\`
// and `/` alike, comes back with `/` separators (`C:/a/b`, which every
// Windows API accepts) and compares case-insensitively. A UNC path is not
// handled: the engine's file system never touches a network location.

// A drive-rooted path: `C:\...`, `C:/...` or a bare `C:`.
export const isWindowsPath = (p: string): boolean => /^[A-Za-z]:(?:[\\/]|$)/.test(p)

// A Windows path's root (`C:`) and the rest after it.
const windowsRoot = (p: string): { root: string, rest: string } => ({ root: p.slice(0, 2), rest: p.slice(2) })

const fold = (segments: readonly string[]): string[] => {
    const out: string[] = []
    for (const part of segments) {
        if (part === '' || part === '.') {
            continue
        }
        if (part === '..') {
            out.pop()
        } else {
            out.push(part)
        }
    }

    return out
}

// Lexical: folds `.`, `..` and repeated separators; never touches the disk.
// A Windows path keeps its root (`C:/`) and takes `/`.
export const normalize = (p: string): string => {
    if (isWindowsPath(p)) {
        const { root, rest } = windowsRoot(p)

        return `${root}/${fold(rest.split(/[\\/]/)).join('/')}`
    }

    return `/${fold(p.split('/')).join('/')}`
}

// `p` against `base` unless `p` is absolute. Against a Windows base a
// Windows path is absolute too; against a POSIX base only `/...` is, as before.
export const join = (base: string, p: string): string => {
    if (p.startsWith('/') || (isWindowsPath(base) && isWindowsPath(p))) {
        return p
    }

    return `${base}/${p}`
}

export const dirname = (p: string): string => {
    const n = normalize(p)
    if (isWindowsPath(n)) {
        const { root, rest } = windowsRoot(n)
        const cut = rest.lastIndexOf('/')

        return cut <= 0 ? `${root}/` : root + rest.slice(0, cut)
    }
    const cut = n.lastIndexOf('/')

    return cut <= 0 ? '/' : n.slice(0, cut)
}

// The last component, `\` a separator only in a Windows path.
export const basenameOf = (p: string): string => {
    const cut = isWindowsPath(p) ? Math.max(p.lastIndexOf('/'), p.lastIndexOf('\\')) : p.lastIndexOf('/')

    return p.slice(cut + 1)
}

// Whether `p` is a root: `/` or `C:/`.
export const isRoot = (p: string): boolean => dirname(p) === normalize(p)

// What `os.path.realpath` gives: every existing prefix resolved through its
// symlinks, the part that does not exist yet appended lexically. Undefined
// when not even the root resolves (a hook above withheld it): callers fail
// open. A Windows answer comes back normalized (`C:/a/b`).
export const realpath = async (io: Io, p: string): Promise<string | undefined> => {
    const stat = await io.stat(p, true).catch(() => undefined)
    if (stat?.realPath !== undefined) {
        return isWindowsPath(stat.realPath) ? normalize(stat.realPath) : stat.realPath
    }
    if (isWindowsPath(p)) {
        if (isRoot(p)) {
            return undefined
        }
        const n = normalize(p)
        const parent = await realpath(io, dirname(n))

        return parent === undefined ? undefined : normalize(`${parent}/${basenameOf(n)}`)
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

// The machine-local orchestra state directory, as agent-exec's paths.StateDir
// resolves it: $XDG_STATE_HOME/claude-orchestra when XDG_STATE_HOME is an
// absolute path, else <home>/.local/state/claude-orchestra, home being $HOME
// when absolute, else the OS home. POSIX has no other home than $HOME. On
// Windows (`windows` given, with %USERPROFILE%) only a Windows path is
// absolute, as Go's filepath.IsAbs has it -- an msys `/c/Users/x` HOME is
// not -- and the OS home is %USERPROFILE%. Undefined when no home is known.
// (No realpath: the same file either way.)
export const stateDirOf = (
    home: string | undefined,
    xdgStateHome: string | undefined,
    windows?: { userProfile: string | undefined },
): string | undefined => {
    const absolute = (p: string | undefined): p is string =>
        p !== undefined && (windows ? isWindowsPath(p) : p.startsWith('/'))
    if (absolute(xdgStateHome)) {
        return normalize(`${xdgStateHome}/claude-orchestra`)
    }
    const profile = windows?.userProfile
    const base = absolute(home) ? home : absolute(profile) ? profile : undefined
    if (base === undefined) {
        return undefined
    }

    return normalize(`${base}/.local/state/claude-orchestra`)
}

// The components; a Windows path's lowercased, `\` a separator too.
const parts = (p: string): string[] => {
    if (isWindowsPath(p)) {
        const { root, rest } = windowsRoot(p)

        return [root, ...rest.split(/[\\/]/)].filter(s => s !== '').map(s => s.toLowerCase())
    }

    return p.split('/').filter(s => s !== '')
}

// Strictly inside: `parent` itself is not inside `parent`. Both already real.
export const inside = (child: string, parent: string): boolean => {
    const c = parts(child)
    const p = parts(parent)

    return c.length > p.length && p.every((s, i) => c[i] === s)
}

// The same path: exact for POSIX, by components and case-insensitive for
// Windows. Both already real (or both normalized).
export const samePath = (a: string, b: string): boolean => {
    if (!isWindowsPath(a) && !isWindowsPath(b)) {
        return a === b
    }
    const x = parts(a)
    const y = parts(b)

    return x.length === y.length && x.every((s, i) => y[i] === s)
}
