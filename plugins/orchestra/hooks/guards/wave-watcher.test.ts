import { describe, expect, test } from 'claude-code/testing'
import { checkWaveWatcher, waveWatcherContext } from './wave-watcher.ts'

// The watcher's wait hops are held to 150 s and its two commands, and each
// deny or context names the command to run next. Pinned: the allowed shapes,
// the shell characters and flags that must not pass as a wait, the background
// deny, the seq that the next command carries, per-agent tracking and results
// that must give no context. The tracking map is shared, so every test uses
// its own agent id.

const W = 'agent-exec wave wait --state /st/w.json --max-wait 150'
const RUN = { command: 'agent-exec wave run --plan p --detach' }

const record = (agentId: string, command: string, stdout: string): string | undefined =>
    waveWatcherContext(agentId, { command }, { result: { stdout } })

const otherText = (next: string): string =>
    `orchestra: the wave watcher runs only \`agent-exec wave run … --detach\` and \`agent-exec wave wait --state <state> --max-wait 150 [--since <seq>]\`; it never reads files, sleeps or polls another way. Next: ${next}.`

describe('checkWaveWatcher', () => {
    test('allows the detached run and the bounded waits', () => {
        for (const command of [
            'agent-exec wave run --plan p.json --detach',
            '  /opt/bin/agent-exec wave run --plan p --detach  ',
            W,
            `${W} --since 7`,
            'agent-exec wave wait --since=7 --max-wait=170 --state=/st/w.json',
            'agent-exec wave wait --state /st/w.json --max-wait 1',
        ]) {
            expect(checkWaveWatcher('allow', { command })).toBeUndefined()
        }
    })

    test('denies every other command with the other-command text', () => {
        for (const command of [
            'agent-exec wave run --plan p',
            'agent-exec wave run --detach --plan p',
            'agent-exec wave run --detach',
            'agent-exec wave status --state s',
            'cat /tmp/out',
            'sleep 60',
            `${W}; cat out`,
            `${W} | tail -1`,
            `${W} > out`,
            `${W} 2>&1`,
            `${W} && echo`,
            `${W} --since \`cat s\``,
            `${W} --since $(cat s)`,
            `${W}\ncat out`,
            `${W} --json`,
            `${W} --state /b`,
            `${W} --since 1 --since=2`,
            'agent-exec wave wait --max-wait 150',
            'agent-exec wave wait --state /st/w.json',
            'agent-exec wave wait --state /st/w.json --max-wait 0',
            'agent-exec wave wait --state /st/w.json --max-wait 171',
            'agent-exec wave wait --state /st/w.json --max-wait 560',
            'agent-exec wave wait --state /st/w.json --max-wait 1.5',
            'agent-exec wave wait --state /st/w.json --max-wait abc',
            'agent-exec wave wait --state /st/w.json --max-wait -5',
            'agent-exec wave wait --state /st/w.json --max-wait=',
            'agent-exec wave wait --state --max-wait 150',
            'wave-exec wave wait --state /st/w.json --max-wait 150',
        ]) {
            expect(checkWaveWatcher('deny', { command })).toBe(otherText('the wave run command you were given, with --detach appended'))
        }
    })

    test('denies a background call, even for a wait or the detached run', () => {
        record('bg', RUN.command, '{"status":"detached","state":"/st/w.json"}\n')
        const next = 'agent-exec wave wait --state /st/w.json --max-wait 150'
        expect(checkWaveWatcher('bg', { command: W, run_in_background: true }))
            .toBe(`orchestra: the wave watcher runs every command in the foreground (timeout 600000), never with run_in_background. Next: \`${next}\`.`)
        expect(checkWaveWatcher('bg', { command: RUN.command, run_in_background: true }))
            .toBe(`orchestra: the wave watcher runs every command in the foreground (timeout 600000), never with run_in_background. Next: \`${next}\`.`)
    })

    test('names the state and the last seq seen in the deny', () => {
        record('deny-seq', RUN.command, '{"status":"detached","state":"/st/w.json"}\n')
        record('deny-seq', W, '{"status":"running","seq":7,"line":"x"}\n')
        expect(checkWaveWatcher('deny-seq', { command: 'cat /tmp/out' }))
            .toBe(otherText('`agent-exec wave wait --state /st/w.json --max-wait 150 --since 7`'))
    })
})

describe('waveWatcherContext', () => {
    test('a running answer names the seq of that answer', () => {
        record('run-seq', RUN.command, '{"status":"detached","state":"/st/w.json"}\n')
        expect(record('run-seq', W, 'hop 1\n{"status":"running","seq":7,"line":"x"}\n\n'))
            .toBe('orchestra: the wave is still running (status running): that is not a final answer. Run `agent-exec wave wait --state /st/w.json --max-wait 150 --since 7` now, and return only on needs, held, done, stopped or crashed.')
    })

    test('a call moved to the background names the last seq seen before it', () => {
        record('bg-seq', RUN.command, '{"status":"detached","state":"/st/w.json"}\n')
        record('bg-seq', W, '{"status":"running","seq":7,"line":"x"}\n')
        expect(waveWatcherContext('bg-seq', { command: `${W} --since 7` }, { result: { stdout: '', backgroundTaskId: 'b1' } }))
            .toBe('orchestra: this call was moved to the background, so its answer will not reach you. Do not read its output file. Run `agent-exec wave wait --state /st/w.json --max-wait 150 --since 7` in the foreground now.')
    })

    test('before any seq the next wait has no --since', () => {
        record('no-seq', RUN.command, '{"status":"detached","state":"/st/w.json"}\n')
        expect(waveWatcherContext('no-seq', { command: W }, { result: { stdout: '', backgroundTaskId: 'b2' } }))
            .toBe('orchestra: this call was moved to the background, so its answer will not reach you. Do not read its output file. Run `agent-exec wave wait --state /st/w.json --max-wait 150` in the foreground now.')
    })

    test('a final answer gives no context but records its seq', () => {
        record('final', RUN.command, '{"status":"detached","state":"/st/w.json"}\n')
        expect(record('final', W, '{"status":"done","seq":9}')).toBeUndefined()
        expect(record('final', W, '{"status":"needs"}')).toBeUndefined()
        expect(checkWaveWatcher('final', { command: 'cat x' }))
            .toBe(otherText('`agent-exec wave wait --state /st/w.json --max-wait 150 --since 9`'))
    })

    test('tracking is per agent', () => {
        record('pa-1', RUN.command, '{"status":"detached","state":"/st/a.json"}\n')
        record('pa-1', W.replace('/st/w.json', '/st/a.json'), '{"status":"running","seq":3}\n')
        record('pa-2', 'agent-exec wave run --plan q --detach', '{"status":"detached","state":"/st/b.json"}\n')
        expect(checkWaveWatcher('pa-2', { command: 'cat x' }))
            .toBe(otherText('`agent-exec wave wait --state /st/b.json --max-wait 150`'))
        expect(checkWaveWatcher('pa-1', { command: 'cat x' }))
            .toBe(otherText('`agent-exec wave wait --state /st/a.json --max-wait 150 --since 3`'))
    })

    test('a new wave run resets the entry, and a wait on another state drops the old seq', () => {
        record('reset', RUN.command, '{"status":"detached","state":"/st/old.json"}\n')
        record('reset', 'agent-exec wave wait --state /st/old.json --max-wait 150', '{"status":"running","seq":4}\n')
        record('reset', 'agent-exec wave wait --state /st/new.json --max-wait 150', '{"status":"running"}\n')
        expect(checkWaveWatcher('reset', { command: 'cat x' }))
            .toBe(otherText('`agent-exec wave wait --state /st/new.json --max-wait 150`'))
        record('reset', RUN.command, '{"status":"detached","state":"/st/fresh.json"}\n')
        expect(checkWaveWatcher('reset', { command: 'cat x' }))
            .toBe(otherText('`agent-exec wave wait --state /st/fresh.json --max-wait 150`'))
    })

    test('an agent with nothing recorded gets the run command in its next text', () => {
        expect(checkWaveWatcher('fresh', { command: 'cat x' }))
            .toBe(otherText('the wave run command you were given, with --detach appended'))
    })

    test('gives no context and never throws for unusable input or results', () => {
        expect(record('quiet', W, 'not json')).toBeUndefined()
        expect(record('quiet', W, '[1,2]')).toBeUndefined()
        expect(record('quiet', W, 'null')).toBeUndefined()
        expect(record('quiet', W, '{"status":"running"')).toBeUndefined()
        expect(waveWatcherContext('quiet', { command: W }, undefined)).toBeUndefined()
        expect(waveWatcherContext('quiet', { command: W }, null)).toBeUndefined()
        expect(waveWatcherContext('quiet', { command: W }, 'x')).toBeUndefined()
        expect(waveWatcherContext('quiet', { command: W }, {})).toBeUndefined()
        expect(waveWatcherContext('quiet', { command: W }, { result: 'x' })).toBeUndefined()
        expect(waveWatcherContext('quiet', {}, { result: { stdout: '{"status":"running","seq":1}' } })).toBeUndefined()
        expect(waveWatcherContext('quiet', { command: 5 }, { result: { stdout: '{"status":"running","seq":1}' } })).toBeUndefined()
        expect(waveWatcherContext('quiet', { command: 'cat x' }, { result: { stdout: '{"status":"running"}' } })).toBeUndefined()
    })
})
