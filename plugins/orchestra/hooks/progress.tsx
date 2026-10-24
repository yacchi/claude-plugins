import type { EngineInterface, On } from 'claude-code'

// The wave progress views the HUD draws from `wave status --json`'s
// `progress` object: the status-line part, the `orchestra` detail pane and
// the band above the prompt. hud.ts tracks the waves and polls them; this
// file parses and draws, from what hud.ts hands it at each draw.

export type Running = {
    id: string,
    cls: string,
    model?: string,
    phase: string,
    elapsedS: number,
    estS?: number,
}

export type Need = { id?: string, kind: string, detail: string }

export type Progress = {
    runId?: string,
    total: number,
    integrated: number,
    running: Running[],
    needs: Need[],
    next: string[],
    recent: string[],
    elapsedS: number,
    etaS?: number,
    costUsd?: number,
}

// What a drawing reads: the agent part of the status line and the progress
// of every tracked wave that has answered a status read.
export type Snapshot = { agentPart?: string, waves: Progress[] }

const isRecord = (v: unknown): v is Record<string, unknown> =>
    typeof v === 'object' && v !== null && !Array.isArray(v)

const num = (v: unknown): number | undefined => (typeof v === 'number' && Number.isFinite(v) ? v : undefined)

const str = (v: unknown): string | undefined => (typeof v === 'string' && v !== '' ? v : undefined)

const records = (v: unknown): Record<string, unknown>[] => (Array.isArray(v) ? v.filter(isRecord) : [])

// The `progress` object of `wave status --json`; undefined when absent.
export const progressOf = (v: unknown): Progress | undefined => {
    if (!isRecord(v)) {
        return undefined
    }

    return {
        runId: str(v.run_id),
        total: num(v.total) ?? 0,
        integrated: num(v.integrated) ?? 0,
        running: records(v.running).map(r => ({
            id: str(r.id) ?? '?',
            cls: str(r.cls) ?? '?',
            model: str(r.model),
            phase: str(r.phase) ?? '',
            elapsedS: num(r.elapsed_s) ?? 0,
            estS: num(r.est_seconds),
        })),
        needs: records(v.needs).map(n => ({
            id: str(n.id),
            kind: str(n.kind) ?? 'need',
            detail: str(n.detail) ?? '',
        })),
        next: Array.isArray(v.next) ? v.next.filter(id => typeof id === 'string') : [],
        recent: records(v.recent).map(r => str(r.id)).filter(id => id !== undefined),
        elapsedS: num(v.elapsed_s) ?? 0,
        etaS: num(v.eta_s),
        costUsd: num(v.cost_usd),
    }
}

const PHASES: Readonly<Record<string, string>> = {
    implementing: '実装',
    verifying: '検証',
    correcting: '修正',
    integrating: '統合',
    waiting: '待機',
}

const phaseOf = (phase: string): string => PHASES[phase] ?? phase

// `45s`, `35m`, `2h10m`.
export const durationOf = (seconds: number): string => {
    const s = Math.max(0, Math.round(seconds))
    if (s < 60) {
        return `${s}s`
    }
    const m = Math.round(s / 60)

    return m < 60 ? `${m}m` : `${Math.floor(m / 60)}h${String(m % 60).padStart(2, '0')}m`
}

// The wave's part of the status line: `wave 12/40 · 5 running · 2 needs · ~35m`.
export const statusPart = (p: Progress): string => {
    const parts = [`wave ${p.integrated}/${p.total}`, `${p.running.length} running`]
    if (p.needs.length > 0) {
        parts.push(`${p.needs.length} needs`)
    }
    if (p.etaS !== undefined) {
        parts.push(`~${durationOf(p.etaS)}`)
    }

    return parts.join(' · ')
}

// Terminal cells a character takes: East Asian wide letters (the phase
// labels, 経過, 残り) and emoji take two.
const widthOf = (ch: string): number => {
    const cp = ch.codePointAt(0) ?? 0
    const isWide = (cp >= 0x1100 && cp <= 0x115f) || (cp >= 0x2e80 && cp <= 0xa4cf)
        || (cp >= 0xac00 && cp <= 0xd7a3) || (cp >= 0xf900 && cp <= 0xfaff)
        || (cp >= 0xfe30 && cp <= 0xfe4f) || (cp >= 0xff00 && cp <= 0xff60)
        || (cp >= 0xffe0 && cp <= 0xffe6) || (cp >= 0x1f300 && cp <= 0x1faff)

    return isWide ? 2 : 1
}

const cellsOf = (text: string): number => [...text].reduce((n, ch) => n + widthOf(ch), 0)

// The text cut to `columns` cells, an ellipsis ending what was cut.
const clip = (text: string, columns: number): string => {
    if (cellsOf(text) <= columns) {
        return text
    }
    let out = ''
    let used = 0
    for (const ch of text) {
        if (used + widthOf(ch) > columns - 1) {
            break
        }
        out += ch
        used += widthOf(ch)
    }

    return `${out}…`
}

// As many items as fit `columns` after `prefix`, ` · `-joined, `+k` naming
// those left out.
const fitJoin = (prefix: string, items: readonly string[], columns: number): string => {
    for (let k = items.length; k > 0; k--) {
        const rest = items.length - k
        const text = prefix + [...items.slice(0, k), ...(rest > 0 ? [`+${rest}`] : [])].join(' · ')
        if (cellsOf(text) <= columns) {
            return text
        }
    }

    return clip(`${prefix}+${items.length}`, columns)
}

const timeOf = (r: Running): string =>
    durationOf(r.elapsedS) + (r.estS !== undefined ? `/${durationOf(r.estS)}` : '')

// The detail pane's lines for every tracked wave, each cut to `columns`;
// the running rows of a wave give way to `… +k` past their share of `rows`.
export const paneLines = (waves: readonly Progress[], columns: number, rows: number): string[] => {
    const fixed = waves.reduce((n, p) => n + 3
        + (p.needs.length > 0 ? 1 + p.needs.length : 0)
        + (p.next.length > 0 ? 1 : 0)
        + (p.recent.length > 0 ? 1 : 0), -1)
    const busy = Math.max(1, waves.filter(p => p.running.length > 0).length)
    const share = Math.max(1, Math.floor((rows - fixed) / busy))
    const lines: string[] = []
    for (const p of waves) {
        if (lines.length > 0) {
            lines.push('')
        }
        const header = [p.runId ?? 'wave', `${p.integrated}/${p.total}`, `経過 ${durationOf(p.elapsedS)}`]
        if (p.etaS !== undefined) {
            header.push(`残り ~${durationOf(p.etaS)}`)
        }
        if (p.costUsd !== undefined) {
            header.push(`~$${p.costUsd.toFixed(2)}`)
        }
        lines.push(header.join('  '))
        lines.push(`▶ running (${p.running.length})`)
        const shown = p.running.length <= share ? p.running : p.running.slice(0, Math.max(1, share - 1))
        for (const r of shown) {
            lines.push(`  ${r.id}  ${r.model ? `${r.cls}→${r.model}` : r.cls}  ${phaseOf(r.phase)}  ${timeOf(r)}`)
        }
        if (shown.length < p.running.length) {
            lines.push(`  … +${p.running.length - shown.length}`)
        }
        if (p.needs.length > 0) {
            lines.push(`⚠ needs (${p.needs.length})`)
            for (const n of p.needs) {
                lines.push(`  ${n.id ?? 'wave'}  ${n.kind}  ${n.detail}`)
            }
        }
        if (p.next.length > 0) {
            lines.push(`⏭ next: ${p.next.join(', ')}`)
        }
        if (p.recent.length > 0) {
            lines.push(`✓ recent: ${p.recent.join(', ')}`)
        }
    }

    return lines.map(line => clip(line, columns))
}

// The band's lines, at most 4: the status line with run ids, the first
// running rows, the needs (only when any), what starts next.
export const bandLines = (snapshot: Snapshot, columns: number): string[] => {
    const waves = snapshot.waves.map(p => (p.runId ? `${p.runId} · ${statusPart(p)}` : statusPart(p)))
    const parts = [snapshot.agentPart, ...waves].filter(part => part !== undefined)
    const lines = [clip(`orch ▸ ${parts.join(' │ ')}`, columns)]
    const running = snapshot.waves.flatMap(p => p.running)
        .map(r => `${r.id} ${r.model ?? r.cls} ${phaseOf(r.phase)} ${durationOf(r.elapsedS)}`)
    lines.push(running.length > 0 ? fitJoin('', running, columns) : clip('▶ none running', columns))
    const needs = snapshot.waves.flatMap(p => p.needs).map(n => `${n.id ?? 'wave'} ${n.kind}`)
    if (needs.length > 0) {
        lines.push(fitJoin('⚠ ', needs, columns))
    }
    const next = snapshot.waves.flatMap(p => p.next)
    if (next.length > 0) {
        lines.push(clip(`⏭ next: ${next.join(', ')}`, columns))
    }

    return lines
}

const PANE = 'orchestra'
const TITLE = 'orchestra'

// Whether this module opened the pane on its own (it closes it again once
// no wave is tracked), and whether the person closed it or toggled it off
// (it is then not opened unasked again this session).
let openedUnasked = false
let dismissed = false

// Redraws the pane and the band after the tracked waves changed, and closes
// a pane opened unasked once nothing is tracked.
// (A function declaration: the engine follows `$` past an import only into one.)
export function redraw($: EngineInterface, snapshot: Snapshot): void {
    $.ui.invalidate('ui.render')
    if (openedUnasked && snapshot.waves.length === 0) {
        openedUnasked = false
        void $.ui.close({ id: PANE }).catch(() => undefined)
    }
}

const paneOf = async ($: EngineInterface) =>
    (await $.ui.panes().catch(() => [])).find(pane => pane.id === PANE)

export const registerProgress = (on: On, snapshot: () => Snapshot): void => {
    on('session.start', async ($, e, next) => {
        try {
            await $.command.register({ name: 'progress', description: 'Toggle the orchestra wave progress pane' })
        } catch {
            // the HUD must never break a session start
        }

        return next(e)
    }).catch(($, e, next) => next(e))

    // `/orchestra:progress` toggles the pane, wherever a surface places one.
    on('command.run', { command: 'progress' }, async $ => {
        openedUnasked = false
        if (await paneOf($)) {
            dismissed = true
            await $.ui.close({ id: PANE })

            return { text: 'orchestra: progress pane closed.' }
        }
        dismissed = false
        const opened = await $.ui.open({ id: PANE, title: TITLE })

        return { text: opened.isPlaced ? 'orchestra: progress pane opened.' : `orchestra: progress pane waits: ${opened.reason}` }
    }).catch(() => ({ text: 'orchestra: progress pane unavailable.' }))

    on('ui.close', ($, e, next) => {
        if (e.id === PANE) {
            openedUnasked = false
            dismissed = dismissed || e.origin.kind === 'person'
        }

        return next(e)
    }).catch(($, e, next) => next(e))

    on('ui.render', { component: 'Pane', requestId: PANE }, ($, e) => {
        const { Box, Text } = $.ui.resolve(e)
        const { waves } = snapshot()
        if (waves.length === 0) {
            return <Text dimColor>no orchestra wave tracked</Text>
        }

        return (
            <Box flexDirection="column">
                {paneLines(waves, e.props.bodyColumns, e.props.scroll.bodyRows)
                    .map(line => <Text dimColor={line.startsWith('✓')}>{line || ' '}</Text>)}
            </Box>
        )
    }).catch(($, e, next) => next(e))

    // The band shows while a wave is tracked and the pane is not up. Where
    // the surface docks panes beside the transcript (fullscreen) the pane
    // is opened unasked instead, a sidebar; elsewhere only when asked.
    on('ui.render', { component: 'AbovePrompt' }, async ($, e, next) => {
        const now = snapshot()
        if (e.props.hasSurvey || now.waves.length === 0) {
            return next(e)
        }
        const pane = await paneOf($)
        if (!pane && !dismissed && e.viewport?.isFullscreen === true) {
            openedUnasked = true
            void $.ui.open({ id: PANE, title: TITLE }).catch(() => undefined)

            return next(e)
        }
        if (pane?.isPlaced && pane.isShown) {
            return next(e)
        }
        const { Box, Text } = $.ui.resolve(e)

        return (
            <Box flexDirection="column">
                {bandLines(now, e.props.bodyColumns).map(line => <Text>{line}</Text>)}
            </Box>
        )
    }).catch(($, e, next) => next(e))
}
