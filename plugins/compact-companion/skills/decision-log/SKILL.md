---
name: decision-log
description: Appends decision rationale — which the built-in compact summary does not preserve — to an external journal. Use automatically the moment (1) a design or implementation approach is decided (adopted), (2) an alternative is rejected, (3) the user states a constraint, rule, or prohibition, or (4) a tried approach is abandoned or reverted. Also triggered by "record this decision", "decision log", or "decision journal".
---

# decision-log

The built-in compact summary preserves *what happened* (user messages, file changes, errors and fixes, task state) but not *why*. This skill appends only that missing part to an external journal, which a hook re-injects automatically after compaction.

## What to record (these 4 kinds only)

| Tag | Target |
|-----|--------|
| `[adopted]` | A design/implementation decision, with its rationale |
| `[rejected]` | An alternative that was considered but not adopted, with the reason |
| `[constraint]` | A constraint, rule, or prohibition stated by the user (including non-security ones) |
| `[abandoned]` | An approach that was tried and withdrawn, with the reason (including ones without errors) |

## What NOT to record (the built-in compact already preserves these)

- Work performed, edited files, code snippets
- Errors and how they were fixed
- Task progress and next steps
- Contents of user messages

## Journal path

Use the path announced in the `[compact-companion]` system-reminder at session start (`~/.claude/compact-companion/logs/<session_id>.md`).

If no announcement is found, skip recording and continue working (do not treat it as an error; do not guess or invent a path).

## How to append

The moment a decision occurs, append one line via Bash (the file is created on first append):

```bash
printf '%s\n' '- [rejected] Adding a Redis cache layer — reason: user will not accept the added operational cost' >> "$HOME/.claude/compact-companion/logs/<session_id>.md"
```

Format:

```
- [adopted] <decision> — reason: <why>
- [rejected] <alternative> — reason: <why not>
- [constraint] <constraint/rule> (source: user instruction, etc.)
- [abandoned] <approach> — reason: <why it was dropped>
```

## Notes

- One line per decision. Be concise, but never omit the reason (the reason is the whole point)
- Append immediately after deciding, not in a batch later (auto-compact fires without warning)
- Never write credentials, API keys, or other secrets
- Skip trivial choices (variable names, etc.). Record only decisions whose loss after compaction would cause rework
