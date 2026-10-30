import type { EngineInterface, On, Timer } from 'claude-code'
import { agentExec } from '../lib/config.ts'
import type { Io } from '../lib/io.ts'
import { verdictOf } from '../router/verdict.ts'
import { type Identity, identityOf, pageOf, publishedUrlOf, publishPrompt } from './parts.ts'
import { createWriter, docIdOf, type Writer } from './writer.ts'

// The account's board page: `/orchestra:board` names it, the page's publish
// stores it, and a 15 s loop mirrors status.json into it while this session
// may write. Every hook fails open: the board never breaks a session.

const STORE_KEY = 'board'
const TICK_MS = 15_000

let timer: Timer | undefined
let writer: Writer | undefined
let decided = false
let blocked: string | undefined
let identity: Identity | undefined
let toasted = false

// The host as the writer and the verdict see it, built per call from `$`.
function hostIo($: EngineInterface): Io {
    return {
        run: (argv, init) => $.process.run(argv, init),
        pluginRoot: () => {
            try {
                return $.plugin.root
            } catch {
                return ''
            }
        },
        root: () => $.session.root(),
        sessionCwd: () => $.session.cwd(),
        sessionId: () => $.session.id(),
        model: () => $.session.model(),
        exists: path => $.fs.exists(path),
        read: path => $.fs.read(path),
        stat: (path, resolve) => $.fs.stat(path, { resolve }),
    } as Pick<Io, 'run' | 'pluginRoot' | 'root' | 'sessionCwd' | 'sessionId' | 'model' | 'exists' | 'read' | 'stat'> as Io
}

// Cached on success only: a failed whoami is asked again on the next check.
async function identityFor($: EngineInterface): Promise<Identity> {
    if (identity !== undefined) {
        return identity
    }
    const r = await agentExec(hostIo($), ['status', 'whoami'], 5000)
    const parsed = r.kind === 'ok' ? identityOf(r.stdout) : undefined
    if (parsed === undefined) {
        return { account: null, host: '' }
    }
    identity = parsed

    return parsed
}

// Decides once whether this session writes the page; `blocked` says why not.
async function ensure($: EngineInterface): Promise<void> {
    if (timer !== undefined || decided) {
        return
    }
    decided = true
    blocked = undefined
    try {
        const page = pageOf(await $.store.get(STORE_KEY))
        if (page === undefined) {
            return
        }
        if ((await $.session.surfaces()).length === 0) {
            blocked = 'this session draws nowhere (a -p run)'

            return
        }
        const io = hostIo($)
        if (await verdictOf(io, await io.sessionId().catch(() => '')) === 'worker') {
            blocked = 'a hosted worker session never writes the board'

            return
        }
        const me = await identityFor($)
        if (page.account !== null && me.account !== null && page.account !== me.account) {
            blocked = `the stored page belongs to ${page.account}, and this session is signed in to ${me.account}: /orchestra:board publish makes this account's page`

            return
        }
        if (!(await $.tool.list()).some(t => t.name === 'ArtifactData')) {
            blocked = 'the ArtifactData tool is not available here (it needs a claude.ai sign-in)'

            return
        }
        // A write the permission check would ask about would open a dialog on every tick.
        const check = await $.tool.check({
            tool: 'ArtifactData',
            input: { action: 'set', url: page.url, collection: 'status', doc_id: docIdOf(me.host), data: {} },
        })
        if (check.decision !== 'allow') {
            const why = [check.reason, check.rule && `rule ${check.rule}`, check.setting && `setting ${check.setting}`]
                .filter(Boolean).join('; ')
            blocked = `the permission check answers ${check.decision} for ArtifactData writes${why === '' ? '' : ` (${why})`}: `
                + 'add "ArtifactData" to permissions.allow in this profile\'s settings.json, then run /orchestra:board'

            return
        }
        writer = createWriter(
            { url: page.url, host: me.host, account: me.account, home: (await $.env.get('HOME')) ?? '' },
            {
                nowMs: () => $.clock.now(),
                stat: async p => ({ mtimeMs: (await $.fs.stat(p)).mtimeMs }),
                read: p => $.fs.read(p),
                write: (p, t) => $.fs.write(p, t),
                artifactData: input => $.tool.call({ ...input, tool: 'ArtifactData' } as never),
            },
        )
        timer = $.clock.every(TICK_MS, () => {
            void runTick($).catch(() => undefined)
        })
        void runTick($).catch(() => undefined)
    } catch {
        decided = false
    }
}

async function runTick($: EngineInterface): Promise<void> {
    const current = writer
    if (current === undefined) {
        return
    }
    if (await current.tick() !== 'refused') {
        return
    }
    stopWriter()
    blocked = 'claude.ai refused the write: the page is in another organization than this session\'s; /orchestra:board publish makes this account\'s page'
    if (!toasted) {
        toasted = true
        $.ui.toast('orchestra: the board page refused this account — /orchestra:board publish')
    }
}

function stopWriter(): void {
    timer?.cancel()
    timer = undefined
    writer = undefined
}

// Recovers the loop after the engine replaced the module's copy of this state.
export function dashboardTurnStart($: EngineInterface): void {
    void ensure($)
}

async function boardCommand($: EngineInterface, args: string): Promise<{ text: string }> {
    const arg = args.trim()
    if (arg === 'forget') {
        stopWriter()
        decided = true
        blocked = undefined
        await $.store.delete(STORE_KEY)

        return { text: 'orchestra: forgot this account\'s board page; the page stays on claude.ai. /orchestra:board publishes a new one.' }
    }
    if (arg !== '' && arg !== 'publish') {
        return { text: 'usage: /orchestra:board [publish|forget]' }
    }
    const page = pageOf(await $.store.get(STORE_KEY))
    if (arg === 'publish' || page === undefined) {
        const text = publishPrompt($.plugin.root, page?.url)
        $.clock.after(1, () => {
            void $.prompt.submit({ text, asUser: true }).catch(() => undefined)
        })

        return {
            text: page !== undefined
                ? `orchestra: asking Claude to update the board page at ${page.url}.`
                : 'orchestra: asking Claude to publish this account\'s board page and pin it to the claude.ai sidebar.',
        }
    }
    stopWriter()
    decided = false
    await ensure($)
    const text = `orchestra board: ${page.url}`

    return { text: timer === undefined && blocked !== undefined ? `${text}\nnot writing: ${blocked}` : text }
}

export function registerDashboard(on: On): void {
    on('session.start', { isInteractive: true }, async ($, e, next) => {
        try {
            await $.command.register({
                name: 'board',
                description: 'Show, publish or forget this account\'s orchestra board page on claude.ai',
                argumentHint: '[publish|forget]',
            })
        } catch {
            // the board must never break a session start
        }
        void ensure($)

        return next(e)
    }).catch(($, e, next) => next(e))

    on('command.run', { command: 'board' }, ($, e) => boardCommand($, e.args))
        .catch(() => ({ text: 'orchestra: the board is unavailable in this session.' }))

    // The page's own publish is the only way its URL is learned.
    on('tool.call', { tool: 'Artifact' }, async ($, e, next) => {
        const answer = await next(e)
        try {
            const url = publishedUrlOf(e as unknown as Readonly<Record<string, unknown>>, answer)
            if (url !== undefined) {
                await $.store.set(STORE_KEY, { url, account: (await identityFor($)).account })
                stopWriter()
                decided = false
                void ensure($)
            }
        } catch {
            // the capture must never break a publish
        }

        return answer
    }).catch(($, e, next) => next(e))
}
