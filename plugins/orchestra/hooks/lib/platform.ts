import type { Io } from './io.ts'
import { isWindowsPath, normalize } from './paths.ts'

// What differs on Windows, where Claude Code runs without HOME and with only
// Git's cmd/ on PATH (no sh, mv, rm, ps). The engine has no platform API, so
// the plugin's own root tells: a Windows path there means Windows.

export const onWindows = (io: Pick<Io, 'pluginRoot'>): boolean => {
    try {
        return isWindowsPath(io.pluginRoot())
    } catch {
        return false
    }
}

// The user's home as `~` and $HOME mean it: $HOME on POSIX; on Windows $HOME
// when it is a Windows path (not an msys `/c/Users/x`), else %USERPROFILE%.
export const homeOf = async (io: Pick<Io, 'env' | 'pluginRoot'>): Promise<string | undefined> => {
    const home = await io.env('HOME')
    if (!onWindows(io)) {
        return home
    }
    if (home !== undefined && isWindowsPath(home)) {
        return home
    }
    const profile = await io.env('USERPROFILE')

    return profile !== undefined && isWindowsPath(profile) ? profile : undefined
}

// The state dir's inputs as lib/paths.ts stateDirOf takes them.
export const stateDirArgs = async (io: Pick<Io, 'env' | 'pluginRoot'>): Promise<[string | undefined, string | undefined, { userProfile: string | undefined } | undefined]> => [
    await io.env('HOME'),
    await io.env('XDG_STATE_HOME'),
    onWindows(io) ? { userProfile: await io.env('USERPROFILE') } : undefined,
]

// Git for Windows' root (`C:/Program Files/Git`), from `git --exec-path`
// (`<root>/mingw64/libexec/git-core`). Kept once found, per module load; a
// failed lookup is asked again next time.
let gitRoot: string | undefined
const tools = new Map<string, string>()

const gitRootOf = async (io: Pick<Io, 'run'>): Promise<string | undefined> => {
    if (gitRoot !== undefined) {
        return gitRoot
    }
    const r = await io.run(['git', '--exec-path'], { timeoutMs: 5000 }).catch(() => undefined)
    const exec = r?.exitCode === 0 ? r.stdout.trim() : ''
    const m = /^(.+?)[\\/]+[^\\/]+[\\/]+libexec[\\/]+git-core[\\/]*$/i.exec(exec)
    if (!m?.[1] || !isWindowsPath(m[1])) {
        return undefined
    }
    gitRoot = normalize(m[1])

    return gitRoot
}

// The program to run for a POSIX tool: the bare name on POSIX (PATH finds
// it); on Windows Git's own copy, `sh` as <git>/bin/sh.exe (which puts
// usr/bin on PATH for the script it runs) and any other as
// <git>/usr/bin/<name>.exe. Undefined on Windows when Git or the tool is not
// found.
export const toolOf = async (io: Pick<Io, 'run' | 'exists' | 'pluginRoot'>, name: string): Promise<string | undefined> => {
    if (!onWindows(io)) {
        return name
    }
    const kept = tools.get(name)
    if (kept !== undefined) {
        return kept
    }
    const root = await gitRootOf(io)
    if (root === undefined) {
        return undefined
    }
    const path = name === 'sh' ? `${root}/bin/sh.exe` : `${root}/usr/bin/${name}.exe`
    if (!await io.exists(path).catch(() => false)) {
        return undefined
    }
    tools.set(name, path)

    return path
}
