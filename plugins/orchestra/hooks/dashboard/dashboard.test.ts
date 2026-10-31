import { expect, mock, test } from 'claude-code/testing'
import type { On } from 'claude-code'

// The bottom stands for the engine: a signed-in host on a terminal, a page
// stored under "board" when given one, the status file in place, and the
// ArtifactData calls the writer makes recorded in `calls`.
const board = (on: On, { page, decision = 'allow', declined = false }: { page?: Record<string, unknown>, decision?: 'allow' | 'ask' | 'deny', declined?: boolean } = {}) => {
    const files = new Map<string, string>([['/h/.claude/orchestra/status.json', JSON.stringify({ summary: { ok: 1 }, runs: [] })]])
    const store = new Map<string, unknown>(page ? [['board', page]] : [])
    const calls: any[] = []
    const prompts: any[] = []
    const toasts: string[] = []
    const asked: any[] = []
    const done = (exitCode: number, stdout = '') =>
        ({ value: { exitCode, stdout, stderr: '', isStdoutTruncated: false, isStderrTruncated: false } })

    on('env.get', (_$, e: any) => ({ value: e.name === 'HOME' ? '/h' : undefined }))
    on('session.surfaces', () => ({ value: ['terminal'] }))
    on('session.id', () => ({ value: 's1' }))
    on('ui.toast', (_$, e: any) => {
        toasts.push(e.text)

        return { value: undefined }
    })
    on('process.run', (_$, e: any) => [...e.argv].slice(4, 6).join(' ') === 'status whoami'
        ? done(0, '{"account":"Acme","host":"mbp"}')
        : done(127))
    on('fs.exists', (_$, e: any) => ({ value: files.has(e.path) }))
    on('fs.stat', (_$, e: any) => files.has(e.path) ? { value: { kind: 'file', size: 1, mtimeMs: 1000, isLink: false } } : { deny: 'ENOENT' })
    on('fs.read', (_$, e: any) => files.has(e.path) ? { value: files.get(e.path) ?? '' } : { deny: 'ENOENT' })
    on('fs.write', (_$, e: any) => {
        files.set(e.path, e.text)

        return { value: undefined }
    })
    on('store.get', (_$, e: any) => ({ value: store.get(e.key) }))
    on('store.set', (_$, e: any) => {
        store.set(e.key, e.value)

        return { value: undefined }
    })
    on('store.delete', (_$, e: any) => {
        store.delete(e.key)

        return { value: undefined }
    })
    on('tool.list', () => ({ value: [{ name: 'ArtifactData', description: '', mcp: false }] }))
    // A query (no tool_use_id) gets `decision`; a real call's ask is settled
    // by the mode: allowed, or refused when `declined`.
    on('tool.check', (_$, e: any) => {
        if (e.tool_use_id !== undefined) {
            asked.push(e)
        }

        return { decision: e.tool_use_id === undefined || decision !== 'ask' ? decision : declined ? 'deny' : 'allow' }
    })
    on('tool.call', (_$, e: any) => {
        if (e.tool === 'Artifact') {
            return { result: { url: 'https://claude.ai/artifact/p1' } }
        }
        calls.push(e)

        return e.action === 'get'
            ? { result: { db_read: { docs: [] } } }
            : { result: { db_write: { committed: true, version: 2 } } }
    })
    on('prompt.submit', (_$, e: any) => {
        prompts.push(e)

        return { text: e.text }
    })
    const clock = mock.clock(on, { now: 1_000_000 })

    return { calls, asked, prompts, toasts, files, store, clock }
}

// The kit types `$` strictly; this passes only the fields the board reads.
const runBoard = ($: any, args: string): Promise<{ text: string }> => $.command.run({ command: 'board', args })

test('no page: /orchestra:board asks Claude to publish the page pinned', async ($, on) => {
    const { prompts, clock } = board(on)
    const answer = await runBoard($, '')
    expect(answer.text).toContain('publish this account')
    await clock.advance(10)
    expect(prompts.length).toBe(1)
    expect(prompts[0].origin.asUser).toBe(true)
    expect(prompts[0].text).toContain('/dashboard/dashboard.html')
    expect(prompts[0].text).toContain('pin true')
})

test('a publish of the page is captured and mirrored; another file is not', async ($, on) => {
    const { calls, store, clock } = board(on)
    await $.tool.call({ tool: 'Artifact', action: 'publish', file_path: '/sp/other.html' })
    expect(store.has('board')).toBe(false)
    await $.tool.call({ tool: 'Artifact', action: 'publish', file_path: '/sp/orchestra-dashboard.html', pin: true })
    expect(store.get('board')).toEqual({ url: 'https://claude.ai/artifact/p1', account: 'Acme' })
    await clock.advance(15_000)
    expect(calls.some(c => c.action === 'set' && c.collection === 'status' && c.doc_id === 'mbp')).toBe(true)
})

test('a write the permission check asks about still starts: the mode settles the ask', async ($, on) => {
    const { calls, toasts, clock } = board(on, { page: { url: 'https://claude.ai/artifact/p1', account: 'Acme' }, decision: 'ask' })
    await $.turn.start({ turnId: 't1', prompt: 'hi' } as never).catch(() => undefined)
    await clock.advance(10)
    await clock.advance(15_000)
    expect(calls.some(c => c.action === 'set' && c.collection === 'status')).toBe(true)
    expect(toasts).toEqual([])
})

test('a write the permission check denies never starts, and says so unasked', async ($, on) => {
    const { calls, toasts, files, clock } = board(on, { page: { url: 'https://claude.ai/artifact/p1', account: 'Acme' }, decision: 'deny' })
    await $.turn.start({ turnId: 't1', prompt: 'hi' } as never).catch(() => undefined)
    await clock.advance(10)
    await clock.advance(15_000)
    expect(calls).toEqual([])
    expect(toasts.length).toBe(1)
    expect(toasts[0]).toContain('denies')
    const record = JSON.parse(files.get('/h/.claude/orchestra/dashboard/blocked.json') ?? '{}')
    expect(record.session).toBe('s1')
    expect(record.at).toBe(1000)
    expect(record.reason).toContain('denies')
    expect((await runBoard($, '')).text).toContain('denies')
    expect(toasts.length).toBe(1)
})

test('a declined write stops the writer instead of asking again every few ticks', async ($, on) => {
    const { asked, toasts, clock } = board(on, { page: { url: 'https://claude.ai/artifact/p1', account: 'Acme' }, decision: 'ask', declined: true })
    await $.turn.start({ turnId: 't1', prompt: 'hi' } as never).catch(() => undefined)
    await clock.advance(10)
    await clock.advance(15_000)
    const before = asked.length
    expect(before).toBe(1)
    expect(toasts.length).toBe(1)
    expect(toasts[0]).toContain('declined')
    await clock.advance(15_000 * 10)
    expect(asked.length).toBe(before)
    expect(toasts.length).toBe(1)
})

test('a started writer clears the session\'s recorded block', async ($, on) => {
    const { files, clock } = board(on, { page: { url: 'https://claude.ai/artifact/p1', account: 'Acme' } })
    await runBoard($, '')
    await clock.advance(10)
    const record = JSON.parse(files.get('/h/.claude/orchestra/dashboard/blocked.json') ?? '{}')
    expect(record.session).toBe('s1')
    expect(record.reason).toBe(null)
})

test('forget deletes the stored page and stops the writes', async ($, on) => {
    const { calls, store, clock } = board(on, { page: { url: 'https://claude.ai/artifact/p1', account: 'Acme' } })
    await runBoard($, '')
    await clock.advance(10)
    const before = calls.length
    const answer = await runBoard($, 'forget')
    expect(answer.text).toContain('forgot')
    expect(store.has('board')).toBe(false)
    await clock.advance(15_000)
    expect(calls.length).toBe(before)
})
