import { describe, expect, test } from 'claude-code/testing'
import type { ToolCallResult } from 'claude-code'
import { type BoardTarget, createWriter, docIdOf, pageKeyOf, type WriterHost } from './writer.ts'

const HOME = '/home/u'
const STATUS = `${HOME}/.claude/orchestra/status.json`
const CLAIM = `${HOME}/.claude/orchestra/dashboard/ab-12.json`
const target: BoardTarget = { url: 'https://claude.ai/artifact/ab-12', host: 'my.host', account: 'Acme', home: HOME }

// A host whose files, clock and status mtime the test sets, and whose
// ArtifactData answers come from a queue (a function gets the call's input).
type Fake = {
    files: Map<string, string>,
    calls: Record<string, unknown>[],
    answers: unknown[],
    mtimeMs: number,
    now: number,
    host: WriterHost,
}

const fakeHost = (): Fake => {
    const files = new Map<string, string>()
    const calls: Record<string, unknown>[] = []
    const answers: unknown[] = []
    const fake: Fake = {
        files,
        calls,
        answers,
        mtimeMs: 1000,
        now: 10_000_000,
        host: {
            nowMs: async () => fake.now,
            stat: async (path: string) => {
                if (!files.has(path)) {
                    throw new Error('ENOENT')
                }

                return { mtimeMs: fake.mtimeMs }
            },
            read: async (path: string) => {
                const text = files.get(path)
                if (text === undefined) {
                    throw new Error('ENOENT')
                }

                return text
            },
            write: async (path: string, text: string) => {
                files.set(path, text)
            },
            artifactData: async (input: Readonly<Record<string, unknown>>) => {
                calls.push({ ...input })
                const next = answers.shift()

                return (typeof next === 'function' ? next(input) : next) as ToolCallResult
            },
        },
    }

    return fake
}

const status = (line = '1 running', heartbeat_s = 60) => JSON.stringify({
    version: 1,
    heartbeat_s,
    summary: { line, busy: true },
    runs: [{ key: 'k1', status: 'running', pid: 42, state: '/x/state.json', title: 'wave' }],
})

const EMPTY_GET = { result: { db_read: { docs: [] } } }
const COMMITTED = (version: number) => ({ result: { db_write: { committed: true, version } } })

describe('board writer', () => {
    test('a first write sends no if_version, strips pid and state, and records the claim', async () => {
        const h = fakeHost()
        h.files.set(STATUS, status())
        h.answers.push(EMPTY_GET, COMMITTED(4))
        const writer = createWriter(target, h.host)

        expect(await writer.tick()).toBe('written')
        expect(h.calls).toHaveLength(2)
        expect(h.calls[0]).toEqual({ action: 'get', url: target.url, collection: 'status', doc_id: 'my_host' })
        expect(h.calls[1]).not.toHaveProperty('if_version')
        expect(h.calls[1].doc_id).toBe(docIdOf('my.host'))

        const data = h.calls[1].data as { status: { runs: Record<string, unknown>[] }, writer: unknown }
        expect(data.status.runs[0]).not.toHaveProperty('pid')
        expect(data.status.runs[0]).not.toHaveProperty('state')
        expect(data.status.runs[0].title).toBe('wave')
        expect(data.writer).toEqual({ account: 'Acme', at: 10_000 })
        expect(JSON.parse(h.files.get(CLAIM) ?? 'null')).toEqual({ mtime_ms: 1000, written_ms: 10_000_000, line: '1 running', version: 4 })
    })

    test('an unchanged status file makes no calls on the next tick', async () => {
        const h = fakeHost()
        h.files.set(STATUS, status())
        h.answers.push(EMPTY_GET, COMMITTED(4))
        const writer = createWriter(target, h.host)
        await writer.tick()
        const before = h.calls.length

        expect(await writer.tick()).toBe('unchanged')
        expect(h.calls).toHaveLength(before)
    })

    test('another session\'s claim is mirrored elsewhere, and its version is reused for the next write', async () => {
        const h = fakeHost()
        h.files.set(STATUS, status())
        h.files.set(CLAIM, JSON.stringify({ mtime_ms: 1000, written_ms: 9_000_000, line: '1 running', version: 9 }))
        const writer = createWriter(target, h.host)

        expect(await writer.tick()).toBe('mirrored-elsewhere')
        expect(h.calls).toHaveLength(0)

        h.mtimeMs = 2000
        h.files.set(STATUS, status('2 running'))
        h.answers.push(COMMITTED(10))
        h.files.set(CLAIM, JSON.stringify({ mtime_ms: 500, written_ms: 9_000_000, line: '1 running', version: 9 }))

        expect(await writer.tick()).toBe('written')
        expect(h.calls).toHaveLength(1)
        expect(h.calls[0]).toMatchObject({ action: 'set', if_version: 9 })
    })

    test('the same summary line within the heartbeat is not due; a changed line is written at once', async () => {
        const h = fakeHost()
        h.files.set(STATUS, status())
        h.files.set(CLAIM, JSON.stringify({ mtime_ms: 500, written_ms: 10_000_000 - 30_000, line: '1 running', version: 4 }))
        const writer = createWriter(target, h.host)

        expect(await writer.tick()).toBe('not-due')
        expect(h.calls).toHaveLength(0)

        h.files.set(STATUS, status('2 running'))
        h.mtimeMs = 1500
        h.answers.push(COMMITTED(5))

        expect(await writer.tick()).toBe('written')
        expect(h.calls).toHaveLength(1)
        expect(h.calls[0]).toMatchObject({ action: 'set', if_version: 4 })
    })

    test('a failed set backs off for two ticks, then a due tick gets before it sets', async () => {
        const h = fakeHost()
        h.files.set(STATUS, status())
        h.answers.push(EMPTY_GET, { isError: true, text: 'version mismatch' }, EMPTY_GET, COMMITTED(6))
        const writer = createWriter(target, h.host)

        expect(await writer.tick()).toBe('failed')
        const outcomes = [await writer.tick(), await writer.tick(), await writer.tick()]
        expect(outcomes).toEqual(['backoff', 'backoff', 'written'])
        expect(h.calls.map(c => c.action)).toEqual(['get', 'set', 'get', 'set'])
    })

    test('an "another of the user\'s organizations" refusal is refused, in deny or only inside the result', async () => {
        const phrase = 'this Artifact is in another of the user\'s organizations'
        const h = fakeHost()
        h.files.set(STATUS, status())
        h.answers.push(EMPTY_GET, { deny: `Refused: ${phrase}` }, EMPTY_GET, { result: { db_write: { committed: false, reason: phrase } } })
        const writer = createWriter(target, h.host)

        expect(await writer.tick()).toBe('refused')
        h.mtimeMs = 1500
        expect(await writer.tick()).toBe('refused')
    })

    test('a set that loses the race to another session is mirrored elsewhere, with no backoff', async () => {
        const h = fakeHost()
        h.files.set(STATUS, status())
        h.answers.push(EMPTY_GET, () => {
            h.files.set(CLAIM, JSON.stringify({ mtime_ms: 1000, written_ms: 10_000_000, line: '1 running', version: 7 }))

            return { isError: true, text: 'version mismatch' }
        })
        const writer = createWriter(target, h.host)

        expect(await writer.tick()).toBe('mirrored-elsewhere')
        expect(await writer.tick()).toBe('unchanged')
    })
})
