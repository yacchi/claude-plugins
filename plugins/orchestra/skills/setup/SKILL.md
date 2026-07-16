---
name: setup
description: Interactively detect Codex/Copilot CLI and agent availability, then configure orchestra's model tiers and external-executor settings, writing YAML at project (`.claude/orchestra.yaml`) or user (`~/.claude/orchestra.yaml`) scope with correct deep-merge override semantics. Invoke with /setup (or, cross-plugin, `orchestra:setup`), or whenever the user wants to enable/disable Codex or Copilot dispatch, change model tiers, set a project-specific override, or migrate an old orchestra.json.
when_to_use: Use when the user asks to configure orchestra, turn Codex or Copilot on/off, add a per-project override on top of their personal orchestra config, or convert a legacy orchestra.json to the current YAML format. Do not use this skill to actually run a task through the pipeline — that's the `orchestra:run` skill; this one only edits its config file.
---

# setup: configuration skill

This skill is a small companion to `run` (see its SKILL.md §9 for the config schema this file edits). It exists to make setting up `.claude/orchestra.yaml` / `~/.claude/orchestra.yaml` interactive and safe — detect what's actually available, ask where to write the change, and edit without clobbering the user's existing comments. It does not relitigate the schema itself, and it does not run the orchestration pipeline.

## 1. Config file locations & format

- Project scope: `.claude/orchestra.yaml` (`.yml` also recognized)
- User scope: `~/.claude/orchestra.yaml` (`.yml` also recognized)
- Format is YAML, not JSON, specifically so the file can carry comments. When editing a file that already exists, use `Edit` on just the lines that change — never blindly overwrite the whole file with `Write`, or the user's own comments and layout are lost.
- Legacy `.claude/orchestra.json` / `~/.claude/orchestra.json` (the pre-YAML format) are no longer read by `orchestra:run`. See step 4 for what to do when one is found.

## 2. Merge semantics (what a "per-project override" actually does)

Effective config = deep-merge(built-in defaults, user config, project config), applied in that order — later layers win. When explaining this to the user or deciding what to write:

- **Mapping keys merge recursively, key by key.** A project file that sets only `external_executors.copilot.enabled: true` does not need to repeat `codex`'s block or copilot's `model_policy` — those are inherited from the user config (or defaults, if the user has no config either).
- **Scalars and arrays are replaced wholesale** by the more specific layer, never concatenated or merged element-by-element. `role_priority.<role>.<archetype>` is a list, so this applies to it too: a project override that wants to move one executor must restate the *entire* ordered list for that role/archetype, not just the one entry it's changing — a partial list silently drops the rest of the priority order, it does not merge with the inherited one.
- **An explicit `null`/`~` is a value, not "reset to default."** To inherit a key, omit it entirely — don't null it out.

Always write the *smallest* diff that expresses the user's intent: a project override file should contain only the keys being changed at that scope, not a full copy of the schema. Dumping the whole schema into a project file defeats the purpose of layered overrides and makes future user-level changes silently stop applying to that project.

## 3. Detection

Run this before asking the user anything, and report the findings plainly (e.g. "Codex CLI: found at /usr/local/bin/codex. codex:codex-rescue agent: available in this session. Copilot CLI: not found.") before proceeding to step 5:

- **Codex** — two independent checks, since they gate different things:
  - `command -v codex` — whether the Codex CLI binary itself is installed. Informational only: `external_executors.codex` in this plugin's schema uses `dispatch: agent` (routes through the `codex:codex-rescue` subagent), not this CLI directly.
  - Whether `codex:codex-rescue` / `codex:setup` currently resolve as available agent/skill names in this session (check the available-agents system reminder or the skills list). This is what actually determines whether `dispatch: agent` will work right now — if it doesn't resolve, `orchestra:run` falls back to normal Claude tiers for that role automatically (SKILL.md §9), so this is a soft signal, not a hard gate.
- **Copilot** — `command -v copilot`. This one *is* invoked directly via `dispatch: cli`, so its presence is the real functional gate: enabling `external_executors.copilot` without the CLI installed will fail loudly the first time it's dispatched.
- Absence of either is informational, never a reason to silently refuse to enable something the user asked for — just tell them what will happen (soft fallback for Codex, hard failure at call time for Copilot) and let them decide.

## 4. Legacy JSON migration check

Check for `.claude/orchestra.json` and `~/.claude/orchestra.json`. If either exists and there is no sibling `.yaml`/`.yml` at that same scope:

1. Read the JSON file.
2. Show the user the equivalent YAML (same keys, structure carried over 1:1 — use `examples/orchestra.yaml`, shipped alongside the `orchestra:run` skill, as the commented template to fill in).
3. Ask whether to write the new `.yaml` file and remove the old `.json`. Never do this silently or delete the old file without asking — it may not be tracked in git and could be someone else's in-progress edit.

## 5. Interactive flow

1. Detect availability (step 3) and check for a legacy JSON config (step 4).
2. Read whatever `.claude/orchestra.yaml` and `~/.claude/orchestra.yaml` currently exist. Compute and show the user the *effective* merged config (step 2's algorithm), noting for each non-default value which layer it actually comes from (project / user / default).
3. Ask the user (AskUserQuestion) what to change: which executor(s) to enable/disable, whether to touch model tiers, whether to set or reorder `role_priority` (e.g. prefer Copilot for investigation-style worker fan-out, with reactive fallback to the next entry — see `run` SKILL.md §9), and — this is the important one — **at which scope**. Default guidance: project scope for anything specific to this repo or team (checked into git, shared), user scope for a durable personal default that should apply everywhere. If the user doesn't already have a config at the scope they pick, that's fine — step 4 of §5 creates it.
4. Apply the change:
   - Target file doesn't exist yet: create it from `examples/orchestra.yaml` (shipped alongside the `orchestra:run` skill), but **trim it down to only the keys actually being set** — see the "smallest diff" rule in step 2. Keep a short comment on any field whose meaning isn't obvious from the key name alone.
   - Target file exists: use `Edit`, touching only the changed lines. Preserve everything else, comments included.
5. Recompute and show the resulting effective config so the user can confirm the change did what they intended before considering the task done.

## 6. What this skill does not do

- It does not change the model-tier defaults documented in `run` SKILL.md §3 — those are hard-coded fallback values; this skill only edits the override file that sits on top of them.
- It does not verify that Codex or Copilot actually work end-to-end (auth, quota, etc.) — for the Codex CLI itself, that's the separately-installed `codex:setup` skill's job. This skill's scope is exactly one file: orchestra's own config.
