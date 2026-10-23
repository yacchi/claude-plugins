import { field } from './agents.ts'
import { join, normalize } from './paths.ts'

// A worker must not edit installed dependencies. Editing node_modules is
// never a deliverable (the diff leaves it out), and with
// isolation.pnpm_global_virtual_store the package directories are links into
// the pnpm store's `links`, SHARED by every worktree: an edit there changes
// the dependency for every other worker and for the user. Always on, whatever
// that setting says.
//
// Deny when the call comes from a worker (the caller decides: a subagent, or
// the main thread of a session a wave hosts -- never the instructor's main
// thread) and an Edit/Write/MultiEdit/NotebookEdit target has a path segment
// that is exactly `node_modules` (lexically, relative paths against cwd; a
// `my_node_modules/` or `node_modules.d.ts` is not one).
//
// Bash is NOT covered, by design: a redirection, `cp`, `sed -i` or an
// interpreter writing under node_modules cannot be caught reliably. That is
// the residual risk, documented in references/isolation.md.

const STRUCTURED = new Set(['Edit', 'Write', 'MultiEdit', 'NotebookEdit'])

const reason = (target: string): string =>
    `orchestra: this edit targets an installed dependency ('${target}'). Dependency files are shared across worktrees (with pnpm's global virtual store they are links into one store every tree uses), and node_modules is never part of your diff, so the edit would change other workers' dependencies and never reach the result. Fix it in the project's own code instead, or, if a dependency truly needs a patch, do it through the package manager (pnpm patch, patch-package, packageExtensions) and say so in your verdict.`

export const checkDependencyEdit = (tool: string, input: Readonly<Record<string, unknown>>, cwd: string): string | undefined => {
    if (!STRUCTURED.has(tool)) {
        return undefined
    }
    for (const key of ['file_path', 'notebook_path']) {
        const path = field(input, key)
        if (!path) {
            continue
        }
        const target = normalize(join(cwd || '/', path))
        if (target.split('/').includes('node_modules')) {
            return reason(target)
        }
    }

    return undefined
}
