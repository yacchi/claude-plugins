---
name: cleanup
description: Sweep leftover orchestra worktrees and branches out of the current repository after the fact — the ones the SessionEnd cleanup hook never reclaimed because the session crashed, predates the hook, ran with it disabled, or left a dirty tree behind on purpose. Shows a dry-run report first, keeps anything holding uncollected work, and removes the rest on confirmation. Invoke with /cleanup (cross-plugin, `orchestra:cleanup`).
when_to_use: Use when orchestra worktrees have piled up in a repo (`git worktree list` shows many `orchestra/*` entries), when the user asks to clean up worktrees / 掃除 / 残骸を消したい, or when `orchestra/*` branches with no worktree are cluttering the branch list. Do not use it to remove the CURRENT session's worktrees mid-run — those are still in use and the SessionEnd hook reclaims them on its own.
---

# cleanup: reclaim leftover orchestra worktrees

`agent-exec dispatch --isolate` gives every worker a worktree on an `orchestra/<session8>/<task>` branch. `hooks/cleanup-worktrees.sh` reclaims them at `SessionEnd` — but only the finishing session's, and only when the hook actually runs. Everything else survives with nothing to reclaim it:

- a session that crashed, was killed, or ended before the hook shipped (< v0.26.0);
- `ORCHESTRA_SESSION_CLEANUP=off` or `enforcement.session_cleanup: "off"`;
- legacy `orchestra/<task>` worktrees with no session segment (`--session` never matches them);
- worktrees that were **dirty** at SessionEnd and were kept on purpose — a worktree holding uncollected work is the only record of what a worker tried, so the hook keeps it and the user is expected to deal with it later. "Later" is this skill.

This skill drives `agent-exec isolate sweep`, which is repo-wide rather than session-scoped.

The orchestrated pipeline sweeps at **run end** (`run` SKILL.md §5/§10), which is the reliable moment — nothing is racing it and every diff is already collected. `SessionEnd` and this skill are the two backstops for everything that still slips through.

## 1. Report first, always

Never sweep blind. Start with the dry run:

```bash
agent-exec isolate sweep --dry-run --text
```

Read the statuses:

| status | meaning | what to do |
|---|---|---|
| `would-remove` | clean, or already collected — safe to reclaim | sweep it |
| `dirty` | holds changes nobody collected | **keep**; surface it to the user (see §3) |
| `skipped` | the running session's own tree, another session still active, or newer than `--older-than` | leave it |
| `orphan` (branches) | an `orchestra/*` branch whose worktree is gone | report; delete only if the user says so |

Summarize for the user in their language: how many would be removed, how many are dirty (with task id and path), how many orphan branches. Do not paste the JSON.

## 2. Sweep

With the user's go-ahead:

```bash
agent-exec isolate sweep --text                    # remove clean worktrees
agent-exec isolate sweep --branches --text         # ...and delete orphan orchestra/* branches
agent-exec isolate sweep --older-than 7 --text     # only what is at least 7 days old
```

The sweep runs `git worktree prune` first, so worktrees whose directory was deleted by hand stop being reported. It applies exactly the same review gate as `isolate remove`, so nothing dirty is destroyed without `--force`.

Flags worth knowing:

- `--repo <path>` — sweep another checkout (default: cwd).
- `--older-than <days>` — age is the branch tip's commit date. Good default when the user is unsure.
- `--live-window <minutes>` — how recently *another* session must have been active for its worktrees to count as in use (default 120). Sessions are tracked by a heartbeat file that every `agent-exec isolate`/`dispatch`/`route`/`run` call refreshes; `0` disables liveness entirely.
- `--include-live` — sweep another session's worktrees even though it looks active. Only with a clear instruction: a parallel session may be mid-run.
- `--include-current` — also sweep the **running** session's worktrees. Only when the user explicitly wants it and no run is in flight; otherwise a live worker loses its tree mid-task.
- `--branches` — delete `orchestra/*` branches with no worktree. Off by default: a branch is the last copy of whatever it holds.
- `--force` — discard uncollected work. **Never pass this on your own judgment** — only when the user has been shown the dirty list and said to discard it.
- `--json` (default) / `--text` — use `--text` when you are going to read it; the JSON shape is for scripts.

## 3. Handling dirty worktrees

A dirty worktree is unreviewed worker output. For each one, offer the user the three real options rather than deciding:

```bash
agent-exec isolate diff --task <id> --names-only   # what is in there (also marks it collected)
agent-exec isolate collect --task <id>             # "I took this out by other means"
agent-exec isolate sweep --force                   # discard everything still dirty
```

`isolate diff` and `isolate collect` both mark the tree collected, after which a plain `sweep` reclaims it — that is the safe route. `--force` is the destructive one and needs an explicit instruction.

## 4. Scope and non-goals

- **One repository.** Sweep reads `git worktree list` for the repo you point it at; it does not walk the filesystem hunting for other checkouts. Run it again with `--repo` for each repo that needs it.
- **Only `orchestra/*`.** Worktrees and branches orchestra did not create are never listed and never touched.
- **Not a substitute for the SessionEnd hook.** That hook stays on and handles the normal case; this skill is for the leftovers.
- **Never commit, rebase, or push** anything while cleaning up.
