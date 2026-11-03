import { expect, test } from 'claude-code/testing'
import { checkWorkerNode } from './worker-node.ts'

// Pins the worker Bash guard against NODE_OPTIONS replacement and test runners
// started from node -e (two fork bombs). Covered: the forms each rule reads,
// the operands it must not judge (quoted data, heredoc bodies, $NODE_OPTIONS
// appends), the escape hatches, the order (replacement before runner), the
// 80-character cut, and the exact reason texts.

const deny1Word = (command: string): string | undefined =>
    checkWorkerNode(command, '')?.match(/^orchestra: this command replaces NODE_OPTIONS \((.*)\), which drops/)?.[1]

const deny2Runner = (command: string): string | undefined =>
    checkWorkerNode(command, '')?.match(/^orchestra: do not start a test runner from node -e \/ node -p \('(.*)'\)/)?.[1]

test('denies NODE_OPTIONS replacement and removal, quoting the offending word', () => {
    const cases: [string, string][] = [
        ['NODE_OPTIONS= pnpm test', 'NODE_OPTIONS='],
        ['NODE_OPTIONS=\'--x\' node a.js', 'NODE_OPTIONS=\'--x\''],
        ['cd pkg && NODE_OPTIONS="--max-old-space-size=4096" pnpm build', 'NODE_OPTIONS="--max-old-space-size=4096"'],
        ['export NODE_OPTIONS=--x', 'NODE_OPTIONS=--x'],
        ['export A=1 NODE_OPTIONS=--x', 'NODE_OPTIONS=--x'],
        ['declare -x NODE_OPTIONS=--x', 'NODE_OPTIONS=--x'],
        ['typeset -gx NODE_OPTIONS=--x', 'NODE_OPTIONS=--x'],
        ['env NODE_OPTIONS=--x node a.js', 'NODE_OPTIONS=--x'],
        ['env -i NODE_OPTIONS=--x node a.js', 'NODE_OPTIONS=--x'],
        ['/usr/bin/env FOO=1 NODE_OPTIONS=--x pnpm test', 'NODE_OPTIONS=--x'],
        ['unset NODE_OPTIONS', 'unset NODE_OPTIONS'],
        ['unset FOO NODE_OPTIONS BAR', 'unset NODE_OPTIONS'],
        ['env -u NODE_OPTIONS node a.js', '-u NODE_OPTIONS'],
        ['env --unset=NODE_OPTIONS node a.js', '--unset=NODE_OPTIONS'],
    ]
    for (const [command, word] of cases) {
        expect(deny1Word(command)).toBe(word)
    }
})

test('denies a test runner started from a node eval or print script', () => {
    const cases: [string, string][] = [
        ['node -e "process.chdir(\'/x\'); require(\'/x/node_modules/jest/bin/jest\')"', 'jest'],
        ['nodejs --eval="require(\'vitest/node\')"', 'vitest'],
        ['/usr/local/bin/node -p "require(\'mocha\')"', 'mocha'],
        ['env FOO=1 time node -e \'require("playwright")\'', 'playwright'],
        ['node -r ./setup.js -e "require(\'jest-cli\').run()"', 'jest'],
    ]
    for (const [command, runner] of cases) {
        expect(deny2Runner(command)).toBe(runner)
    }
})

test('allows NODE_OPTIONS appends, reads and quoted mentions, and non-runner eval scripts', () => {
    const allowed = [
        'NODE_OPTIONS="$NODE_OPTIONS --max-old-space-size=4096" pnpm build',
        'NODE_OPTIONS="${NODE_OPTIONS:-} --x" node a.js',
        'export NODE_OPTIONS="$NODE_OPTIONS --x"',
        'NODE_OPTIONS+=" --x" node a.js',
        'echo $NODE_OPTIONS',
        'echo "NODE_OPTIONS=--x"',
        'grep -r \'unset NODE_OPTIONS\' docs',
        'export NODE_OPTIONS',
        'node -e "console.log(process.versions)"',
        'node scripts/build.js',
        'pnpm exec jest src/a.test.ts',
        'node -p "require(\'./package.json\').version"',
        'node -e "console.log(1)" && npx jest a.test.ts',
        'echo jest | node -e "process.stdin.pipe(process.stdout)"',
        'echo "NODE_OPTIONS=--x; unset NODE_OPTIONS"',
    ]
    for (const command of allowed) {
        expect(checkWorkerNode(command, '')).toBeUndefined()
    }
})

test('ignores heredoc bodies', () => {
    expect(checkWorkerNode('cat > task.txt <<EOF\nNODE_OPTIONS=--x node -e "require(\'jest\')"\nEOF', '')).toBeUndefined()
})

test('honours the allow-node escape in the command or the description', () => {
    expect(checkWorkerNode('NODE_OPTIONS=--x node a.js [orchestra:allow-node]', '')).toBeUndefined()
    expect(checkWorkerNode('unset NODE_OPTIONS', 'debug [orchestra:allow-node: need a clean env]')).toBeUndefined()
    expect(checkWorkerNode('node -e "require(\'jest\')"', '[orchestra:allow-node]')).toBeUndefined()
})

test('reads a backslash-newline as whitespace, not part of the word', () => {
    expect(deny2Runner('node -e \\\n "require(\'jest\')"')).toBe('jest')
})

test('checks replacement before the test runner', () => {
    const command = 'NODE_OPTIONS=--no-experimental-strip-types node -e "require(\'jest\')"'
    expect(deny1Word(command)).toBe('NODE_OPTIONS=--no-experimental-strip-types')
    expect(deny2Runner(command)).toBeUndefined()
})

test('cuts the offending word to 80 characters', () => {
    const long = `NODE_OPTIONS=--${'a'.repeat(200)}`
    expect(deny1Word(`${long} node a.js`)).toBe(long.slice(0, 80))
})

test('states the exact reason texts', () => {
    expect(checkWorkerNode('NODE_OPTIONS=--x node a.js', '')).toBe(
        `orchestra: this command replaces NODE_OPTIONS (NODE_OPTIONS=--x), which drops orchestra's preload from it (the Node CPU cap and the stop for forked eval children). Keep what is there and add yours: NODE_OPTIONS="$NODE_OPTIONS --no-experimental-strip-types" <command>. Never unset NODE_OPTIONS. If it really must be replaced, include [orchestra:allow-node: <reason>] in the command or its description.`,
    )
    expect(checkWorkerNode('node -e "require(\'jest\')"', '')).toBe(
        `orchestra: do not start a test runner from node -e / node -p ('jest'): jest's workers re-run the eval script and multiply until the machine dies. Run the package's test script instead (e.g. 'pnpm --filter <pkg> test -- <path>', or 'pnpm exec jest <path>' in the package directory), or 'agent-exec check --task <id>', which runs the checks scoped to your changed files. If it really must run from an eval script, include [orchestra:allow-node: <reason>] in the command or its description.`,
    )
})
