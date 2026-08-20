# Configuration reference (orchestra `run`)

Detail split out of `SKILL.md` in v0.12.0. The text is unchanged, and `SKILL.md`'s section numbering was kept stable across that split, so every `§N` reference below still resolves.

Load this file only when a configuration question actually arises: what a key means, how the four layers merge, why `route` dropped a candidate, what `enforcement.light_class` does, or what telemetry and the run ledger record. For normal orchestration you do not need it — one `agent-exec doctor --json` call already returns the resolved config, and `agent-exec route`/`dispatch` already execute the priority walk.

## 1. Configuration file and external executors


At the start of every orchestration, the instructor resolves configuration from up to four layers, merged in this order (later layers win):

1. **Defaults**: `tiers` — light: haiku, standard: sonnet, deep: opus, review: sonnet (unchanged — these remain the Claude-side fallback models `agent-exec route` resolves to). `external_executors.copilot` and `external_executors.codex` now ship **`enabled: true`** out of the box, each with its `class_policy` (Copilot: `gpt-5.6-luna`/medium for `light`+`standard`; Codex: `gpt-5.6-luna`/medium `standard`, `gpt-5.6-sol`/xhigh `deep`, `gpt-5.6-sol`/low `review`) and a built-in `priority` (`light: [copilot, claude]`, `standard: [copilot, claude, codex]`, `deep: [claude, codex]`, `review: [claude]`, `independent-review: [codex]`), plus `enforcement.light_class: off` and `ledger.enabled: true`, `ledger.dir: "~/.claude/orchestra/runs"`, `ledger.retention_days: 30`. Shipping external executors enabled by default is safe because `agent-exec route` gates every candidate on actual availability (below) — on a machine with neither CLI installed, every call still resolves to `claude`.
2. **User**: `~/.claude/orchestra.yaml` (or `.yml`), if present.
3. **Project**: `.claude/orchestra.yaml` (or `.yml`), if present — checked into git, shared with the team.
4. **Project-local**: `.claude/orchestra.local.yaml` (or `.yml`), if present — this developer's personal override for this one project. Mirrors Claude Code's own `settings.json` / `settings.local.json` split; never commit this file (see `setup` SKILL.md §1).

This is a **deep merge**, not a first-found-wins lookup: object/mapping keys are merged recursively key-by-key, so a project file only needs to state the keys it actually wants to change — e.g. a project override of just `external_executors.copilot.enabled: true` inherits everything else (codex's whole block, copilot's `class_policy`, etc.) from the user config or defaults. Scalars and arrays are replaced wholesale by the more specific layer, not concatenated or element-merged. An explicit `null`/`~` is a value, not "reset to default" — omit the key entirely to inherit.

**Upgrade trap for existing configs:** the key-by-key merge means a *partial* `priority` override does not insulate the untouched classes from new built-in defaults. A config written before v0.11.0 that overrides only, say, `priority.deep` (to add Codex there) will, after upgrading, silently inherit the *entire new* `priority.light`/`priority.standard` maps from the v0.11.0 defaults — i.e. the new copilot-first ordering — because those keys were never stated in the user's own file and so were never "theirs" to keep frozen. If you want to keep pre-v0.11.0 behavior for a class you didn't mean to touch, state that class's `priority` list explicitly in your own config, or set `external_executors.copilot.enabled: false` to opt out of external executors altogether.

The format is YAML, not JSON, specifically so the file can carry comments (JSON can't). `.claude/orchestra.json` / `~/.claude/orchestra.json` (the pre-YAML format) are no longer read — use the `setup` skill (`orchestra:setup`) to convert an old one.

Instead of merging these four layers in-context, the instructor can obtain the already-resolved configuration deterministically via **`agent-exec config [--json]`** — it deep-merges the same four layers with the same precedence/merge rules described above and additionally reports, per `external_executors.<name>` with `enabled: true` and `dispatch: cli`, whether its executable resolves on `PATH` (`"available": true/false`, or `null` if the name has no built-in profile). This keeps the merge logic out of the instructor's own context. **Prefer a single startup call, though:** `agent-exec doctor --json` (needed anyway for the `dispatch: cli` pre-flight below) now embeds this exact resolved config under `config.values` alongside its readiness report, so one `doctor` call yields both the `ready.<executor>.ok` verdicts *and* the resolved `tiers` / `external_executors` / `priority` — a standalone `config` call is only worth it when you want the config and nothing else. Likewise, Copilot's `dispatch: cli` invocation may use **`agent-exec run <profile> --model M --effort E --workdir W --prompt-file F [--prompt-file G ...] [...] [--run-id ID]`** as a normalized entry point; a supplied `--run-id ID` tags the ledger line with that run. With `--capture`, it runs copilot as a subprocess and prints one normalized `{ status, answer, session_id, reason, exit_code }` JSON object to stdout — see `references/external-executors.md` §5 for details.

`--prompt-file` is repeatable on both `dispatch` and `run`: files are read in order and concatenated with exactly one blank line between consecutive files. One file is backward-compatible; a missing or unreadable file is a usage error (exit 2), names the path on stderr, and dispatches nothing.

**`agent-exec route` / `agent-exec dispatch`: the walk itself is code, not instructor judgment.** `agent-exec route --class <light|standard|deep|review|independent-review> [--archetype default|investigation] [--exhausted a,b] [--json|--text]` runs the entire `priority` walk described below in one call: it deep-merges the four config layers, then hard-gates each candidate on reality — dropping it if `enabled: false`, its binary/agent doesn't resolve, `doctor`'s own `ready.<x>.ok` is false, it has no `class_policy` entry for the class, or the caller listed it in `--exhausted` — and returns the surviving top candidate (`executor`, `dispatch`, `model`, `effort`, `agent_type`) plus the full `candidates`/`remaining`/`skipped` trail. The pre-flight `doctor` check described above is folded into this call; it is no longer a separate step the instructor performs before building a `priority` candidate list. `agent-exec dispatch --class <cls> --prompt-file F [--prompt-file G ...] --workdir W [...] [--run-id ID]` goes one step further: it calls `route` internally, and if the winning candidate is a `dispatch: cli` executor it also runs it, returning `{status: 'ok'|'unavailable', answer, session_id, reason, exit_code, executor, model, effort, route}`; if the winner is Claude or a `dispatch: agent` executor (Codex), it returns `{status: 'delegate', executor, model, effort, agent_type, route}` instead, since only the instructor's own `agent()`/Agent-tool call can spawn a Claude or plugin subagent. With nothing viable it returns `{status: 'unroutable', route}`. This is exactly what §5's `dispatchClass()` helper wraps: **the instructor never decides which executor runs a `light`/`standard` task — it asks, via one relay agent, and branches on the answer.** This availability gate is exactly what makes shipping external executors `enabled: true` by default safe (point 1 above).

As of v0.4.0, the configuration vocabulary itself was renamed from role names to capability/performance classes: `tiers.worker/hard_worker/verifier` → `tiers.light/standard/deep` + `review`, `external_executors.*.roles` → `classes`, `model_policy` → `class_policy`, `role_priority` → `priority`, and `long_context_escalation.{model, effort}` → `long_context_escalation.{class}`. Pre-0.4 keys using the old role vocabulary are no longer read — use the `setup` skill (`orchestra:setup`) to convert an old config to the new vocabulary, the same way it converts legacy JSON to YAML.

A copy of the schema, fully commented, ships with this plugin at `examples/orchestra.yaml`. Full shape:

```yaml
tiers:
  # implementation capability classes (cheapest/fastest -> hardest). These
  # are the CLAUDE-side fallback models `agent-exec route` resolves to when
  # no external executor is enabled/ready for the class - see `priority`
  # below for what actually gets tried first.
  light: haiku       # fast, mechanical, fully-specified work
  standard: sonnet   # normal implementation with modest design latitude
  deep: opus         # design-sensitive / hard problems needing real judgment
  # verification role
  review: sonnet     # mandatory same-run adversarial review of the work
  # `independent-review` has no built-in Claude model on purpose - it exists to
  # add a DIFFERENT provider's eyes; see `priority.independent-review` below.

external_executors:
  codex:
    enabled: true
    dispatch: agent
    agent_type: codex:codex-rescue
    classes:
      - standard
      - deep
      - review
    class_policy:
      standard:
        model: gpt-5.6-luna
        effort: medium
      deep:
        model: gpt-5.6-sol
        effort: xhigh
      review:
        model: gpt-5.6-sol
        effort: low
    long_context_escalation:
      when: task requires deep traversal of a large repo (Luna's long-context recall is weak)
      class: deep

  copilot:
    enabled: true  # ships true by default; gated on binary+doctor readiness, see §9 below
    dispatch: cli
    # dispatched via the agent-exec wrapper: copilot -p 1.0.74 runs file/shell/
    # network tools autonomously in non-interactive mode without any allow-all
    # flag or env var, so agent-exec injects nothing — the only permission
    # rule needed is Bash(agent-exec:*). --disable-builtin-mcps drops the
    # builtin github-mcp-server/customize-cloud-agent tools from the surface
    # (lower token/latency cost); --add-dir scopes the filesystem to workdir.
    # (see the dispatch: "cli" notes below, and the M2 sandboxing caveat)
    command: >-
      agent-exec copilot -p {promptfile} --model {model} --effort {effort}
      --add-dir {workdir} --output-format json --disable-builtin-mcps
    resume_command: >-
      agent-exec copilot --resume={session_id} -p {promptfile} --model {model} --effort {effort}
      --add-dir {workdir} --output-format json --disable-builtin-mcps
    classes:
      - light
      - standard
    class_policy:
      light:
        model: gpt-5.6-luna
        effort: medium
      standard:
        model: gpt-5.6-luna
        effort: medium

# Persisted, time-decaying executor cooldown. Entries are written automatically
# when `agent-exec run --capture` or `agent-exec dispatch` parses
# `status: unavailable`; routing loads active entries as an additional candidate
# gate. `0` means no cooldown.
cooldown:
  enabled: true
  path: ~/.claude/orchestra/executor-state.json
  seconds:
    rate-limit: 900
    quota: 3600
    credits: 3600
    auth: 0
    nonzero-exit: 0

# copilot ships `enabled: true` above, so this list actually prefers it
# when ready - `agent-exec route`/`dispatch` execute the walk, not the
# instructor (see the route/dispatch paragraph above).
priority:
  light:
    investigation: [copilot, claude]
    default: [copilot, claude]
  standard:
    default: [copilot, claude, codex]
  deep:
    default: [claude, codex]
  review:
    default: [claude]
  independent-review:
    default: [codex]

# Local executor usage ledger. It is default-on, local-only, and independent
# of telemetry. It records only when Claude Code supplies a session id.
ledger:
  enabled: true
  dir: "~/.claude/orchestra/runs"
  retention_days: 30

# Nudges and guards, never hard walls - see the `enforcement` paragraphs below
# for the escape hatches. NOTE: quote "off" - YAML 1.1 parses a bareword `off`
# as boolean False, not the string "off".
enforcement:
  light_class: "off"    # opt-in: nudge a generic Haiku spawn toward dispatch
  worker_vcs: "block"   # default ON: no destructive VCS from a subagent
  turn_edits: 8         # main-thread edits in one turn before asking to re-route
```

Run ledger files for tagged runs use an atomically allocated, per-session
ordinal prefix such as `001-wf_153ae50e-9bf.jsonl`; this orders a session's
runs by execution, and a bare ordinal (`3`, `03`, or `003`) addresses that run
only within the current `CLAUDE_CODE_SESSION_ID` session. The harness run id
stays in the filename because the Claude source resolves transcripts through
the harness's own `workflows/<run-id>` directory, which a locally minted id
could not address. Older un-prefixed files are still read and are not migrated.

To set this up interactively (detect whether Codex/Copilot are actually available in this environment, choose project vs user scope, edit the file in place without clobbering existing comments), use the `setup` skill instead of hand-editing — see its own SKILL.md.

**`tiers`** overrides the model-class defaults of section 3. Values are model aliases (or full model IDs) used whenever this skill — or `agent-exec route` — resolves a class/role to a Claude model: the `claude` candidate in `priority`, or any class/role with no external executor configured for it.

**`external_executors`** declares non-Claude executors (Codex, Copilot, etc.) that may be woven into the pipeline. Only entries with `"enabled": true` are used. Two dispatch mechanisms:

- **`dispatch: "agent"`** — the executor is an installed plugin subagent. Pass `agent_type`'s value as `agentType` in Workflow `agent()` calls, or as `subagent_type` in the Agent tool. **The route's `model`/`effort` must be appended to the prompt text as `--model <m> --effort <e>`, not passed as `agent()` options** — those options select a *Claude* tier and are meaningless to a plugin subagent, while Codex's rescue agent explicitly leaves the executor's model and reasoning effort unset unless the request names them. Drop them and the run silently falls back to whatever `~/.codex/config.toml` defaults to, which is typically a different tier entirely (e.g. `gpt-5.6-sol`/`low` instead of the resolved `gpt-5.6-luna`/`medium`). See `dispatchClass()`'s `routingFlags()` helper in SKILL.md §5. If the agent name doesn't resolve in this environment, fall back to the normal model tier for that class/role.
- A delegated Codex dispatch is correlated primarily by a one-way fingerprint of its prompt-file paths, because the path is required for the task to be carried out at all and therefore survives request rewriting, whereas a decorative marker can be dropped at no cost. The ledger stores only those fingerprints and never paths; the marker remains a secondary signal.
- **`dispatch: "cli"`** — the executor is a non-interactive CLI (Copilot today). The instructor prepares a token with `agent-exec dispatch prepare`; `dispatchClass()` (§5) gives only that token to one relay agent, which runs `agent-exec dispatch --token ... --capture` and hands back its JSON verbatim. `dispatch` calls `route` internally, so the `ready.<executor>.ok` gate has already been applied before the CLI ever runs.
  **Not a security boundary (M2):** `copilot -p` is not reliably confinable by its own tool-permission flags — excluding a named tool (e.g. `--excluded-tools=bash`) has been observed to be routed around via the `task` tool rather than actually blocked. `--allow-tool`/`--deny-tool`/`--excluded-tools` are defense-in-depth at best, never treat them as a containment guarantee; real containment needs an external OS sandbox (container, `sandbox-exec`, restricted user, network egress control) or a disposable worktree, which this plugin does not implement — treat any `dispatch: cli` Copilot task as an autonomous, directory-scoped agent, and prefer a disposable workdir (a scratch clone or worktree it can freely write/execute in) over the user's primary working tree when the task doesn't need to persist there. A `--deny-tool` list may still be set by the user as an optional, explicitly-advisory knob, but document it to them as narrowing the *named* surface only, not as a boundary.

**`classes`** controls where the executor is used:
- `"light"`: as a light-class implementer (in place of, or alongside, the Claude `light` tier).
- `"standard"`: as a standard-class implementer (in place of, or alongside, the Claude `standard` tier / `model: 'sonnet'`).
- `"deep"`: as a design-latitude implementation class (in place of, or alongside, `orchestra-deep`/Opus) — only meaningful for an executor whose `class_policy` names a model strong enough for that class (see the Codex policy below; Copilot's shipped example intentionally omits this class — see the Copilot section).
- `"review"`: as the same-run adversarial review pass (in place of, or alongside, the Claude `review` tier / `orchestra-review`).
- `"independent-review"`: as a third-party review pass in addition to the Claude review — useful to avoid single-provider model bias. An independent review supplements `orchestra-review`; it does not replace the structured-verdict contract, so wrap its output into the same verdict shape. Because an external, CLI-backed reviewer may ignore the Workflow `schema:` option, force JSON in the prompt and normalize the reply with a tolerant parser — see `references/external-executors.md` §4 (`parseExternalVerdict`).

**`class_policy`** maps each class this executor participates in to a concrete `{ model, effort }` pair to pass to that executor. Without it, the executor runs on its own default model, which defeats the purpose of cost-tiering by external provider just as surely as an unpinned Claude `agent()` call does (rule #4, section 4). Treat this the same way: every external-executor class in `classes` should have a matching `class_policy` entry.

**`priority`** declares, per class/role (and, for `light`, per task archetype — `investigation` vs `default`), an ordered candidate list to try — this list is still the single source of truth for preference order. What changed is *who walks it*: **`agent-exec route`/`dispatch` execute the walk**, not the instructor. `route` tries candidates left-to-right and drops one only on a **reactive fallback signal** — the executor is *unavailable*: `enabled: false`, its binary/agent doesn't resolve, `doctor`'s `ready.<x>.ok` is false, it has no `class_policy` entry for the class, or the caller passed it in `--exhausted`. This is the crucial **unavailable-vs-failed** distinction: a task that runs but returns a wrong result is NOT a fallback signal — it stays on the same executor and goes through the normal review/retry loop; only a genuinely unavailable executor (rate-limit/usage-window cap, credit exhaustion, auth failure) makes `route` drop to the next candidate. Once `dispatch` reports an executor `unavailable` for a task, the instructor's one remaining manual job is to **carry that executor forward in `--exhausted` for every subsequent `route`/`dispatch` call in the same run** (§5's `dispatchClass()` does this automatically via its module-level `exhausted` Set) — this is sticky exhaustion, and it is the instructor's only bookkeeping in the whole selection process. `priority` **supersedes `classes` for ordering** when present for a class/role; `classes` remains the legacy fallback when `priority` is absent (`route`/`dispatch` apply this same fallback internally). Operational detail — per-executor unavailable signals and the priority-walk logic `route` implements — lives in `references/external-executors.md`; read it before second-guessing a fallback decision.

**`cooldown`** enables the persisted, time-decaying cross-run layer beneath the in-run `--exhausted`/sticky exhaustion `Set`. `seconds` is a mapping, so deep-merge combines it per key; unlike the `priority` lists, it is not replaced wholesale. The CLI surface is `agent-exec cooldown` to inspect state, `agent-exec cooldown clear [executor]` to reset it, and `--no-cooldown` on `route`/`dispatch` to bypass it for one call.

**`enforcement.light_class`** (`"off"` default | `"block"` — quote the value; YAML 1.1 parses a bareword `off` as boolean `False`, not the string) is a **nudge with a guaranteed escape, not a hard wall**. When set to `"block"`, a `PreToolUse` hook (`hooks/enforce-router.sh`) fires only when `agent-exec route --class light` reports a non-`claude` executor ready — i.e. only when there's genuinely something better to redirect to — and the call would spawn a **generic** Claude implementer: no `subagent_type` at all, or `subagent_type` in `general-purpose`/`claude`, at a Haiku-class model. **Naming any other specific `subagent_type` is itself the carve-out**: the Agent tool has no per-call tool-restriction parameters — its schema is only `description`/`isolation`/`model`/`prompt`/`run_in_background`/`subagent_type` — so a named agent is how tool access and a specialized system prompt actually get pinned, and Copilot can substitute for neither; `orchestra:orchestra-light` itself stays a deny target, since redirecting it to `agent-exec dispatch` is the whole point. Escape hatches:

1. **Hard cap: one deny per session, full stop.** Not a per-task or per-prompt cap — after the first nudge the hook goes inert for the rest of the session regardless of what's asked next. (A prompt-keyed fingerprint doesn't work here: this plugin's own retry convention appends new feedback text to the prompt every round (§5), which would re-deny every round of the same task — so the bound is session-wide instead.)
2. **Explicit escape marker.** `[orchestra:allow-claude: <reason>]` anywhere in the prompt/description allows immediately, no deny recorded — for a deliberate choice to stay on Claude.
3. **Named-subagent carve-out.** Any call naming a specific `subagent_type` other than `general-purpose`/`claude` is always allowed (see above) — `Explore`, `Plan`, `statusline-setup`, `claude-code-guide`, `orchestra-review`, and any other installed agent all qualify; only a generic/anonymous Haiku-class call (or `orchestra:orchestra-light` itself) is a deny candidate.
4. **Kill switch + fail-open.** `ORCHESTRA_ENFORCEMENT=off` short-circuits to allow, and every uncertainty (`agent-exec` missing, `config` failing/slow, no `python3`, unparseable stdin, anything unexpected) fails open — it never denies on doubt.

Ships **`"off"`** — turning it on is a deliberate opt-in via the `setup` skill or by hand-editing `orchestra.yaml`, not a default behavior change.

**`enforcement.worker_vcs`** (`"block"` default | `"off"` — quote the value) blocks a **subagent** from running destructive VCS commands (`git checkout -- <paths>`, `git checkout .`, `git restore` without `--staged`, `git reset --hard/--merge/--keep`, `git clean -f`, `git stash push`, `git worktree remove --force`) against a working tree that is not one of orchestra's own (`orchestra/*` branch). It ships **on**, unlike `light_class`, because it prevents data loss rather than steering a cost decision: transcripts recorded 35 such commands run by workers of every model tier, and three escalating generations of prose prohibition in the worker prompts did not stop them (`references/isolation.md` §0). Normalization is correspondingly inverted — an ambiguous value keeps the guard on; only an explicit `off` (string, or the bareword YAML loads as `False`) disables it. Scope and escapes:

1. **Main thread untouched.** Only calls carrying an `agent_id` (i.e. from inside a subagent) are candidates. An instructor resolving a rebase conflict with `git checkout --ours` is doing its job.
2. **A worker's own worktree is its own business.** On an `orchestra/*` branch the command is allowed — there is nothing there but that worker's work.
3. **The supervising layer is exempt.** `orchestra-delegate` owns snapshots, rollbacks, and merges by design.
4. **Not fooled by text.** Heredoc bodies and quoted strings are stripped before matching, so writing the prohibition itself into a worker prompt (`cat > task.txt <<EOF … never run git reset --hard … EOF`) is not mistaken for running it.
5. **Escape marker + kill switch + fail-open.** `[orchestra:allow-vcs: <reason>]` in the command or its description, `ORCHESTRA_VCS_GUARD=off`, and fail-open on every uncertainty.

**`enforcement.turn_edits`** (`8` default | any positive integer | `"off"`) is the turn-size tripwire. Once the **main thread** has hand-edited that many files inside a single turn, a `PostToolUse` hook (`hooks/count-turn-edits.sh`) injects one piece of context asking it to re-classify into the orchestrated lane. It never blocks an edit. The number comes from turn-level transcript analysis: orchestra was invoked in 1 of the 102 turns that went on to hand-edit 10+ files, because the prompts that produce those turns ("作業を続けて", "実装して", "y") look small at classification time and only reveal their size once work is underway. Subagent edits are not counted — that is the delegation working. Fires at most once per turn, stays silent in `cheap`-model sessions, and `ORCHESTRA_TURN_GUARD=off` disables it outright. An unusable value (0, negative, a non-integer string) falls back to the default rather than silently disabling the tripwire.

**Safety note:** `cli` dispatch only ever executes command templates that come from the user's own configuration file (`.claude/orchestra.yaml` or `~/.claude/orchestra.yaml`). This plugin ships no CLI commands of its own and must never invent one; if no config file declares a CLI executor, `cli` dispatch is unavailable. The `agent-exec` binary those templates call through is not part of this plugin either — it's a separate tool the user explicitly installs and authorizes via their own `Bash(agent-exec:*)` permission rule (see the `setup` skill); this plugin only ever writes the command template that invokes it, never runs it directly, and never grants it permission on the user's behalf.

### 9.1 External executor model policy (Codex / Copilot), pricing, and CLI usage

Full detail lives in `references/external-executors.md` (Japanese) — Codex's Sol/Terra/Luna + effort policy and class assignment, Copilot's model catalog and `light`/`standard`-class candidates, exact CLI usage recipes (one-shot and session-continuation for retry rounds), and official per-token pricing for Codex/Copilot/Claude. Read it before dispatching to an external executor, and before second-guessing any model/effort choice in the example config.

In brief, as of this plugin's own validation (full reasoning and every round-by-round result: `references/poc-findings.md`):

- **Codex `standard`** → `gpt-5.6-luna` at **`effort: medium`** (not `high` — this plugin's own PoC found `high` produced a real bug on a realistic task that `medium` did not, cheaper and faster besides). **`deep`** → `gpt-5.6-sol`/`xhigh`. **`review`** → `gpt-5.6-sol`/`low` (this is the `class_policy` entry Codex uses when the `priority.independent-review` list dispatches to it). `gpt-5.6-terra` is not in the default policy but hasn't been shown to be dominated either — worth reconsidering for `independent-review` if cost is a concern.
- **Copilot `light`/`standard`** → `gpt-5.6-luna` at `effort: medium`, the fastest/cheapest validated candidate — luna leads the `priority` list for both the `light` and `standard` classes (see `examples/orchestra.yaml`); `kimi-k2.7-code` and the newly-resolved `mai-code-1-flash-picker` are viable alternatives (see the reference doc for what's been observed about each).
- **Long-context caveat:** Luna has measurably weak long-context recall. Escalate a nominally `light`/`standard`-class task to `deep` if it requires deep traversal of a large repository — the `long_context_escalation` field (`{ class: deep }`) in the example config documents this trigger.
- Across 6 rounds of escalating task difficulty, no accuracy differentiation was observed until task size crossed into genuine multi-file, multi-language feature territory — at which point every cheap tier tested eventually showed at least one real, narrow defect. Treat the adversarial review stage as mandatory beyond a trivially small change, regardless of which model/provider/effort level is implementing the work — this holds for external executors exactly as much as for Claude's own tiers.

## 2. Telemetry (opt-in, anonymized)



Orchestra can optionally emit a small, anonymized telemetry stream so the maintainer can see how the pipeline behaves in practice — class usage, executor fallback rates, dispatch outcomes — without ever seeing what any of it was about.

**Default off.** Telemetry is opt-in via `telemetry.enabled: true` in `orchestra.yaml` (see `examples/orchestra.yaml`). The resolved value surfaces at `doctor`'s `config.values.telemetry.enabled`, same as any other config key (section 9) — check that field rather than re-reading the YAML layers yourself. When disabled (the default, and the as-shipped state), nothing is written, and every `telemetry` subcommand that would record data is a silent no-op. To toggle without hand-editing YAML, use `agent-exec telemetry enable [--scope user|project|local]` or `agent-exec telemetry disable [--scope user|project|local]` — these perform a comment-preserving surgical edit of the scope's orchestra.yaml (creating a stub if absent) and require no manual YAML work.

**Crash-dump-style, allowlist-enforced.** Redaction is not a matter of callers behaving — `agent-exec` itself enforces an ALLOWLIST of field names and enumerated values; only enumerated categorical strings and non-negative integers can ever be stored, and `schema_version`/`ts`/`os` are stamped by `agent-exec` itself, never supplied by the caller. It is structurally impossible to record prompts, task text, file names, paths, task ids, `summary` strings, code, or error-message text: no field accepts free text, and enum fields are checked by exact match, so free text placed in an enum field is silently dropped rather than stored.

Allowed fields:
- `event`: `run_summary` | `dispatch`
- `lane`: `express` | `orchestrated`
- `orchestra_version`: semver
- `executor`: `claude` | `copilot` | `codex`
- `cls`: `light` | `standard` | `deep` | `review`
- `status`: `ok` | `unavailable`
- `reason`: `quota` | `rate-limit` | `credits` | `auth` | `nonzero-exit` | `error`
- `resumed`: boolean
- `run_summary`-only numerics: `task_count`, `pass`, `fail`, `exhausted`, `fallbacks`
- `run_summary`-only histograms (dict): `classes`, `rounds`, `executors_used`, `external_enabled`
- `usage`: integers-only counts from `input_tokens`, `output_tokens`, `cached_input_tokens`, `aiu_nano`, `premium_requests`, `api_duration_ms`, and `session_duration_ms`

Telemetry records use `schema_version: 2`. The no-free-text guarantee is unchanged:
the usage allowlist is flat and integers-only, so it cannot carry prompts, task
text, paths, or other free text.

**Two emission sources:**
- **AUTO — per dispatch.** `agent-exec run ... --capture` self-logs one `dispatch` record after printing its result (status/reason, and `cls` if `--cls` was passed to tag the dispatch's capability class). This is LLM-independent — it happens inside `agent-exec` regardless of who called it, and is a no-op when telemetry is disabled.
- **RUN SUMMARY — once per orchestration.** At run end, if telemetry is enabled (visible via `doctor`'s `config.values.telemetry.enabled`), the instructor emits exactly one `run_summary` record through a cheap **haiku relay agent** that calls `agent-exec telemetry record --json '...'` — never the instructor directly. This keeps the instructor's own context clean, and `record` only accepts categorical/numeric fields regardless of who calls it. Skip silently when disabled.

Example `run_summary` payload — categorical/numeric fields only, no ids or free text:

```json
{
  "event": "run_summary",
  "lane": "orchestrated",
  "orchestra_version": "0.11.0",
  "task_count": 3,
  "pass": 2,
  "fail": 1,
  "exhausted": 1,
  "fallbacks": 1,
  "classes": { "light": 2, "standard": 1 },
  "rounds": { "1": 2, "3": 1 },
  "executors_used": { "claude": 2, "copilot": 1 },
  "external_enabled": { "copilot": 1 }
}
```

**Storage and CLI.** Records accumulate under `telemetry.dir` (default `~/.claude/orchestra/telemetry`, see `examples/orchestra.yaml`). CLI surface:

- **`agent-exec telemetry record (--json STR | --file F)`** — appends ONE sanitized record; no-op + exit 0 when telemetry is disabled; never echoes the record's content to stdout.
- **`agent-exec telemetry show [--json]`** — inspect what's stored.
- **`agent-exec telemetry archive [--out FILE]`** — bundle stored records into a `.tar.gz`.
- **`agent-exec telemetry clear`** — delete stored records.
- **`agent-exec usage [--since <N>m|<N>h|<N>d|<ISO8601>] [--run <id>[,<id>...]] [--session <id>[,<id>...]] [--list-runs] [--source claude,codex,copilot] [--all-projects] [--json|--text]`** — read-only aggregation of Claude transcripts, Codex rollouts, and Copilot ledger/telemetry records. Use `--run` as the deterministic scope for a workflow run; it applies no time filtering and accepts a comma-separated list. `--session` is the deterministic session scope, also without time filtering, and accepts a comma-separated list. `--run` and `--session` may be combined as a de-duplicated union. An empty list element is a usage error (exit 2); unknown run or session ids contribute nothing and are not errors. `--list-runs` prints discoverable run ids and exits 0, ignoring `--since`; JSON is `{"runs":[{"run_id":str,"files":int,"first_ts":ISO,"last_ts":ISO}, ...]}`, sorted by `last_ts` ascending, while text prints one run per line. `--since` retains its 24-hour default for windowed measurement when no deterministic id is available. `--since` cannot be combined with `--run` or `--session`; `--list-runs` cannot be combined with `--run`, `--session`, or `--since`; each violation exits 2 with a message on stderr and nothing on stdout. Under a run or session scope, the JSON report has a top-level `scope` object: `{"kind":"window","since":<ISO>,"until":<ISO>}`, `{"kind":"run","run_ids":[...]}`, `{"kind":"session","session_ids":[...]}`, or `{"kind":"run+session","run_ids":[...],"session_ids":[...]}`. In text output the scope is the first line. The top-level `since`/`now` keys appear **only** under the window scope: a deterministic scope applies no time filter, so reporting one would invite a consumer to read the numbers as filtered. A source that cannot be attributed reports `{"attributable":false,"reason":<short fixed string>}` in place of its totals, or `<source>: not attributable (<reason>)` in text. Claude usage defaults to the current project; Copilot telemetry usage requires telemetry to be enabled, while ledger usage is independent of that setting.

## 3. Run ledger

The ledger is a default-on, local-only store, separate from telemetry and never uploaded. `ledger` participates in the same four-layer deep merge as `telemetry`: `enabled` defaults to `true`, `dir` to `"~/.claude/orchestra/runs"`, and `retention_days` to `30`; `retention_days` must be a positive integer or it falls back to `30`. `ledger.enabled: false` stops all ledger writes, but does not prevent reading existing data, and either setting is independent of `telemetry.enabled`.

Each session is a directory, `<ledger-dir>/<session-id>/`, where the session id comes only from the `CLAUDE_CODE_SESSION_ID` environment variable and must match `^[A-Za-z0-9_.-]{1,128}$` (with `..` rejected). A dispatch with `--run-id <id>` writes to `<ledger-dir>/<session-id>/<run-id>.jsonl`; one without it writes to `<ledger-dir>/<session-id>/no.run.jsonl`. Run ids must match `^[A-Za-z0-9_-]{1,64}$` and still exit 2 when invalid. The run id is carried by the path, not by a record field. Ledger lines retain the existing allowlist — `ts`, `executor`, `cls`, `model`, `status`, the integer usage counters, delegated Codex `corr`, and prompt-path fingerprints — and appends are best-effort and never alter dispatch behavior. Flat pre-v0.20.0 `<ledger-dir>/<run-id>.jsonl` files are inert history, readable only through `ledger show`, never through `--run` or `--session`, and are not migrated. A v0.19.0 session file and a v0.18.0 run file cannot be told apart, so continuing to read them is the one remaining way a session total could be reported as a run total.

`agent-exec usage --run <id>` reads every `<id>.jsonl` inside every session directory, de-duplicating by resolved path; flat legacy files are inert. A session directory can therefore never satisfy a run lookup. `--session <id>` reads every JSONL file directly inside that session directory and never reads legacy flat files. Under session scope, Copilot and Codex report normal ledger totals when lines exist and add a `runs` breakdown keyed by each run id plus literal `no-run`; delegated Codex lines resolve through the correlation id and retain the `delegated`/`measured` counters. Without matching lines they report `attributable: false` with reason `no matching run ledger data`; `--since` behavior is unchanged. A session figure is a different quantity from a run figure: a dispatch with no run id belongs to its session and to no run, so a session total must never be used as the cost of a single run. On the Claude side, a run lookup requires the literal `workflows/<run-id>` path segment.

The ledger CLI is **`agent-exec ledger show [--session <id>] [--run <id>] [--json|--text]`**, **`agent-exec ledger archive [--json|--text]`**, and **`agent-exec ledger clear [--yes] [--json|--text]`**. `show` reports ledger contents as counts and summed integer fields per executor; without a selector it distinguishes session directories (including their run files) from legacy flat run files. `archive` and `clear` cover both layouts. Missing files or directories are empty, not errors; unknown options or malformed selectors exit 2, and an empty ledger exits 0.

After a successful append, at most once per process, old `*.jsonl` files directly in the ledger directory and directly inside session directories are deleted. Cleanup is best-effort, never removes a session directory, and failures never affect dispatch.

No workflow run id is inferred from the environment, a transcript path, or directory mtimes. A run id reaches agent-exec only through `--run-id`, because inference makes a cost number untrustworthy.
