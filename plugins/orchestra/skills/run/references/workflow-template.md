# Workflow interface (orchestra `run`)

The cost-tiered pipeline is a plugin workflow: `plugins/orchestra/workflows/pipeline.js`, shipped with the plugin. Do not copy its script into a Workflow call. Invoke it by name, and pass only the run's `args`. The script owns every model and effort pin, the size gate, the probe author, the ownership pre-check, the review gates and the run-end cleanup (`SKILL.md` §5 and §7 to §12 say what each does).

Contents: §1 Invoking · §2 The args contract · §3 The return value · §4 The relay protocol.

## 1. Invoking

Primary, once the plugin is installed:

```
Workflow({ name: 'orchestra:pipeline', args: { runId, tasks } })
```

Fallback, when that name does not resolve (plugin not registered in this session): pass the script file by path. `<plugin root>` is the `plugins/orchestra/` directory, three levels above this `references/` directory.

```
Workflow({ scriptPath: '<plugin root>/workflows/pipeline.js', args: { runId, tasks } })
```

To change the protocol for one run, copy `<plugin root>/workflows/pipeline.js` to the session scratchpad, edit the copy, and run the copy with `scriptPath`. Do not edit the installed plugin file for a single run.

## 2. The args contract

`args` is an object with two required fields:

- `runId` (string, required): the workflow run id. The script passes it to the cleanup step, which reports the run's usage. Pass the same id to `agent-exec dispatch prepare --run-id` when you mint each token (§4 explains why it is not sent on the dispatch call).
- `tasks` (array, required): one task object per package. An empty array is accepted and returns at once with no dispatch.

Each task object:

| Field | Required | Meaning |
|---|---|---|
| `id` | yes | Task id. Also the `--task` id of its isolated tree. |
| `cls` | no | `'light'`, `'standard'` or `'deep'`. Initial class, default `'light'`; the script bumps it on escalation. |
| `dispatchToken` | yes | Token from `agent-exec dispatch prepare --isolate always --task <id> --run-id <runId>`, minted with the round-1 prompt file after `agent-exec isolate request start --into <runId>` (SKILL.md §5). |
| `workerPromptFile` | yes | Path to the literal spec, edge cases and verify command for the worker. |
| `workerPrompt` | yes | The same contract text, inlined. Used in the correction packet when no session can be resumed. |
| `verifierPrompt` | yes | What the reviewer must re-check and which implementation mistakes to probe for. |
| `files_owned` | yes | Non-empty array of repo-relative paths the worker may change, its test files included. A trailing `/` is a directory prefix. At most 3 entries. |
| `est_seconds` | yes | Integer estimate of worker time. At most 300. |
| `probeDir` | yes, unless `noProbes` | Scratch directory outside every worktree where the probe author writes throwaway tests. |
| `allowLarge` | no | Non-empty reason string. Waives the `est_seconds` and `files_owned` limits. Logged and echoed in the task result. |
| `noProbes` | no | Non-empty reason string. Skips the probe author. Logged. |
| `workdir` | no | Working directory for the dispatch, when it is a subdirectory of the repo. |
| `correctionPromptFile` | no | Path that does not exist yet. The script writes the correction packet there on a failed gate. |
| `correctionTokenFull` | no | Token minted with the worker prompt file, the correction prompt file, `--task <id>`, `--isolate always` and `--run-id <runId>` (the same tree as round 1). Used when round 1 reported no session, or after a class bump. |
| `correctionTokenDelta` | no | Token minted with the correction prompt file only, and `--run-id <runId>` (the same tree as round 1). Used when round 1 reported a session and the class did not change. |
| `preimplementedPath` | no | Tree that already holds this task's round-1 work (a lost dispatch result). Round 1 is skipped and the gate starts at review. |

Refusal and size-gate outcomes, both returned without any dispatch:

- `args` missing, `args.runId` missing, or `args.tasks` not an array: `{ error: '<which one>' }`.
- Any task fails the size gate (a required field missing, `est_seconds` over 300, or more than 3 `files_owned` entries without `allowLarge`): `{ sizeGate: 'failed', violations: [...] }`. Each violation names the task id, its problems and a hint to split the task by file ownership.

## 3. The return value

A run that starts returns `{ results, runEnd }`:

- `results`: one entry per task, in `tasks` order. Each carries `pass`, `rounds`, and `allowLarge` when it was set, plus the verdict fields the review returned.
- `runEnd`: the text of the cleanup step (the `agent-exec isolate integrate` of the passing tasks into `<runId>`, `agent-exec isolate sweep` and `agent-exec usage --run <runId>`), verbatim. Report cost from it. The passing tasks now sit in integration tree `<runId>`: gate it, then `agent-exec isolate merge --task <runId> -m "<subject>"`; land only on the user's request.

## 4. The relay protocol

The relay protocol: one hop, one command, one line back. Every relay call is a `relayHop()`: the relay runs exactly one Bash command, once, and prints only the last line of its stdout. It never loops, waits, retries, or summarizes; every loop is the script's own `while`. A dispatch is two hop kinds: one **start** hop, `agent-exec dispatch --token T --capture --detach` (timeout 30000), which returns `detached`/`running`/`done` at once; then **wait** hops, `agent-exec dispatch wait --token T --summary` (timeout 600000), until the status is final. `--summary` keeps the final result to one compact JSON line, with `answer` cut to 400 code points (`answer_truncated: true`) and the full text at `answer_path`.

`wait` is idempotent, so an unparsable reply is retried as another wait hop, never as a re-dispatch. The retry count is bounded by `MAX_GARBLED_HOPS`, and the whole wait by `MAX_WAIT_HOPS` (60). Relay failures throw `RelayFailureError` carrying `token` and `label`, so a caller can resume with `awaitDispatch(err.token, err.label)` instead of paying for the work twice (`references/programme.md` §3 does this). The `--run-id` flag belongs to `dispatch prepare` only: `agent-exec dispatch --token ...` rejects it with `--token conflicts with --run-id`, because the relay is given the token and nothing else.

Routing outcomes that reach the script's agent() calls:

- A CLI executor (e.g. `pi`) runs the work inside `agent-exec`, and its `answer` is the round's output.
- `dispatch: 'agent'` (a plugin subagent): the worker prompt gets the `--model` and `--effort` text appended, and the call carries `agentType`, `model` and `effort`.
- `dispatch: 'claude'` (the Claude winner, `orchestra:orchestra-light` or `orchestra:orchestra-deep`): the call carries `agentType`, `model` and `effort`, with no flag text.
