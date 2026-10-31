import type { ToolCallResult } from 'claude-code'

// Mirrors `~/.claude/orchestra/status.json` into one ArtifactData document of
// the account's board page. The hooks module writes it, never a model. Every
// session on this machine runs a writer; a claim file per page makes one of
// them write each change.

export type WriterHost = {
    nowMs: () => Promise<number>,
    stat: (path: string) => Promise<{ mtimeMs: number }>,
    read: (path: string) => Promise<string>,
    write: (path: string, text: string) => Promise<void>,
    artifactData: (input: Readonly<Record<string, unknown>>) => Promise<ToolCallResult>,
}
export type BoardTarget = { url: string, host: string, account: string | null, home: string }
export type TickOutcome = 'unchanged' | 'mirrored-elsewhere' | 'not-due' | 'written' | 'failed' | 'backoff' | 'refused' | 'denied'
export type Writer = { tick: () => Promise<TickOutcome> }

type Obj = Record<string, unknown>
type Claim = { mtime_ms: number, written_ms: number, line: string, version: number | null }
type Answer = {
    deny?: string,
    text?: string,
    isError?: boolean,
    result?: {
        db_read?: { docs?: { version?: unknown }[] },
        db_write?: { committed?: unknown, version?: unknown },
    },
}

const isObj = (v: unknown): v is Obj => typeof v === 'object' && v !== null && !Array.isArray(v)

const summaryOf = (status: unknown): Obj | undefined => {
    if (!isObj(status) || !isObj(status.summary)) {
        return undefined
    }

    return status.summary
}

const build = (status: Obj, host: string, account: string | null, atS: number): Record<string, unknown> => {
    const runs = Array.isArray(status.runs)
        ? status.runs.filter(isObj).map(({ pid: _pid, state: _state, ...run }) => run)
        : []

    return { schema: 1, host, status: { ...status, runs }, writer: { account, at: atS } }
}

export const docIdOf = (host: string): string => host === ''
    ? 'local'
    : host.replace(/[^A-Za-z0-9_-]/g, '_')

export const pageKeyOf = (url: string): string => {
    const last = url.split(/[?#]/)[0].split('/').filter(s => s !== '').pop() ?? ''

    return last === '' ? 'page' : last.replace(/[^A-Za-z0-9_-]/g, '_')
}

export const mirrorOf = (
    status: unknown,
    host: string,
    account: string | null,
    atS: number,
): Record<string, unknown> | undefined => {
    if (!isObj(status) || summaryOf(status) === undefined) {
        return undefined
    }

    return build(status, host, account, atS)
}

// A get or set answer that the engine refused, or that lacks the shape we read.
const answerFailed = (answer: Answer, shape: 'db_read' | 'db_write'): boolean => {
    if (answer.deny !== undefined || answer.isError) {
        return true
    }
    if (shape === 'db_read') {
        return !Array.isArray(answer.result?.db_read?.docs)
    }

    return answer.result?.db_write?.committed !== true
}

export const createWriter = (target: BoardTarget, host: WriterHost): Writer => {
    const statusPath = `${target.home}/.claude/orchestra/status.json`
    const claimPath = `${target.home}/.claude/orchestra/dashboard/${pageKeyOf(target.url)}.json`
    const docId = docIdOf(target.host)

    let lastMtime: number | undefined
    let cachedVersion: number | undefined
    let needGet = false
    let failures = 0
    let skip = 0

    const readClaim = async (): Promise<Claim | undefined> => {
        try {
            const claim: unknown = JSON.parse(await host.read(claimPath))
            if (!isObj(claim) || typeof claim.mtime_ms !== 'number' || typeof claim.written_ms !== 'number' || typeof claim.line !== 'string') {
                return undefined
            }

            return {
                mtime_ms: claim.mtime_ms,
                written_ms: claim.written_ms,
                line: claim.line,
                version: typeof claim.version === 'number' ? claim.version : null,
            }
        } catch {
            return undefined
        }
    }

    const failed = async (mtimeMs: number, answer?: Answer): Promise<TickOutcome> => {
        const text = [answer?.deny, answer?.text, JSON.stringify(answer)]
            .filter((s): s is string => typeof s === 'string')
            .join(' ')
        if (/another of the user's organizations/i.test(text)) {
            return 'refused'
        }
        // The engine refused the call before it ran: the permission check
        // (a declined dialog, the classifier) or a hook.
        if (answer?.deny !== undefined) {
            return 'denied'
        }

        // Another session wrote this change while our set was in flight.
        const claim = await readClaim()
        if (claim && claim.mtime_ms >= mtimeMs) {
            cachedVersion = claim.version ?? undefined
            lastMtime = mtimeMs

            return 'mirrored-elsewhere'
        }

        needGet = true
        cachedVersion = undefined
        failures++
        skip = Math.min(2 ** failures, 40)

        return 'failed'
    }

    const tick = async (): Promise<TickOutcome> => {
        if (skip > 0) {
            skip--

            return 'backoff'
        }

        let mtimeMs = 0
        try {
            const stat = await host.stat(statusPath).catch(() => undefined)
            if (stat === undefined || stat.mtimeMs === lastMtime) {
                return 'unchanged'
            }
            mtimeMs = stat.mtimeMs

            // A torn or unreadable read retries on the next tick, same mtime.
            let text: string
            try {
                text = await host.read(statusPath)
            } catch {
                return 'unchanged'
            }

            let parsed: unknown
            try {
                parsed = JSON.parse(text)
            } catch {
                parsed = undefined
            }
            const summary = summaryOf(parsed)
            if (!isObj(parsed) || summary === undefined) {
                lastMtime = mtimeMs

                return 'unchanged'
            }

            const claim = await readClaim()
            if (claim && claim.mtime_ms >= mtimeMs) {
                cachedVersion = claim.version ?? cachedVersion
                lastMtime = mtimeMs

                return 'mirrored-elsewhere'
            }

            const now = await host.nowMs()
            const line = typeof summary.line === 'string' ? summary.line : ''
            const heartbeatMs = (typeof parsed.heartbeat_s === 'number' && parsed.heartbeat_s > 0 ? parsed.heartbeat_s : 60) * 1000
            if (claim && claim.line === line && now - claim.written_ms < heartbeatMs) {
                return 'not-due'
            }

            let version = needGet ? undefined : (claim?.version ?? cachedVersion)
            if (needGet || version == null) {
                const got = await host.artifactData({ action: 'get', url: target.url, collection: 'status', doc_id: docId }) as Answer
                if (answerFailed(got, 'db_read')) {
                    return failed(mtimeMs, got)
                }
                const doc = got.result?.db_read?.docs?.[0]
                version = typeof doc?.version === 'number' ? doc.version : undefined
            }

            const answer = await host.artifactData({
                action: 'set',
                url: target.url,
                collection: 'status',
                doc_id: docId,
                data: build(parsed, target.host, target.account, Math.floor(now / 1000)),
                ...(version !== undefined ? { if_version: version } : {}),
            }) as Answer
            if (answerFailed(answer, 'db_write')) {
                return failed(mtimeMs, answer)
            }

            const written = answer.result?.db_write
            cachedVersion = typeof written?.version === 'number' ? written.version : undefined
            needGet = false
            failures = 0
            lastMtime = mtimeMs
            try {
                await host.write(claimPath, JSON.stringify({ mtime_ms: mtimeMs, written_ms: now, line, version: cachedVersion ?? null }))
            } catch {
                // A missed claim only costs another session a duplicate write.
            }

            return 'written'
        } catch {
            return failed(mtimeMs)
        }
    }

    return { tick }
}
