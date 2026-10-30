# Status file: `~/.claude/orchestra/status.json`

New in v0.73.0. This is the contract for a viewer that reads orchestra's status. orchestra writes the file. The orchestra hooks module mirrors it to each account's claude.ai board page (`/orchestra:board`), from the user's interactive sessions; see the README's Board section.

## Location and writers

- Path: `~/.claude/orchestra/status.json`. It is based on `$HOME`, so every ccprofile profile writes the same file, whatever its `CLAUDE_CONFIG_DIR`.
- Only `agent-exec` writes it. No model or claude.ai sign-in is involved.
- The hooks module reads it, never writes it, to mirror it to the board page (see Board mirror).
- orchestra never reads anything back from it. Approve and deny happen in the instructor's Claude Code session, not in a viewer.
- Each `agent-exec wave run` writes its own entry, `~/.claude/orchestra/status/runs/<key>.json`. `<key>` is the first 16 hex characters of the SHA-1 of the real path of the run's state file. A run writes:
  - at start;
  - within 5 s of any change to its state, whether the change is its own or another process's (e.g. `wave mark`);
  - at least every 60 s while it runs (`heartbeat_s`);
  - once at its end.
- The skeleton planner (`orchestra-planner`) writes `~/.claude/orchestra/status/plans/<plan id>.json` with `agent-exec status plan` at phase boundaries. `agent-exec plan review` and `agent-exec plan next` write the `review`, `specs` and `done` phases and the stuck `needs-user` status themselves, from the plan id the instructor passed to `plan next --id`.
- After every write, the writer rebuilds `status.json` under the lock `~/.claude/orchestra/status/.lock`.

## Reading

- Every file is replaced atomically (written to a temporary file, then renamed). Read `status.json` whole, without taking any lock.
- Never read `.lock` or any dotfile.

## Freshness

- The file is rebuilt only when something writes.
- If `summary.busy` is `true` but `generated_at` is older than `stale_after_s` (180 s), every writer has stopped. Show "unknown / probably crashed" until the next rebuild.
- `agent-exec status show` rebuilds the file on demand and prints it.

## Retention

- A run that no live runner keeps fresh (ended, crashed or stale), and any plan, leave `status.json` 6 hours after their `updated_at`.
- Their files are deleted after 7 days.

## Board mirror

The hooks module mirrors `status.json` into the account's board page, from the user's interactive sessions only. `/orchestra:board` publishes the page once; after that the module writes it with `ArtifactData` under the session's own sign-in. No model writes it.

- Collection `status`, one document per machine. The doc id is the host name, and the data is `{schema: 1, host, status, writer: {account, at}}`.
- `status` is `status.json` without `runs[].pid` and `runs[].state`. Both are left out of the mirror.
- The mirror is written at most once per `heartbeat_s` (60) unless `summary.line` changed. One writer per change: the sessions on one machine take turns through a claim file under `~/.claude/orchestra/dashboard/`.
- Only where `ArtifactData` is allowed without asking. A refusal stops the loop.

## Privacy

- `summary` names no repository, path, run id or package id. It can be published alone, for example to a phone as "is anything working?".
- `runs[]` and `plans[]` carry project names, titles, package ids, need texts and the local state path. Treat them as private.
- The board page holds `runs[]` and `plans[]`, so it is private to the account unless its owner shares it.

## Commands

```sh
agent-exec status show
agent-exec status plan --id ID --status planning|done|needs-user|failed [--phase PHASE] [--repo DIR] [--title TEXT] [--questions N] [--packages N] [--est-total S] [--est-critical S]
agent-exec status close --state PATH --status done|stopped
agent-exec status whoami
```

- `status show`: rebuild and print the file.
- `status plan`: the skeleton planner's call at phase boundaries. `plan review` and `plan next` write their own plan entries.
- `status close`: the instructor closes an ended run that was left in `needs`. It is refused while the run's runner is alive.
- `status whoami`: print `{"account": <label>|null, "host": <host name up to the first ".">}`.

## Conventions

- All times are unix seconds.
- Top-level keys and object fields are documented in the tables below.

## Top level

| Key | Type | Meaning |
|---|---|---|
| `version` | int | Always `1`. |
| `generated_at` | int | Time of this rebuild. |
| `heartbeat_s` | int | Always `60`. Maximum interval between a live run's rewrites. |
| `stale_after_s` | int | Always `180`. See Freshness. |
| `summary` | object | Counts across all runs and plans. |
| `runs` | array | One object per run, newest `updated_at` first. |
| `plans` | array | One object per plan, newest first. |

## `summary`

| Key | Type | Meaning |
|---|---|---|
| `busy` | bool | A run's runner is alive, or a plan is `planning`. |
| `running` | int | Runs whose runner is alive. Runs in `needs` are included. |
| `planning` | int | Plans in `planning`. |
| `needs` | int | Runs in `needs`, plus plans in `needs-user`. |
| `held` | int | Non-ended runs that are held: new starts wait for memory pressure, plan usage or a login. |
| `crashed` | int | Runs in `crashed` or `stale`. |
| `updated_at` | int or null | Newest entry update. `null` when there are no entries. |
| `line` | string | Human-readable one-liner, e.g. `2 running · 1 needs`. Parts appear in the order running, planning, needs, held, crashed. `idle` when all counts are 0. |

## `runs[]`

| Key | Type | Meaning |
|---|---|---|
| `key` | string | 16 hex characters. Names the file in `status/runs/`. |
| `run_id` | string or null | From `--run-id`. `null` when the run was started without it. |
| `project` | string | `owner/repo` from the `origin` remote. Otherwise the main checkout's directory name. |
| `title` | string | `wave run --title`, else the run id, else the key. |
| `account` | string or null | The claude.ai account label of the profile that started the run's runner, recorded at each `wave run` start: `oauthAccount.organizationName` from Claude Code's global config (`$CLAUDE_CONFIG_DIR/.claude.json`, else `~/.claude.json`), else `oauthAccount.emailAddress`. `null` when unknown. Never a uuid. |
| `status` | string | See run statuses below. |
| `line` | string | One-liner, e.g. `wave 3/8 · 2 running · 1 needs · ~12m left · ~$1.40`. |
| `counts` | object | `total`, `integrated`, `running`, `pending`, `blocked`, `needs`, `failed`. |
| `packages` | array | Packages in flight. See the package object below. |
| `needs` | array | Open needs. See the need object below. |
| `held` | object | Present only while new starts are held: `{kind, reason, since}`. `kind` is `login`, `memory` or `usage`; `since` is a Unix time. Absent otherwise. |
| `eta_s` | int or null | Seconds left. `null` when unknown. |
| `cost_usd` | number or null | Estimate. `null` when unknown. |
| `started_at` | int | Start of this runner process. |
| `updated_at` | int | Last update. |
| `ended_at` | int or null | `null` while the run is live. |
| `pid` | int | Runner process id. |
| `state` | string | Absolute path of the run's state file. |

### Run statuses

| Status | Meaning |
|---|---|
| `running` | Runner alive, nothing open. |
| `needs` | Runner alive with needs open, or the run ended with needs open. The instructor must act. |
| `done` | Ended successfully. |
| `stopped` | Ended by stop. |
| `crashed` | Ended without its end entry: the pid is gone. |
| `stale` | Pid alive but no rewrite for 180 s. Hung, or the pid was reused. |

### Package object (`runs[].packages[]`)

| Key | Type | Meaning |
|---|---|---|
| `id` | string | Package id. |
| `phase` | string | `implementing`, `verifying`, `correcting`, `integrating` or `waiting`. |
| `elapsed_s` | int | Seconds since the package started. |
| `est_s` | int or null | Estimated total seconds. `null` when unknown. |
| `model` | string or null | Model in use. `null` when unknown. |

### Need object (`runs[].needs[]`)

| Key | Type | Meaning |
|---|---|---|
| `id` | string or null | Package id. `null` for a wave-level need. |
| `kind` | string | See need kinds below. |
| `detail` | string | First line of the need, at most 120 characters. |

### Need kinds

Kinds worth a viewer's attention:

- `login`: the Claude login of this profile expired. New starts are held until someone runs `/login` and hands the package back.
- `permission`, `network`, `delegate`: a dialog is waiting for approval in the instructor's session.

Other kinds include `escalate`, `conflict`, `self-verify`, `post-integration`, `dispatch-error` and `environment`.

## `plans[]`

| Key | Type | Meaning |
|---|---|---|
| `plan_id` | string | The run id the wave will use. Join it with `runs[].run_id`. |
| `project` | string | As in runs. |
| `title` | string | Plan title. |
| `status` | string | See plan statuses below. |
| `phase` | string | `explore`, `verify`, `critique`, `design`, `synthesize`, `reality-check`, `lint`, `review`, `specs` or `done`. |
| `questions` | int | Count of open questions. |
| `packages` | int | Count of packages. |
| `est_total_s` | int or null | `null` until known. |
| `est_critical_s` | int or null | `null` until known. |
| `started_at` | int | Plan start. |
| `updated_at` | int | Last update. |

### Plan statuses

| Status | Meaning |
|---|---|
| `planning` | Planner or spec writers are working. |
| `done` | Plan finished. |
| `needs-user` | Open questions wait for the user. |
| `failed` | Planning failed. |
| `stale` | Still `planning`, with no update for 1 hour. |

## Example

One live run in `needs` with two packages in flight and a `login` need, plus one finished plan.

```json
{
    "version": 1,
    "generated_at": 1793060000,
    "heartbeat_s": 60,
    "stale_after_s": 180,
    "summary": {
        "busy": true,
        "running": 1,
        "planning": 0,
        "needs": 1,
        "crashed": 0,
        "updated_at": 1793059900,
        "line": "1 running · 1 needs"
    },
    "runs": [
        {
            "key": "3f9a0c1d7e2b4a58",
            "run_id": "wave-20261027-api",
            "project": "yacchi/claude-plugins",
            "title": "status-file doc",
            "account": "Acme",
            "status": "needs",
            "line": "wave 3/8 · 2 running · 1 needs · ~12m left · ~$1.40",
            "counts": {
                "total": 8,
                "integrated": 3,
                "running": 2,
                "pending": 2,
                "blocked": 0,
                "needs": 1,
                "failed": 0
            },
            "packages": [
                {
                    "id": "docs",
                    "phase": "verifying",
                    "elapsed_s": 312,
                    "est_s": 600,
                    "model": "haiku"
                },
                {
                    "id": "cli",
                    "phase": "waiting",
                    "elapsed_s": 905,
                    "est_s": null,
                    "model": null
                }
            ],
            "needs": [
                {
                    "id": "cli",
                    "kind": "login",
                    "detail": "Claude login for this profile expired; run /login, then hand the package back"
                }
            ],
            "eta_s": 720,
            "cost_usd": 1.4,
            "started_at": 1793057000,
            "updated_at": 1793059900,
            "ended_at": null,
            "pid": 48213,
            "state": "/Users/example/projects/app/.claude/wave/state.json"
        }
    ],
    "plans": [
        {
            "plan_id": "wave-20261027-api",
            "project": "yacchi/claude-plugins",
            "title": "status-file doc plan",
            "status": "done",
            "phase": "done",
            "questions": 0,
            "packages": 8,
            "est_total_s": 3600,
            "est_critical_s": 1500,
            "started_at": 1793055000,
            "updated_at": 1793056800
        }
    ]
}
```
