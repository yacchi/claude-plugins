import { expect, test } from 'claude-code/testing'
import { stateDirOf } from './paths.ts'

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
