# compact-companion

A **companion** to Claude Code's built-in compact (context compaction): records the decision rationale that the compaction summary discards in an external journal, and re-injects it automatically right after compaction.

## Background: what the built-in compact preserves — and what it doesn't

Claude Code's built-in compaction uses a fixed 9-section summary template and preserves *what happened* quite thoroughly: user messages verbatim, file and code changes, errors and fixes, pending tasks, current work, and security-relevant constraints verbatim.

However, the summary template has **no instruction to preserve**:

- Rejected alternatives and why they were rejected
- The rationale behind adopted design decisions
- Non-security constraints (operational rules, policies)
- Approaches abandoned without an error

As a result, long sessions that compact repeatedly suffer rework: rejected alternatives get re-proposed, abandoned procedures get retried. This plugin fills **only** that gap. It deliberately does not save anything the built-in compact preserves (duplicating it would risk stale copies overriding the authoritative summary after compaction).

## How it works

```
Session start ──▶ SessionStart(startup|resume|clear) hook
                    └─ announces the journal path via additionalContext

Decisions during work ──▶ decision-log skill (auto-triggered)
                    └─ appends adopted/rejected/constraint/abandoned entries, one line each

Compaction ──▶ SessionStart(compact) hook
                    └─ injects the journal via additionalContext:
                       "if the summary contradicts the journal, the journal wins"
```

- Journal: `~/.claude/compact-companion/logs/<session_id>.md` (outside any repo, local only, never sent anywhere)
- Journals older than 14 days are deleted automatically at session start
- Injection is capped at ~16KB from the tail (newest decisions win)

## Components

| Component | Role |
|-----------|------|
| `skills/decision-log/` | Appends decisions to the journal as they happen (also invocable via `/decision-log`) |
| `hooks/scripts/announce-journal.sh` | Announces the journal path at session start |
| `hooks/scripts/inject-decision-log.sh` | Injects the journal right after compaction |

## Installation

```bash
/plugin install compact-companion@yacchi-plugins
```

## Usage

Works automatically once installed. Recording relies on the skill's auto-trigger; saying "record this decision" right after an important call makes it more reliable.

Journal format:

```
- [adopted] Session cookies over JWT — reason: token revocation is impractical on the existing infra
- [rejected] Adding a Redis cache layer — reason: user will not accept the added operational cost
- [constraint] No deploys to staging (source: user instruction)
- [abandoned] Parallelizing E2E tests in CI — reason: test DB cannot be isolated, becomes flaky
```

## Requirements

- `jq` (used by the hook scripts; if missing, the hooks exit silently and harmlessly)

## Limitations & design principles

- Skill auto-triggering is best-effort. Not every decision is guaranteed to be recorded, but whatever is recorded is reliably restored
- If the built-in compact ever starts preserving decision rationale and rejected alternatives, this plugin can simply be retired — until then it cannot conflict with upstream improvements, because it only stores what upstream explicitly does not
- Hooks never access the network
