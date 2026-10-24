import type { Io } from '../lib/io.ts'
import { field, isSupervisor } from '../lib/agents.ts'
import { hasEscape } from '../lib/shell.ts'
import type { CommandClass } from '../lib/slot.ts'

// Workers must not run the whole test suite. Workers and their reviewers ran
// `go test ./...` (~900 tests, minutes) for every small task, two to four
// times per task, and that dominated the wall clock of every run; a written
// rule in the contract was ignored. A worker verifies the packages it
// changed; the full suite runs once, after integration.
//
// Deny only when ALL hold: a worker's Bash call (a subagent's, or the main
// thread of a session a wave hosts -- the instructor's own main thread is
// never denied); agent-exec classifies the line as a full run (`scope:
// full`, the one classifier: `agent-exec slot classify`); not
// orchestra-delegate (the supervising layer runs post-integration
// verification); no escape hatch.
//
// Escape hatches: [orchestra:allow-full-suite(: reason)] in the command or
// description (the post-integration run, or a task that really is
// repository-wide); ORCHESTRA_TEST_GUARD=off; fail-open on anything
// unexpected, agent-exec missing included (no classification, no deny).

const reason = (op: string): string =>
    `orchestra: a worker must not run the whole test suite ('${op}' covers the whole repository or workspace). Run the tests of the packages you changed instead (e.g. 'go test ./internal/<pkg>/...', 'pnpm --filter <pkg> test', 'pytest tests/test_<x>.py'), or 'agent-exec check --task <id>', which runs the checks scoped to your changed files. The full suite runs once, after integration. If this IS that post-integration run, include [orchestra:allow-full-suite: <reason>] in the command or its description.`

export const checkWorkerTests = async (
    io: Io,
    input: Readonly<Record<string, unknown>>,
    agentType: string,
    command: CommandClass | undefined,
): Promise<string | undefined> => {
    if (command?.scope !== 'full') {
        return undefined
    }
    if (hasEscape('full-suite', field(input, 'command'), field(input, 'description')) || isSupervisor(agentType)) {
        return undefined
    }
    if (await io.env('ORCHESTRA_TEST_GUARD') === 'off') {
        return undefined
    }

    return reason(command.op)
}
