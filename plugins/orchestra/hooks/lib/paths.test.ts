import { expect, test } from 'claude-code/testing'
import { basenameOf, dirname, inside, isRoot, isWindowsPath, join, normalize, samePath, stateDirOf } from './paths.ts'

// The same table as internal/paths/paths_test.go: agent-exec and the hooks
// must resolve the machine-local state dir to the same path.
test('stateDirOf: XDG_STATE_HOME when absolute, else $HOME/.local/state', () => {
    expect(stateDirOf('/h', '/x/state')).toBe('/x/state/claude-orchestra')
    expect(stateDirOf('/h', '/x/state/')).toBe('/x/state/claude-orchestra')
    expect(stateDirOf('/h', 'relative/state')).toBe('/h/.local/state/claude-orchestra')
    expect(stateDirOf('/h', '')).toBe('/h/.local/state/claude-orchestra')
    expect(stateDirOf('/h', undefined)).toBe('/h/.local/state/claude-orchestra')
    expect(stateDirOf(undefined, undefined)).toBeUndefined()
    expect(stateDirOf('', undefined)).toBeUndefined()
})

// On Windows (Claude Code runs without HOME there) the home falls back to
// %USERPROFILE%, and only a drive path is absolute, as Go's filepath.IsAbs.
test('stateDirOf on Windows: a drive-path HOME, else %USERPROFILE%', () => {
    const profile = { userProfile: 'C:\\Users\\u' }
    expect(stateDirOf(undefined, undefined, profile)).toBe('C:/Users/u/.local/state/claude-orchestra')
    expect(stateDirOf('D:\\h', undefined, profile)).toBe('D:/h/.local/state/claude-orchestra')
    expect(stateDirOf('/c/Users/u', undefined, profile), 'an msys HOME is not absolute').toBe('C:/Users/u/.local/state/claude-orchestra')
    expect(stateDirOf(undefined, 'C:\\x\\state', profile)).toBe('C:/x/state/claude-orchestra')
    expect(stateDirOf(undefined, '/x/state', profile)).toBe('C:/Users/u/.local/state/claude-orchestra')
    expect(stateDirOf(undefined, undefined, { userProfile: undefined })).toBeUndefined()
    expect(stateDirOf('/h', undefined), 'POSIX ignores the Windows rules').toBe('/h/.local/state/claude-orchestra')
})

test('POSIX paths keep the POSIX rules', () => {
    expect(isWindowsPath('/w/repo')).toBe(false)
    expect(normalize('/w//repo/./src/../a')).toBe('/w/repo/a')
    expect(normalize('//w/repo')).toBe('/w/repo')
    expect(join('/w/repo', 'src/a.ts')).toBe('/w/repo/src/a.ts')
    expect(join('/w/repo', '/x')).toBe('/x')
    expect(join('/w/repo', 'C:/x'), 'a drive path is relative against a POSIX base').toBe('/w/repo/C:/x')
    expect(dirname('/w/repo')).toBe('/w')
    expect(dirname('/w')).toBe('/')
    expect(isRoot('/')).toBe(true)
    expect(isRoot('/w')).toBe(false)
    expect(basenameOf('/w/a\\b')).toBe('a\\b')
    expect(inside('/w/repo/src', '/w/repo')).toBe(true)
    expect(inside('/w/repo-worktrees/x', '/w/repo')).toBe(false)
    expect(inside('/w/Repo/src', '/w/repo'), 'case-sensitive').toBe(false)
    expect(samePath('/w/Repo', '/w/repo')).toBe(false)
})

test('Windows paths: either separator, `/` out, case-insensitive', () => {
    expect(isWindowsPath('C:\\w')).toBe(true)
    expect(isWindowsPath('c:/w')).toBe(true)
    expect(isWindowsPath('C:')).toBe(true)
    expect(isWindowsPath('C:x')).toBe(false)
    expect(normalize('C:\\w\\repo\\.\\src\\..\\a')).toBe('C:/w/repo/a')
    expect(normalize('C:\\')).toBe('C:/')
    expect(join('C:\\w\\repo', 'src\\a.ts')).toBe('C:\\w\\repo/src\\a.ts')
    expect(normalize(join('C:\\w\\repo', 'src\\a.ts'))).toBe('C:/w/repo/src/a.ts')
    expect(join('C:\\w\\repo', 'D:\\x')).toBe('D:\\x')
    expect(dirname('C:\\w\\repo')).toBe('C:/w')
    expect(dirname('C:\\w')).toBe('C:/')
    expect(dirname('C:/')).toBe('C:/')
    expect(isRoot('C:\\')).toBe(true)
    expect(isRoot('C:/w')).toBe(false)
    expect(basenameOf('C:\\w\\a.html')).toBe('a.html')
    expect(inside('c:/W/Repo/src', 'C:\\w\\repo')).toBe(true)
    expect(inside('C:/w/repo-worktrees/x', 'C:/w/repo')).toBe(false)
    expect(inside('D:/w/repo/src', 'C:/w/repo')).toBe(false)
    expect(inside('C:/w/repo', 'C:/w/repo')).toBe(false)
    expect(samePath('C:\\W\\Repo', 'c:/w/repo')).toBe(true)
    expect(samePath('C:/w/repo', 'C:/w/repo/x')).toBe(false)
})
