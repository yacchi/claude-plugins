# Configuration reference (orchestra `run`)


Load this file only when a configuration question actually arises: what a key means, how the four layers merge, why `route` dropped a candidate, what `enforcement.light_class` does, or what telemetry and the run ledger record. For normal orchestration you do not need it — one `agent-exec doctor --json` call already returns the resolved config, and `agent-exec route`/`dispatch` already execute the priority walk.

## 1. Configuration file and external executors


At the start of every orchestration, the instructor resolves configuration from up to four layers, merged in this order (later layers win):

1. **Defaults**: `tiers` — light: haiku/medium, standard: haiku/high, deep: opus/medium, review: sonnet/medium, planner: opus/max as `{model, effort}` (the planner is spawned directly as `orchestra:orchestra-planner`, never routed; the rest remain the Claude-side fallback models and efforts `agent-exec route` resolves to). `external_executors.pi` now ships **`enabled: true`** out of the box, with its `class_policy` (pi: `openai-codex/gpt-5.6-luna`/medium for `light`, `openai-codex/gpt-5.6-luna`/high for `standard`, `openai-codex/gpt-6.1-sol`/high `deep`, `openai-codex/gpt-6.1-sol`/medium `independent-review`) and a built-in `priority` (`light: [claude, pi]` (investigation the same), `standard: [claude, pi]`, `deep: [claude, pi]`, `review: [claude]`, `independent-review: [pi]`), plus `enforcement.light_class: off` and `ledger.enabled: true`, `ledger.dir: "~/.claude/orchestra/runs"`, `ledger.retention_days: 30`. Shipping external executors enabled by default is safe because `agent-exec route` gates every candidate on actual availability (below) — on a machine without pi installed, every call still resolves to `claude`.
2. **User**: `~/.claude/orchestra.yaml` (or `.yml`), if present.
3. **Project**: `.claude/orchestra.yaml` (or `.yml`), if present — checked into git, shared with the team.
4. **Project-local**: `.claude/orchestra.local.yaml` (or `.yml`), if present — this developer's personal override for this one project. Mirrors Claude Code's own `settings.json` / `settings.local.json` split; never commit this file (see `setup` SKILL.md §1).

**Where the project layers are looked up (the "project base").** Layers 3 and 4 are not read from the process cwd blindly: `agent-exec` walks upward from the cwd to the git toplevel (inclusive) and uses the nearest directory holding any of `.claude/orchestra.yaml|.yml` or `.claude/orchestra.local.yaml|.yml`. With none found, or outside a git repo, the cwd itself is the base (the old behavior). So a session or relay whose cwd drifted into a subdirectory (`.overhaul/`, `web/`) still reads the repository's project config — before this, such a relay silently skipped `.claude/orchestra.local.yaml` and routed to an executor that file had disabled. The user layer is unaffected.

This is a **deep merge**, not a first-found-wins lookup: object/mapping keys are merged recursively key-by-key, so a project file only needs to state the keys it actually wants to change — e.g. a project override of just `external_executors.pi.enabled: true` inherits everything else (pi's `class_policy`, etc.) from the user config or defaults. Scalars and arrays are replaced wholesale by the more specific layer, not concatenated or element-merged. An explicit `null`/`~` is a value, not "reset to default" — omit the key entirely to inherit.

**Partial overrides inherit the rest:** a config that overrides only, say, `priority.deep` still takes every other class's `priority` from the built-in defaults, including when those defaults change in an upgrade. To freeze a class's order, state its list explicitly; `external_executors.pi.enabled: false` opts out of external executors altogether.

The format is YAML, not JSON, specifically so the file can carry comments (JSON can't). `.claude/orchestra.json` / `~/.claude/orchestra.json` (the pre-YAML format) are no longer read — use the `setup` skill (`orchestra:setup`) to convert an old one.

Instead of merging these four layers in-context, the instructor can obtain the already-resolved configuration deterministically via **`agent-exec config [--json]`** — it deep-merges the same four layers with the same precedence/merge rules described above and additionally reports, per `external_executors.<name>` with `enabled: true` and `dispatch: cli`, whether its executable resolves on `PATH` (`"available": true/false`, or `null` if the name has no built-in profile). This keeps the merge logic out of the instructor's own context. **Prefer a single startup call, though:** `agent-exec doctor --json` (needed anyway for the `dispatch: cli` pre-flight below) now embeds this exact resolved config under `config.values` alongside its readiness report, so one `doctor` call yields both the `ready.<executor>.ok` verdicts *and* the resolved `tiers` / `external_executors` / `priority` — a standalone `config` call is only worth it when you want the config and nothing else. Likewise, a `dispatch: cli` invocation that must bypass routing may use direct mode, **`agent-exec dispatch --executor pi --model M --effort E --workdir W --prompt-file F [--prompt-file G ...] [--class C] [--run-id ID] [--resume SID] [--isolate never|auto|always] [--task T]`**, as a normalized entry point (isolation defaults to `never`); a supplied `--run-id ID` tags the ledger line with that run. It runs the executor as a subprocess and prints one normalized `{ status, answer, session_id, reason, exit_code }` JSON object to stdout — see `references/external-executors.md` §5 for details.

`--prompt-file` is repeatable on `dispatch` (routed and direct mode): files are read in order and concatenated with exactly one blank line between consecutive files. One file is backward-compatible; a missing or unreadable file is a usage error (exit 2), names the path on stderr, and dispatches nothing.

**`agent-exec route` / `agent-exec dispatch`: the walk itself is code, not instructor judgment.** `agent-exec route --class <light|standard|deep|review|independent-review> [--archetype default|investigation] [--exhausted a,b] [--json|--text]` runs the entire `priority` walk described below in one call: it deep-merges the four config layers, then hard-gates each candidate on reality — dropping it if `enabled: false`, its binary/agent doesn't resolve, `doctor`'s own `ready.<x>.ok` is false, it has no `class_policy` entry for the class, or the caller listed it in `--exhausted` — and returns the surviving top candidate (`executor`, `dispatch`, `model`, `effort`, `agent_type`) plus the full `candidates`/`remaining`/`skipped` trail. The pre-flight `doctor` check described above is folded into this call; it is no longer a separate step the instructor performs before building a `priority` candidate list. `agent-exec dispatch --class <cls> --prompt-file F [--prompt-file G ...] --workdir W [...] [--run-id ID]` goes one step further: it calls `route` internally, and if the winning candidate is a `dispatch: cli` executor it also runs it, returning `{status: 'ok'|'unavailable', answer, session_id, reason, exit_code, executor, model, effort, route}`; if the winner is Claude or a `dispatch: agent` executor, it returns `{status: 'delegate', executor, model, effort, agent_type, route}` instead, since only the instructor's own `agent()`/Agent-tool call can spawn a Claude or plugin subagent. With nothing viable it returns `{status: 'unroutable', route}`. This is exactly what §5's `dispatchClass()` helper wraps: **the instructor never decides which executor runs a `light`/`standard` task — it asks, via one relay agent, and branches on the answer.** This availability gate is exactly what makes shipping external executors `enabled: true` by default safe (point 1 above).


A copy of the schema, fully commented, ships with this plugin at `examples/orchestra.yaml`. Full shape:

```yaml
tiers:
  # implementation capability classes (cheapest/fastest -> hardest). These
  # are the CLAUDE-side fallback models `agent-exec route` resolves to when
  # no external executor is enabled/ready for the class - see `priority`
  # below for what actually gets tried first.
  light:             # fast, mechanical, fully-specified work
    model: haiku
    effort: medium
  standard:          # normal implementation with modest design latitude
    model: haiku
    effort: high
  deep:              # design-sensitive / hard problems needing real judgment
    model: opus
    effort: medium
  # verification role
  review:            # mandatory same-run adversarial review of the work
    model: sonnet
    effort: medium
  # `independent-review` has no built-in Claude model on purpose - it exists to
  # add a DIFFERENT provider's eyes; see `priority.independent-review` below.

external_executors:
  pi:
    enabled: true  # ships true by default; gated on binary+doctor readiness, see §9 below
    dispatch: cli
    # No `command` template: agent-exec builds the whole invocation itself
    # (PI_SKIP_VERSION_CHECK=1 pi -p --mode json --no-approve --no-extensions
    # --no-skills --no-prompt-templates --no-themes --model <provider/id>
    # --thinking <effort> [--session <id>], prompt on stdin, cwd = worktree).
    # `model` is always `provider/id`. `openai-codex/*` runs on the ChatGPT
    # subscription; pi's `cost` there is a list-price estimate, not a charge. See external-executors.md.
    # network_domains: unset = derived from the model's provider prefix
    # (openai-codex/ -> chatgpt.com, auth.openai.com; see sandbox.network).
    classes:
      - light
      - standard
      - deep
      - independent-review
    class_policy:
      light:
        model: openai-codex/gpt-5.6-luna
        effort: medium
      standard:
        model: openai-codex/gpt-5.6-luna
        effort: medium
      deep:
        model: openai-codex/gpt-6.1-sol
        effort: high
      independent-review:
        model: openai-codex/gpt-6.1-sol
        effort: medium

# Persisted, time-decaying executor cooldown. Entries are written automatically
# when `agent-exec dispatch` parses
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
    sandbox: 0

# Usage-aware routing: before walking the priority list, route/dispatch read
# how much of the subscription behind each candidate's 5-hour and 7-day
# rate-limit windows is used (Claude plan for claude, ChatGPT plan for pi on
# openai-codex/... models; other providers have no data). A candidate at or
# above a threshold (percent) is demoted like a cooldown: soft, ignored when
# every candidate is over. Lookups are cached for cache_seconds in the state
# dir; every failure is fail-open. --no-usage skips it for one call;
# ORCHESTRA_USAGE_ROUTING=0 turns it off.
usage_routing:
  enabled: true
  thresholds:
    five_hour: 80
    seven_day: 90
  cache_seconds: 120

# OS sandbox around CLI executor children. Lists are UNIONED across all config
# layers (unlike ordinary arrays), so a project cannot silently remove a user
# layer's protection or writable exception.
sandbox:
  mode: auto          # auto | required | off
  allow_write: []     # extra writable paths
  deny_read: []       # extra unreadable paths
  network:            # outbound allowlist via srt: herdr host (§8, v0.50.0) and CLI executors
    mode: allowlist   # allowlist | off
    ecosystem: true   # package-registry hosts from the tree's tracked files
    allowed_domains: [api.anthropic.com]   # unioned across layers; wildcards and :port allowed
    denied_domains: []                     # unioned across layers
    quiet_domains: [mcp-proxy.anthropic.com, downloads.claude.ai, ...]  # blocked, audited, never surfaced

# Runaway protection for CLI executors.
watchdog:
  enabled: true
  idle_seconds: 600
  tool_idle_seconds: 900
  repeat_limit: 3
  wall_seconds:
    light: 1200
    standard: 2400
    deep: 5400
    independent-review: 2400

The learned sandbox grants are stored per machine at `$XDG_STATE_HOME/claude-orchestra/sandbox-learned.json`, or `~/.local/state/claude-orchestra/sandbox-learned.json` when `XDG_STATE_HOME` is unset or non-absolute. `agent-exec sandbox allow <path> [--reason TEXT]` adds a user grant (the reason is stored on it); harmless cache denials can be learned automatically. Decisions made with `agent-exec approvals decide` (wave runs) store their grants with `source: instructor` or `user` and are appended, applied or refused, to `<orchestra state dir>/audit/decisions.jsonl` (`{ts, state, item, kind, subject, decision, by, reason, packages, applied}`); declined hosts of a wave are kept in `<wave state dir>/approvals.json` so they are not offered again. Use `agent-exec sandbox list|allow|forget|probe` to inspect or change them. `cooldown.seconds.sandbox` defaults to `0`: a required-sandbox refusal never cools down an executor.

**`sandbox.network` (v0.50.0; CLI executors since this release).** Applies to the herdr host (§8) and to the CLI executor pi (`references/external-executors.md` §2a). `allowed_domains` and `denied_domains` are unioned across layers like `allow_write`, starting from the default `api.anthropic.com`, so a project adds e.g. `registry.npmjs.org` and can never drop a domain a lower layer allowed. `mode: off` keeps the network unrestricted (the pre-0.50 behavior). Domains approved at run time are stored with `agent-exec sandbox allow-domain <host-or-*.wildcard>` (`forget-domain <host>` removes one) in the same learned-grants file under a separate `domains` key, so they share its tamper protection; `*`, a bare TLD wildcard such as `*.com`, IP literals and `localhost` are refused. `quiet_domains` (also unioned) lists background traffic of Claude Code and herdr that no task needs — the MCP connector proxy, auto-update, telemetry, herdr's update check: those hosts are blocked and written to the audit log like any other, but never appear in `network_denials`, so nobody is invited to approve them. `agent-exec sandbox allow-domain <host> --reason TEXT` stores why a domain was approved; every domain grant records its `source` (`user` for the command) and `added` time, and `agent-exec sandbox list` prints them. Go tools such as `gh` hit the same wall inside any srt sandbox on macOS (fail closed).

**Registry presets (`ecosystem`, default `true`).** Every srt settings file (herdr host and CLI runs) also allows the package registries the tree needs, decided from its **tracked** files only (`git ls-files`, matched by basename; untracked and ignored files do not count): `package.json`/`package-lock.json`/`pnpm-lock.yaml`/`yarn.lock`/`bun.lock`/`bun.lockb` → `registry.npmjs.org` (plus `registry.yarnpkg.com` for `yarn.lock`); `pyproject.toml`/`requirements*.txt`/`uv.lock`/`Pipfile`/`setup.py` → `pypi.org`, `files.pythonhosted.org`; `go.mod` → `proxy.golang.org`, `sum.golang.org`; `Cargo.toml` → `index.crates.io`, `static.crates.io`, `crates.io`; `Gemfile` → `rubygems.org`, `index.rubygems.org`; `pom.xml`/`build.gradle(.kts)`/`settings.gradle(.kts)` → `repo.maven.apache.org`, `repo1.maven.org`, `plugins.gradle.org`, `services.gradle.org`. A tracked `.npmrc` at the repository root adds the host of each `registry=` / `@scope:registry=` URL (host only; IPs, `localhost` and anything else `allow-domain` would refuse are skipped). No preset allows `github.com` or `raw.githubusercontent.com`. `ecosystem: false` turns the presets off.

**Per-executor API hosts (`external_executors.<name>.network_domains`).** Added only to that executor's srt settings, never to another executor's or the herdr host's; unioned across layers like `allowed_domains`. For pi, an unset list is derived from the model's provider prefix at dispatch time: `openai-codex/` → `chatgpt.com`, `auth.openai.com`; `openai/` → `api.openai.com`; `anthropic/` → `api.anthropic.com`; `github-copilot/` → `api.githubcopilot.com`, `*.githubcopilot.com`, `api.github.com`; `google/` → `generativelanguage.googleapis.com`; any other prefix → none (its blocked hosts surface as `network_denials`). `agent-exec doctor` shows, per CLI executor, whether its network would be restricted and its domains, and the preset domains for the current directory.

# pi ships `enabled: true` above, so this list actually prefers it
# when ready - `agent-exec route`/`dispatch` execute the walk, not the
# instructor (see the route/dispatch paragraph above).
priority:
  light:
    investigation: [claude, pi]
    default: [claude, pi]
  standard:
    default: [claude, pi]
  deep:
    default: [claude, pi]
  review:
    default: [claude]
  independent-review:
    default: [pi]

# Local executor usage ledger. It is default-on, local-only, and independent
# of telemetry. It records only when Claude Code supplies a session id.
ledger:
  enabled: true
  dir: "~/.claude/orchestra/runs"
  retention_days: 30

# Extra gitignored directory names (or relative paths, matched by basename)
# `isolate create` copies into a fresh worktree, on top of the built-in list
# (node_modules, .venv, venv, .tox, vendor, target, .next, .nuxt,
# .svelte-kit, .gradle, Pods, .bundle, .dart_tool) - see references/isolation.md.
isolation:
  carry_extra: []
  carry_files:
    - .claude/settings.local.json
    - .claude/orchestra.local.yaml
    - CLAUDE.local.md
  # Dependency/cache directories kept out of every task's change set even
  # when the repo does not ignore them: never intent-added, diffed, patched,
  # committed by integrate, or copied into a worker tree from your checkout.
  # An entry without an inner "/" matches that directory name at any depth;
  # one with an inner "/" (packages/a/.cache/) is repo-root relative. UNIONED
  # across layers (a layer adds entries, never drops a default); entries are
  # normalized ("./x" and "/x" become "x/", entries with ".." are dropped).
  exclude_paths: [.pnpm-store/, node_modules/, .venv/, __pycache__/, .turbo/, .next/, .gradle/, .terraform/]
  # Escape hatch, also unioned: every entry here is removed from exclude_paths
  # (e.g. keep_paths: [node_modules/] for a repo that vendors node_modules).
  keep_paths: []
  # A task adding more new files than this is refused by `isolate integrate`
  # (task status "refused" with a reason, exit 7; the other tasks still land)
  # and becomes an `oversized` need in a wave run. 0 disables the limit.
  max_added_files: 500
  # pnpm repos only. `"auto"` (the default) turns it on where the pnpm that
  # runs in the project is 12 or later (corepack and packageManager pins
  # honoured, probed once per process); `true` and `false` force it. Set it
  # per repository in the repo's .claude/orchestra.yaml. A fresh tree whose
  # root holds pnpm-lock.yaml gets `pnpm install --frozen-lockfile --offline`
  # against pnpm's global virtual store instead of a clone of node_modules
  # (online retry, then the clone as fallback), and every process orchestra
  # starts in such a tree carries pnpm_config_enable_global_virtual_store=true.
  # Caveats (phantom deps, CI, shared links): references/isolation.md "pnpm
  # global virtual store".
  pnpm_global_virtual_store: "auto"

# Nudges and guards, never hard walls - see the `enforcement` paragraphs below
# for the escape hatches. NOTE: quote "off" - YAML 1.1 parses a bareword `off`
# as boolean False, not the string "off".
enforcement:
  light_class: "off"    # opt-in: nudge a generic Haiku spawn toward dispatch
  worker_vcs: "block"   # default ON: no destructive VCS from a subagent
  worker_tree: "block"  # default ON: no worker writes into the main tree
  worktree_lease: "block"      # default ON: one writing subagent per linked worktree
  worktree_lease_window: 120   # seconds a silent holder keeps its lease
  turn_edits: 8         # distinct main-thread files edited in one turn before asking to re-route
```

Run ledger files for tagged runs use an atomically allocated, per-session
ordinal prefix such as `001-wf_153ae50e-9bf.jsonl`; this orders a session's
runs by execution, and a bare ordinal (`3`, `03`, or `003`) addresses that run
only within the current `CLAUDE_CODE_SESSION_ID` session. The harness run id
stays in the filename because the Claude source resolves transcripts through
the harness's own `workflows/<run-id>` directory, which a locally minted id
could not address. Older un-prefixed files are still read and are not migrated.

To set this up interactively (detect whether pi is actually available in this environment, choose project vs user scope, edit the file in place without clobbering existing comments), use the `setup` skill instead of hand-editing — see its own SKILL.md.

**`tiers`** overrides the model-class defaults of section 3. Each entry is either a model alias (or full model ID) string, which leaves the effort unset, or an object `{model, effort}` where `effort` is one of `low`, `medium`, `high`, `xhigh`, `max`; any other key or effort value is a config error. `planner` is the exception: it is never route-resolved — the instructor spawns `orchestra:orchestra-planner` directly with this `{model, effort}` (default `{model: opus, effort: max}`), and no `priority` or `external_executors` entry applies to it. The other entries are used whenever this skill — or `agent-exec route` — resolves a class/role to a Claude model: the `claude` candidate in `priority`, or any class/role with no external executor configured for it. When the winner is `claude`, `agent-exec route` returns `model`, `effort` and `agent_type` for it (`orchestra:orchestra-light` for `light` and `standard`, `orchestra:orchestra-deep` for `deep`, `orchestra:orchestra-review` for `review`, null for `independent-review`).

**`external_executors`** declares non-Claude executors (pi, etc.) that may be woven into the pipeline. Only entries with `"enabled": true` are used. Two dispatch mechanisms:

- **`dispatch: "agent"`** — the executor is an installed plugin subagent. It is a supported override for any executor entry (`dispatch: agent` plus an `agent_type`); `dispatch: "cli"` (below) is the default. Pass `agent_type`'s value as `agentType` in Workflow `agent()` calls, or as `subagent_type` in the Agent tool. **The route's `model`/`effort` must be appended to the prompt text as `--model <m> --effort <e>`, not passed as `agent()` options** — those options select a *Claude* tier and are meaningless to a plugin subagent, while such an agent leaves the executor's model and reasoning effort unset unless the request names them. Drop them and the run silently falls back to that executor's own defaults, typically a different tier entirely. See `dispatchClass()`'s `routingFlags()` helper in SKILL.md §5. If the agent name doesn't resolve in this environment, fall back to the normal model tier for that class/role.
- **`dispatch: "cli"`** — the executor is a non-interactive CLI (pi). The instructor prepares a token with `agent-exec dispatch prepare --class <cls> --prompt-file F --workdir W [--run-id R] [--isolate ...] [--task ID]` (every routing/accounting flag is fixed here, at prepare time; the prompt file(s) named need not exist yet — only `--token` dispatch time requires them to). `dispatchClass()` (§5) gives only that token to relay hops — one start hop running `agent-exec dispatch --token ... --capture --detach` (plus `--exhausted`/`--no-resume` if needed), then wait hops running `agent-exec dispatch wait --token ... --summary` until the status is final; each hop runs one command and returns its last stdout line, and the script does the looping (see "Detached dispatch" below). The token form accepts only `--capture`/`--exhausted`/`--no-resume` (plus `--detach`, and `--summary` on a synchronous `--capture` call); combining it with `--run-id`, `--class`, `--workdir` or any other prepare-time flag exits 2 with `--token conflicts with <flag>`. `dispatch` calls `route` internally, so the `ready.<executor>.ok` gate has already been applied before the CLI ever runs.
  **Not a security boundary (M2):** `pi -p` and the other CLI executors are not reliably confinable by its own tool-permission flags — excluding a named tool (e.g. `--excluded-tools=bash`) has been observed to be routed around via the `task` tool rather than actually blocked. `--allow-tool`/`--deny-tool`/`--excluded-tools` are defense-in-depth at best, never treat them as a containment guarantee; real containment needs an external OS sandbox (container, `sandbox-exec`, restricted user, network egress control) or a disposable worktree, which this plugin does not implement — treat any `dispatch: cli` task as an autonomous, directory-scoped agent, and prefer a disposable workdir (a scratch clone or worktree it can freely write/execute in) over the user's primary working tree when the task doesn't need to persist there. A `--deny-tool` list may still be set by the user as an optional, explicitly-advisory knob, but document it to them as narrowing the *named* surface only, not as a boundary.
  **Per-task session store and auto-resume (all `dispatch: cli` executors with a session concept — pi):** `~/.claude/orchestra/sessions/<executor>-<task>.json` (dir mode 0700, file mode 0600) holds `{"executor","task","session_id","workdir","updated"}` for the last dispatch of that `(executor, task)` pair. `agent-exec dispatch --task <id>` with no explicit `--resume` looks it up: a hit resumes (`resumed: true`), a miss starts fresh (`resumed: false` — never an error). An explicit `--resume SID` always wins; `--no-resume` opts out of the lookup for that call only. A stored record whose `workdir` differs from the current `--workdir` is treated as a miss (a reused task id in another tree never resumes a foreign session) and is overwritten on success. **This store has no dedicated retention setting** — it is swept by the same `ledger.retention_days` the token/ledger sweep already uses (the retention sweep on `dispatch`), not a second knob. There is no command to print a stored record; the dispatch result's `resumed` / `session_id` fields show what was used.
  **Config root in the token.** `dispatch prepare` resolves the project base (above) and stores it in the token as `"config_root"`; `dispatch --token`, synchronous or the detached child, resolves config from that base whatever its own cwd, so a token routes the same way wherever the relay happens to run. Tokens minted before this field existed keep the cwd behavior. `prepare`'s output also names the layers it merged — `config_layers` (absolute paths of the layer files that exist and were merged) in JSON mode, a `config: <comma-separated paths or (defaults only)>` line in text mode — so a wrong project config shows up at prepare time instead of as a surprise `unavailable`.
  **Detached dispatch (`--detach` / `dispatch wait`).** `dispatch --token T ... --capture --detach` starts the dispatch as a background child and returns at once with `{"status":"detached",...}` (re-issued for the same token: `running` while the child lives, `done` after it exits). `agent-exec dispatch wait --token T [--max-wait N] [--summary]` blocks up to N seconds (default 560, under a common 600s foreground-call limit):
  - still running → `{"status":"running","token":T,"pid":P,"waited_s":<int>,"wait_calls":<int>}`, exit 0;
  - child exited → its final JSON (or `{"status":"relay-failed","token":T,"raw":...}` when the output was unparsable) is persisted at `<tokens>/detach/<T>.result.json`, and the state file `<tokens>/detach/<T>.json` is kept with `"finished": <iso8601>` and `"collected": true` added once a `wait` has returned it. **Repeated `wait` calls return the identical result** — a wait is always safe to repeat, from any session or cwd. A non-empty `answer` is also written in full to `<tokens>/detach/<T>.answer.txt`, and the result gains `"answer_path"` (absolute);
  - `--summary` prints exactly one line of compact JSON: `answer` cut to its first 400 Unicode code points (`"answer_truncated": true` when cut), `answer_path` whenever the answer was non-empty, every other key kept. This is the form relays run: a one-line result is what a cheap relay copies reliably. A synchronous `dispatch --token T ... --capture --summary` (no `--detach`) gives the same shape and writes the same answer file;
  - neither state nor result file for a well-formed token → `{"status":"unknown-token","token":T}`, exit 0 (a malformed token is still a usage error, exit 2);
  - The retention sweep deletes `detach/<T>.*` older than the retention window once the pid is no longer alive. The detached-dispatch listing (what `agent-exec ui` shows) reports per token `result_present`, `collected`, `finished` and `wait_calls`, so a dispatch that finished but was never collected, or whose process died, is visible at a glance.

**`agent-exec write-file --path P [--expect-chars N]`** writes all of stdin (UTF-8, exactly as received) to `P` atomically — a temp file in the same directory plus `os.replace`, parent directories created. With `--expect-chars N` it first compares `N` with the content's Unicode code-point count after removing one trailing `"\n"` (what a heredoc adds); on mismatch nothing is written and it prints `{"status":"mismatch","path":<abs>,"chars":<got>,"expected":N}` with exit 1. Success prints one line `{"status":"ok","path":<abs>,"chars":<count>}`, exit 0; usage errors (missing `--path`, bad `N`, unknown flag) go to stderr with exit 2. It exists so a script can put text on disk through a relay that only pastes one command — §5's `writeCorrectionPacket()` — instead of asking a model to "write this text", which a writer once took as instructions to carry out.

**`classes`** controls where the executor is used:
- `"light"`: as a light-class implementer (in place of, or alongside, the Claude `light` tier).
- `"standard"`: as a standard-class implementer (in place of, or alongside, the Claude `standard` tier / `model: 'haiku'`).
- `"deep"`: as a design-latitude implementation class (in place of, or alongside, `orchestra-deep`/Opus) — only meaningful for an executor whose `class_policy` names a model strong enough for that class (pi's `deep` policy is `openai-codex/gpt-6.1-sol`/high — see the pi section of the reference doc).
- `"review"`: as the same-run adversarial review pass (in place of, or alongside, the Claude `review` tier / `orchestra-review`).
- `"independent-review"`: as a third-party review pass in addition to the Claude review — useful to avoid single-provider model bias. An independent review supplements `orchestra-review`; it does not replace the structured-verdict contract, so wrap its output into the same verdict shape. Because an external, CLI-backed reviewer may ignore the Workflow `schema:` option, force JSON in the prompt and normalize the reply with a tolerant parser — see `references/external-executors.md` §4 (`parseExternalVerdict`).

**`class_policy`** maps each class this executor participates in to a concrete `{ model, effort }` pair to pass to that executor. Without it, the executor runs on its own default model, which defeats the purpose of cost-tiering by external provider just as surely as an unpinned Claude `agent()` call does (rule #4, section 4). Treat this the same way: every external-executor class in `classes` should have a matching `class_policy` entry.

**`priority`** declares, per class/role (and, for `light`, per task archetype — `investigation` vs `default`), an ordered candidate list to try — this list is still the single source of truth for preference order. What changed is *who walks it*: **`agent-exec route`/`dispatch` execute the walk**, not the instructor. `route` tries candidates left-to-right and drops one only on a **reactive fallback signal** — the executor is *unavailable*: `enabled: false`, its binary/agent doesn't resolve, `doctor`'s `ready.<x>.ok` is false, it has no `class_policy` entry for the class, or the caller passed it in `--exhausted`. This is the crucial **unavailable-vs-failed** distinction: a task that runs but returns a wrong result is NOT a fallback signal — it stays on the same executor and goes through the normal review/retry loop; only a genuinely unavailable executor (rate-limit/usage-window cap, credit exhaustion, auth failure) makes `route` drop to the next candidate. Once `dispatch` reports an executor `unavailable` for a task, the instructor's one remaining manual job is to **carry that executor forward in `--exhausted` for every subsequent `route`/`dispatch` call in the same run** (§5's `dispatchClass()` does this automatically via its module-level `exhausted` Set) — this is sticky exhaustion, and it is the instructor's only bookkeeping in the whole selection process. `priority` **supersedes `classes` for ordering** when present for a class/role; `classes` remains the legacy fallback when `priority` is absent (`route`/`dispatch` apply this same fallback internally). Operational detail — per-executor unavailable signals and the priority-walk logic `route` implements — lives in `references/external-executors.md`; read it before second-guessing a fallback decision.

**`cooldown`** enables the persisted, time-decaying cross-run layer beneath the in-run `--exhausted`/sticky exhaustion `Set`. `seconds` is a mapping, so deep-merge combines it per key; unlike the `priority` lists, it is not replaced wholesale. Active entries are visible in `agent-exec doctor` (`cooldowns`) and the `agent-exec ui` dashboard, which can also clear them; `--no-cooldown` on `route`/`dispatch` bypasses them for one call.

**`usage_routing`** demotes a route candidate whose subscription is close to its rate limit. Before the walk, `agent-exec route` / `dispatch` (and so `wave run`) read the 5-hour and 7-day windows of the plans behind the candidates: the Claude subscription for `claude` (the Claude Code OAuth login: the macOS keychain item, else `.credentials.json` under `CLAUDE_CONFIG_DIR` or `~/.claude`) and the ChatGPT subscription for `pi` when its class model is `openai-codex/...` (pi's `openai-codex` login, else the Codex CLI's `auth.json`). Other providers (e.g. `github-copilot/...`) have no data and are never demoted. A candidate whose window is at or above `thresholds.five_hour` / `thresholds.seven_day` (percent) is skipped with `usage:<group>:<window>=<pct>%`, evaluated after the hard skips, alongside cooldowns; like cooldowns the demotion is soft: when no candidate survives, the walk is repeated ignoring both and the route carries `usage_bypassed: true`. The route JSON lists the demotions in `usage_applied` (`executor`, `group`, `window`, `used_percent`, `threshold`, `resets_at`). Lookups are cached for `cache_seconds` in `<state dir>/usage-limits.json` (failures too, so a dead endpoint costs one 5 s timeout per TTL); credentials are only read, never refreshed, and never logged or cached. Every failure (`no-token`, `expired`, `http-<code>`, `network`, `parse`) is fail-open: no data, no demotion. `agent-exec doctor` shows the current numbers under `usage_limits`; `--no-usage` on `route`/`dispatch` skips the lookup for one call, `ORCHESTRA_USAGE_ROUTING=0` disables it, and `ORCHESTRA_USAGE_SNAPSHOT=<file>` replaces the lookup with a fixture (tests).

**`enforcement.light_class`** (`"off"` default | `"block"` — quote the value; YAML 1.1 parses a bareword `off` as boolean `False`, not the string) is a **nudge with a guaranteed escape, not a hard wall**. When set to `"block"`, a `PreToolUse` hook (`hooks/guards/light-class.ts`) fires only when `agent-exec route --class light` reports a non-`claude` executor ready — i.e. only when there's genuinely something better to redirect to — and the call would spawn a **generic** Claude implementer: no `subagent_type` at all, or `subagent_type` in `general-purpose`/`claude`, at a Haiku-class model. **Naming any other specific `subagent_type` is itself the carve-out**: the Agent tool has no per-call tool-restriction parameters — its schema is only `description`/`isolation`/`model`/`prompt`/`run_in_background`/`subagent_type` — so a named agent is how tool access and a specialized system prompt actually get pinned, and an external executor can substitute for neither; `orchestra:orchestra-light` itself stays a deny target, since redirecting it to `agent-exec dispatch` is the whole point. Escape hatches:

1. **Hard cap: one deny per session, full stop.** Not a per-task or per-prompt cap — after the first nudge the hook goes inert for the rest of the session regardless of what's asked next. (A prompt-keyed fingerprint doesn't work here: this plugin's own retry convention appends new feedback text to the prompt every round (§5), which would re-deny every round of the same task — so the bound is session-wide instead.)
2. **Explicit escape marker.** `[orchestra:allow-claude: <reason>]` anywhere in the prompt/description allows immediately, no deny recorded — for a deliberate choice to stay on Claude.
3. **Named-subagent carve-out.** Any call naming a specific `subagent_type` other than `general-purpose`/`claude` is always allowed (see above) — `Explore`, `Plan`, `statusline-setup`, `claude-code-guide`, `orchestra-review`, and any other installed agent all qualify; only a generic/anonymous Haiku-class call (or `orchestra:orchestra-light` itself) is a deny candidate.
4. **Kill switch + fail-open.** `ORCHESTRA_ENFORCEMENT=off` short-circuits to allow, and every uncertainty (`agent-exec` missing, `config` failing/slow, unparseable stdin, anything unexpected) fails open — it never denies on doubt.

Ships **`"off"`** — turning it on is a deliberate opt-in via the `setup` skill or by hand-editing `orchestra.yaml`, not a default behavior change.

**`enforcement.worker_vcs`** (`"block"` default | `"off"` — quote the value) blocks a **subagent** from running destructive VCS commands (`git checkout -- <paths>`, `git checkout .`, `git restore` without `--staged`, `git reset --hard/--merge/--keep`, `git clean -f`, `git stash push`, `git worktree remove --force`) against a working tree that is not one of orchestra's own (`orchestra/*` branch). It ships **on**, unlike `light_class`, because it prevents data loss rather than steering a cost decision: transcripts recorded 35 such commands run by workers of every model tier, and three escalating generations of prose prohibition in the worker prompts did not stop them (`references/isolation.md` §0). Normalization is correspondingly inverted — an ambiguous value keeps the guard on; only an explicit `off` (string, or the bareword YAML loads as `False`) disables it. Scope and escapes:

1. **Main thread untouched.** Only calls carrying an `agent_id` (i.e. from inside a subagent) are candidates. An instructor resolving a rebase conflict with `git checkout --ours` is doing its job.
2. **A worker's own worktree is its own business — except the stash stack.** On an `orchestra/*` branch the command is allowed — there is nothing there but that worker's work. `git stash` push/pop/apply/drop/clear is the exception and is denied in every worktree: `refs/stash` is one stack shared by all worktrees of a repository, and two parallel workers that stashed four seconds apart each popped the other's entry, swapping their changes between worktrees. `stash list`/`show` stay allowed, and the denial points workers at `agent-exec shelf push`/`pop` — the same job with entries kept in the worktree's own git dir, so nothing is shared.
3. **The supervising layer is exempt.** `orchestra-delegate` owns snapshots, rollbacks, and merges by design.
4. **Not fooled by text.** Heredoc bodies and quoted strings are stripped before matching, so writing the prohibition itself into a worker prompt (`cat > task.txt <<EOF … never run git reset --hard … EOF`) is not mistaken for running it.
5. **Escape marker + kill switch + fail-open.** `[orchestra:allow-vcs: <reason>]` in the command or its description, `ORCHESTRA_VCS_GUARD=off`, and fail-open on every uncertainty.

**`enforcement.worker_tree`** (`"block"` default | `"off"` — quote the value) blocks a **worker subagent that already has an orchestra worktree of its own** from writing into the user's **main** working tree (`hooks/guards/worker-tree.ts`). It is the Edit/Write counterpart to `worker_vcs`, and it exists because prose demonstrably does not hold: a Sonnet worker whose prompt opened with `WORKING TREE: <path> / cd there FIRST and do every read, edit, test, and command inside it. Never touch … files outside that path` ignored the line and ran Edit/Write against the same files in the user's tree, whose rejected round-1 output would have been what the user kept. Claude Code's own `sandbox.enabled` cannot substitute: it is session-global (subagents inherit the parent's config) and covers only Bash, not Edit/Write. Like `worker_vcs` it ships **on** and an ambiguous value keeps the guard on; only an explicit `off` disables it. The deny fires only when **all four** hold — the call carries an `agent_id`; `agent_type` is not `orchestra-delegate`; `git worktree list --porcelain` run against the hook's `cwd` shows at least one branch under `orchestra/<session8>/` (the first 8 characters of `session_id`, lowercased); and the target's `realpath` is inside the **first** worktree git lists (the main tree) and inside no `orchestra/*` worktree. Comparison is by path components, so `/repo-worktrees/x` is not inside `/repo`, and a symlink or a `..` escape resolves to what it really is. Coverage and escapes:

1. **Structured tools are airtight, Bash is deliberately narrow.** `Edit`/`Write`/`MultiEdit`/`NotebookEdit` are read straight off `tool_input.file_path` (plus `notebook_path`) with no parsing — that is where the incident happened. For `Bash`, only three high-signal shapes are examined after the same heredoc/quoted-string stripping `worker_vcs` uses: `> PATH` / `>> PATH`, `tee [flags] PATH…`, and `sed -i … PATH…` (a `sed` target counts only if it already exists, which is what separates a path from an unquoted `s/a/b/` script). **`cp`, `mv`, `rm`, `mkdir`, `python -c` and every other interpreter invocation are not analyzed, on purpose** — parsing arbitrary shell for write targets costs more in false denials than it buys. A token carrying an unexpanded `$VAR`, a backtick, or a glob is skipped for the same reason.
2. **Express lane untouched.** With no orchestra worktree for the session there is nowhere else for the worker to write, and the guard never fires.
3. **Main thread and supervisor untouched.** Only calls carrying an `agent_id` are candidates — the instructor has none — and `orchestra-delegate` collects and integrates worker output into the main tree by design.
4. **Escape marker + kill switch + fail-open.** `[orchestra:allow-tree: <reason>]` in the command or its description (Bash-shaped calls; structured tools carry no such field, so use the kill switch or config there), `ORCHESTRA_TREE_GUARD=off`, and fail-open on every uncertainty — no `git`, unparseable stdin, `git worktree list` failing, `cwd` outside a repo, an unresolvable path, a config lookup that fails or times out.

**`enforcement.worktree_lease`** (`"block"` default | `"off"` — quote the value) gives each **linked** worktree to one writing subagent at a time (`hooks/guards/worktree-lease.ts`). Worktree isolation separates tasks from each other; it does nothing about two agents on the *same* task. Observed in a programme-scale run: a message to a running workflow engineer resumed a copy of it, and the copy and the original committed into the same worktree two seconds apart. The same shape is open to any retry of an agent that looked stalled but was still running.

1. **What takes the lease.** The first subagent write into a linked worktree: `Edit`/`Write`/`MultiEdit`/`NotebookEdit` targets, and Bash that runs a state-changing git verb (`commit`, `add`, `reset`, `checkout`, `merge`, `cherry-pick`, `revert`, …, including `cd <tree> && git …` and `git -C <tree> …`) or redirects into a file there. Reads, tests and `git diff` never touch the lease, so a reviewer inspecting a worker's tree is unaffected. The lease lives in the worktree's own git dir (`.git/worktrees/<name>/orchestra-lease.json`), never in the tree.
2. **Who is denied.** A different subagent writing while the holder wrote within `worktree_lease_window` seconds (default 120; `ORCHESTRA_LEASE_WINDOW` overrides). The denial tells a duplicate to stop and report `DUPLICATE`, and tells a legitimate next round to wait out the window.
3. **Release.** A `SubagentStop` hook drops every lease the stopping agent holds, so a correction round that starts right after its predecessor finished is never blocked; the window covers a holder that died without a `SubagentStop`.
4. **Untouched:** the main worktree (same-tree parallel work with disjoint ownership stays supported), the main thread, and `orchestra-delegate`.
5. **Escape marker + kill switch + fail-open.** `[orchestra:allow-lease: <reason>]`, `ORCHESTRA_LEASE_GUARD=off`, and fail-open on every uncertainty.
6. **Activity stamp (never denies).** Every Bash call made inside a linked worktree — by any agent, readers and the main thread included — refreshes `.git/worktrees/<name>/orchestra-activity.json`. `isolate remove`/`sweep` report a tree with a fresh lease or stamp as `busy` instead of removing it, so a test run in the tree protects it. A subagent's stamp is dropped at its `SubagentStop`; otherwise it expires after `ORCHESTRA_ACTIVITY_WINDOW` seconds (default 600, the longest a foreground Bash call runs). The same off switches disable it. The Go side reads the lease window in the hook's order: `ORCHESTRA_LEASE_WINDOW`, then `enforcement.worktree_lease_window`, then 120.

**`enforcement.turn_edits`** (`8` default | any positive integer | `"off"`) is the turn-size tripwire. Once the **main thread** has edited that many distinct files inside a single turn, a `PostToolUse` hook (`hooks/router/turn-edits.ts`) injects one piece of context asking it to re-classify into the orchestrated lane, and asks again, more firmly, at twice the threshold. Repeat edits to a file already counted do not add up: the count is files, not calls. It never blocks an edit. The number comes from turn-level transcript analysis: orchestra was invoked in 1 of the 102 turns that went on to hand-edit 10+ files, because the prompts that produce those turns ("作業を続けて", "実装して", "y") look small at classification time and only reveal their size once work is underway. Subagent edits are not counted — that is the delegation working. Each stage fires at most once per turn, stays silent in `cheap`-model sessions, and `ORCHESTRA_TURN_GUARD=off` disables it outright. An unusable value (0, negative, a non-integer string) falls back to the default rather than silently disabling the tripwire.

**`enforcement.session_cleanup`** (`"on"` default | `"off"`) removes the finishing session's orchestra worktrees at `SessionEnd` (`hooks/lifecycle/cleanup-worktrees.ts`), keeping any that still hold uncollected changes — a dirty worktree is the only record of what a worker tried, so the hook refuses to delete it rather than risk losing that evidence. It takes the session id from the `SessionEnd` event, calls `agent-exec isolate remove --session <id>` for the finishing session, and inspects the nested per-task results (the outer `"status":"removed"` is reported even when individual worktrees come back `"dirty"` or `"error"`) so dirty ones are simply left in place. An unrecognized config value falls back to `"on"`. `ORCHESTRA_SESSION_CLEANUP=off` disables the hook outright regardless of config.

**Safety note:** `cli` dispatch only ever executes command templates that come from the user's own configuration file (`.claude/orchestra.yaml` or `~/.claude/orchestra.yaml`). This plugin ships no CLI commands of its own and must never invent one; if no config file declares a CLI executor, `cli` dispatch is unavailable. The `agent-exec` binary those templates call through is not part of this plugin either — it's a separate tool the user explicitly installs and authorizes via their own `Bash(agent-exec:*)` permission rule (see the `setup` skill); this plugin only ever writes the command template that invokes it, never runs it directly, and never grants it permission on the user's behalf.

### 9.1 External executor model policy (pi), pricing, and CLI usage

Full detail lives in `references/external-executors.md` (Japanese) — pi's model ids/providers and invocation, the removed-executors note, exact CLI usage recipes (one-shot and session-continuation for retry rounds), and official per-token pricing for Claude and the metered Copilot-provider models. Read it before dispatching to an external executor, and before second-guessing any model/effort choice in the example config.

In brief, as of this plugin's own validation (full reasoning and every round-by-round result: `references/poc-findings.md`):

- **pi `light`/`standard`** → `openai-codex/gpt-5.6-luna` at `effort: medium` for `light` and `high` for `standard` (ChatGPT subscription; the fallback for `light` and `standard`, behind Claude Haiku 5.5, which leads both. Rationale: Haiku 5.5 solved the round7 judgment benchmark 3/3 vs Luna 1/3 (Luna is 2-4x faster but twice blamed the tests instead of tracing the failure); Claude subagents run outside the OS sandbox, pi inside it). **`deep`** → `openai-codex/gpt-6.1-sol`/high; **`independent-review`** → `openai-codex/gpt-6.1-sol`/medium. The standalone Codex CLI executor was removed in v0.52.0 (pi drives the same models); a leftover `external_executors.codex` block is ignored, and a `codex` priority entry is skipped by `route` as `unknown-executor:codex` (no warning)ing from `doctor`/`config` — delete it or re-run `orchestra:setup`. Unknown executor names in a user config are skipped as `unknown-executor:<name>` with a warning. Removed `copilot`/`opencode` entries are configuration errors with a migration guide — delete them or re-run `orchestra:setup`. See the reference doc and `references/poc-findings.md`.
- **Long-context caveat:** Luna has measurably weak long-context recall. Escalate a nominally `light`/`standard`-class task to `deep` if it requires deep traversal of a large repository.
- Across 6 rounds of escalating task difficulty, no accuracy differentiation was observed until task size crossed into genuine multi-file, multi-language feature territory — at which point every cheap tier tested eventually showed at least one real, narrow defect. Treat the adversarial review stage as mandatory beyond a trivially small change, regardless of which model/provider/effort level is implementing the work — this holds for external executors exactly as much as for Claude's own tiers.

## 2. Telemetry (opt-in, anonymized)



Orchestra can optionally emit a small, anonymized telemetry stream so the maintainer can see how the pipeline behaves in practice — class usage, executor fallback rates, dispatch outcomes — without ever seeing what any of it was about.

**Default off.** Telemetry is opt-in via `telemetry.enabled: true` in `orchestra.yaml` (see `examples/orchestra.yaml`). The resolved value surfaces at `doctor`'s `config.values.telemetry.enabled`, same as any other config key (section 9) — check that field rather than re-reading the YAML layers yourself. When disabled (the default, and the as-shipped state), nothing is written, and every `telemetry` subcommand that would record data is a silent no-op. To toggle without hand-editing YAML, use `agent-exec telemetry enable [--scope user|project|local]` or `agent-exec telemetry disable [--scope user|project|local]` — these perform a comment-preserving surgical edit of the scope's orchestra.yaml (creating a stub if absent) and require no manual YAML work.

**Crash-dump-style, allowlist-enforced.** Redaction is not a matter of callers behaving — `agent-exec` itself enforces an ALLOWLIST of field names and enumerated values; only enumerated categorical strings and non-negative integers can ever be stored, and `schema_version`/`ts`/`os` are stamped by `agent-exec` itself, never supplied by the caller. It is structurally impossible to record prompts, task text, file names, paths, task ids, `summary` strings, code, or error-message text: no field accepts free text, and enum fields are checked by exact match, so free text placed in an enum field is silently dropped rather than stored.

Allowed fields:
- `event`: `run_summary` | `dispatch`
- `lane`: `express` | `orchestrated`
- `orchestra_version`: semver
- `executor`: `claude` | `pi`
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
- **AUTO — per dispatch.** `agent-exec dispatch` self-logs one `dispatch` record after printing its result (status/reason, and `cls`: the routed class, or `--class` in direct mode). This is LLM-independent — it happens inside `agent-exec` regardless of who called it, and is a no-op when telemetry is disabled.
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
  "executors_used": { "claude": 2, "pi": 1 },
  "external_enabled": { "pi": 1 }
}
```

**Storage and CLI.** Records accumulate under `telemetry.dir` (default `~/.claude/orchestra/telemetry`, see `examples/orchestra.yaml`). CLI surface:

- **`agent-exec telemetry record (--json STR | --file F)`** — appends ONE sanitized record; no-op + exit 0 when telemetry is disabled; never echoes the record's content to stdout.
- **`agent-exec telemetry show [--json]`** — inspect what's stored.
- **`agent-exec telemetry archive [--out FILE]`** — bundle stored records into a `.tar.gz`.
- **`agent-exec telemetry clear`** — delete stored records.
- **`agent-exec usage [--since <N>m|<N>h|<N>d|<ISO8601>] [--run <id>[,<id>...]] [--session <id>[,<id>...]] [--list-runs] [--source claude,pi] [--all-projects] [--json|--text]`** — read-only aggregation of Claude transcripts and pi ledger/telemetry records. Use `--run` as the deterministic scope for a workflow run; it applies no time filtering and accepts a comma-separated list. `--session` is the deterministic session scope, also without time filtering, and accepts a comma-separated list. `--run` and `--session` may be combined as a de-duplicated union. An empty list element is a usage error (exit 2); unknown run or session ids contribute nothing and are not errors. `--list-runs` prints discoverable run ids and exits 0, ignoring `--since`; JSON is `{"runs":[{"run_id":str,"files":int,"first_ts":ISO,"last_ts":ISO}, ...]}`, sorted by `last_ts` ascending, while text prints one run per line. `--since` retains its 24-hour default for windowed measurement when no deterministic id is available. `--since` cannot be combined with `--run` or `--session`; `--list-runs` cannot be combined with `--run`, `--session`, or `--since`; each violation exits 2 with a message on stderr and nothing on stdout. Under a run or session scope, the JSON report has a top-level `scope` object: `{"kind":"window","since":<ISO>,"until":<ISO>}`, `{"kind":"run","run_ids":[...]}`, `{"kind":"session","session_ids":[...]}`, or `{"kind":"run+session","run_ids":[...],"session_ids":[...]}`. In text output the scope is the first line. The top-level `since`/`now` keys appear **only** under the window scope: a deterministic scope applies no time filter, so reporting one would invite a consumer to read the numbers as filtered. A source that cannot be attributed reports `{"attributable":false,"reason":<short fixed string>}` in place of its totals, or `<source>: not attributable (<reason>)` in text. Claude usage defaults to the current project; pi telemetry usage requires telemetry to be enabled, while ledger usage is independent of that setting.

  **Cost (v0.45.0).** The report carries a top-level `cost` object, and the `claude` entry carries `by_model_detail` (fresh input, cache writes by TTL, cache reads, output per model) and its own `cost`. All money is an integer in micro-USD (`estimated_micro_usd`, `components`, per-model `micro_usd`), like the ledger's `cost_micro_usd`. `components.claude` is an estimate from list prices (agent-exec's built-in price list: USD per 1M tokens, cache write 1.25x for the 5-minute TTL and 2x for the 1-hour TTL, cache reads per model); `components.pi` is what the provider reported. `unpriced_sources` lists everything the figure leaves out — any `claude:<model>` with no known price — and the text output prints `PARTIAL: unpriced …` when it is non-empty. Not modelled: batch discounts, fast-mode premiums, data-residency multipliers. Add or override prices in `orchestra.yaml`: `pricing: {models: {<model-id-prefix>: {input: <USD/1M>, output: <USD/1M>, cache_read: <USD/1M>}}}`; `cache_write_5m` / `cache_write_1h` default to 1.25x / 2x of `input`, `cache_read` to 0.1x.

  **Claude token counts de-duplicate by message id (v0.45.0).** Claude Code writes one API response to the transcript once per content block, each line carrying the same `usage`, and a resumed or forked session copies earlier lines again. Before v0.45.0 every line was summed, which overstated a real session about 2.5x in input, output and cache reads. Each message id is now counted once (a later line for the same id replaces an earlier one, since a streamed response's last line carries the final output count); lines without an id are still all counted. Figures taken before v0.45.0 are not comparable.

### 2.1 Timing trace (always on, local)

Separate from telemetry, which is anonymized and allowlisted, `agent-exec` writes one JSON line per finished operation to `<state dir>/trace/YYYY-MM-DD.jsonl` (`~/.local/state/claude-orchestra/trace/` unless `XDG_STATE_HOME` is set; override with `ORCHESTRA_TRACE_DIR`, disable with `ORCHESTRA_TRACE=off`; 14 days are kept). It records paths and commands and never leaves the machine.

- What is timed: every git subprocess (`git worktree add`, `git status`, …), tree removal (`rm -rf (detached)`, `rename-aside`), dependency copies (`cp -c -R`, `clonefile`), herdr CLI calls, `agent-exec` self-calls from the wave runner, wave verify/notify commands, lock waits (`wave integration`, `wave dispatch start`, `herdr warm`, flock waits of 50 ms or more), the warm reader rebuild, and — through the orchestra hook — every Bash command a Claude session runs (`worker-bash`, with the session id as `pkg`).
- Record: `{"at", "end", "ms", "kind", "op", "pid", "pkg"?, "cwd"?, "exit", "load1", "detail"?}`.
- `agent-exec trace report [--since 24h] [--kind K] [--op-prefix P] [--min-count 3] [--json]` groups by operation and, for each, shows count / p50 / p95 / max and p50 per concurrency bucket (how many other non-lock operations were running when it started: 1, 2-3, 4-6, 7-10, 11+). `sensitivity` is p50 in the busiest bucket over p50 in the quietest one: the operations that slow down as parallelism goes up sort first. Lock waits are listed separately by total time — a long lock wait is what a stall where every package starts in the same second looks like.

## 3. Run ledger

The ledger is a default-on, local-only store, separate from telemetry and never uploaded. `ledger` participates in the same four-layer deep merge as `telemetry`: `enabled` defaults to `true`, `dir` to `"~/.claude/orchestra/runs"`, and `retention_days` to `30`; `retention_days` must be a positive integer or it falls back to `30`. `ledger.enabled: false` stops all ledger writes, but does not prevent reading existing data, and either setting is independent of `telemetry.enabled`.

Each session is a directory, `<ledger-dir>/<session-id>/`, where the session id comes only from the `CLAUDE_CODE_SESSION_ID` environment variable and must match `^[A-Za-z0-9_.-]{1,128}$` (with `..` rejected). A dispatch with `--run-id <id>` writes to `<ledger-dir>/<session-id>/<run-id>.jsonl`; one without it writes to `<ledger-dir>/<session-id>/no.run.jsonl`. Run ids must match `^[A-Za-z0-9_-]{1,64}$` and still exit 2 when invalid. The run id is carried by the path, not by a record field. Ledger lines retain the existing allowlist — `ts`, `executor`, `cls`, `model`, `status`, and the integer usage counters — and appends are best-effort and never alter dispatch behavior. Flat pre-v0.20.0 `<ledger-dir>/<run-id>.jsonl` files are inert history, readable only through `ledger show`, never through `--run` or `--session`, and are not migrated. A v0.19.0 session file and a v0.18.0 run file cannot be told apart, so continuing to read them is the one remaining way a session total could be reported as a run total.

`agent-exec usage --run <id>` reads every `<id>.jsonl` inside every session directory, de-duplicating by resolved path; flat legacy files are inert. A session directory can therefore never satisfy a run lookup. `--session <id>` reads every JSONL file directly inside that session directory and never reads legacy flat files. Under session scope, pi reports normal ledger totals when lines exist and adds a `runs` breakdown keyed by each run id plus literal `no-run`. Without matching lines they report `attributable: false` with reason `no matching run ledger data`; `--since` behavior is unchanged. A session figure is a different quantity from a run figure: a dispatch with no run id belongs to its session and to no run, so a session total must never be used as the cost of a single run. On the Claude side, a run lookup requires the literal `workflows/<run-id>` path segment.

There is no separate ledger command: read it through `agent-exec usage` (`--run`, `--session`, `--list-runs`). Old session directories are swept by `ledger.retention_days`; to discard the ledger sooner, delete `<ledger-dir>/<session-id>/` by hand.

After a successful append, at most once per process, old `*.jsonl` files directly in the ledger directory and directly inside session directories are deleted. Cleanup is best-effort, never removes a session directory, and failures never affect dispatch.

No workflow run id is inferred from the environment, a transcript path, or directory mtimes. A run id reaches agent-exec only through `--run-id`, because inference makes a cost number untrustworthy.

## 4. Config-driven checks (`agent-exec check`)

`checks` participates in the same four-layer deep merge as everything else, with one exception: `checks.items` is a **list**, so a higher layer replaces it wholesale rather than merging entry-by-entry — a project's own check list does not silently inherit a user-level one it never asked for, and there is no sane per-name merge for a list of shell commands anyway. Defaults: `checks.max_parallel: 2`, `checks.items: []`, `checks.retry_failed_alone: true`, `checks.flaky_threshold: 3`, and `checks.ensure: []`.

Each entry in `items` is:

```yaml
checks:
  max_parallel: 2          # concurrent check commands machine-wide, across every
                            # agent-exec process; 0 = unlimited
  retry_failed_alone: true # retry failed tests using only their reported files
  flaky_threshold: 3       # wave-runner threshold for repeated flaky checks
  ensure:
    - exists: "web/node_modules"
      run: "pnpm install --frozen-lockfile"
  items:
    - name: lint
      phase: lint           # prepare, lint, type, test, or heavy; default test
      paths: ["**/*.ts", "**/*.tsx"]   # optional glob list; omitted = always runs
      run: "pnpm exec biome check {files}"
      fix: "pnpm exec biome check --write {files}"   # optional; runs first,
      # its exit code and output are ignored — a fixer is not a gate
      cwd: "web"             # optional, relative to the tree root, default "."
      timeout: 900           # optional seconds, default 1800
      junit: "reports/junit.xml"   # optional, relative to `cwd`
      retry_alone: "pnpm test -- {failed}"  # optional isolated retry template
      fast: true             # optional; wave run's per-package check in
                             # verify_mode final (see §9). Default false = full
```

`paths` globs are matched against changed paths, POSIX-relative to the tree root: `**` stands for zero or more whole directories (`**/*.ts` matches both `a.ts` and `x/y/a.ts`), while `*`/`?` stay within one path segment (`src/*.ts` does not match `src/x/a.ts`). A check with no `paths` always runs, independent of what changed.

`{files}` in `run`/`fix` expands to the matched, still-existing files, re-expressed relative to `cwd` (a file outside `cwd` is dropped), each shell-quoted and space-joined. If `run` contains `{files}` and none remain after that filtering, the check is reported `skipped` with reason `no matching files` — it is never run with an empty argument list.

Checks run by phase (`prepare`, `lint`, `type`, `test`, `heavy`) and then config order, stopping at the first failure unless `--all` is given; unreached checks are reported `skipped` with reason `not run after earlier failure`. `fix` (when present) always runs immediately before `run`, in the same `cwd`, with the same `{files}` substitution. An `ensure` command runs only when its `exists` path is missing, before items; a failed ensure is named `ensure:<exists>`. Wave `--full` verification runs these ensure entries before every full check and bisect probe, including the last-green baseline probe. Failed checks can be retried alone: `retry_alone` uses `{failed}`, or a `{files}` check run is adapted automatically. A successful retry reports `flaky`; `--no-retry` disables this behavior.

Both `fix` and `run` execute via `/bin/sh -c` in their own process group; on `timeout` the whole group is killed, not just the shell, so a backgrounded child cannot outlive the check. Before each `fix`/`run` command, agent-exec acquires one of `max_parallel` concurrency slots (lock files under the same state directory as the token/cooldown stores, polled every 0.5s); `max_parallel: 0` means unlimited, no slot is acquired at all. This bounds how many check subprocesses run at once *across every `agent-exec check` invocation on the machine*, not just within one call. Each `fix`/`run`/`ensure` command (and an isolated retry) additionally holds ONE slot of the machine-wide heavy pool while it runs (§10); the isolated retry keeps every `max_parallel` slot to itself but still takes a single heavy slot, never the whole pool.

On failure, if `junit` is set, that file is deleted before the command runs (so a stale report from a previous round is never misread); afterward, if it exists and parses as JUnit XML, the excerpt leads with one line per failing/erroring `<testcase>` (`classname > name: <first line of the failure/error message>`), then fills the remaining budget with the same `failure_excerpt()` used elsewhere in agent-exec, capped at 4000 characters total. Without a usable `junit` file, the excerpt is `failure_excerpt(stdout+stderr, 4000)` directly. A passing check has excerpt `""`.

CLI: **`agent-exec check (--task <id> | --path <dir>) [--since <ref>] [--files a,b,...] [--all] [--baseline] [--repo <path>] [--session <id>] [--json|--text]`** — exactly one of `--task`/`--path`. `--task` resolves the task's worktree the same way `isolate diff` does, and its changed files come from that diff (`isolate diff --task <id>`, files only, no patch). `--path` runs against an arbitrary directory; its changed files are `git diff --name-only <since>` (default `HEAD`) unioned with untracked files. `--files a,b,...` overrides the changed-file list outright, in either mode.

`--baseline` re-runs each **failed** check, over the same file list (filtered to files that still exist there), inside a temporary detached worktree checked out at the task's recorded baseline commit (or `--since`/`HEAD` for `--path` mode) — carrying in the same gitignored dependency directories (`node_modules`, `.venv`, ...) an ordinary orchestra task worktree gets, so a check that shells out to a real toolchain still has one. If the check fails there too, its status becomes `preexisting` instead of `fail`: the breakage predates the change under test, so the worker should not be blamed for it. The temporary worktree is always removed afterward, success or failure.

Result JSON: `{"status": "pass"|"fail"|"preexisting"|"no-checks"|"error", "tree": <path>, "files": <int>, "checks": [{"name", "status": "pass"|"fail"|"preexisting"|"skipped", "exit": <int|null>, "seconds": <float>, "timed_out": <bool>, "excerpt": <str>, "reason": <str>}, ...]}`. Overall `status` is `fail` if any check failed, else `preexisting` if any check is `preexisting`, else `no-checks` if there were no items or every one was skipped, else `pass`. Exit code mirrors `status`: 0 for `pass`/`no-checks`, 1 for `fail`, 2 for a usage error, 3 for an environment error (not a git repository, unknown task, bad `--path`), 4 for `preexisting`. `--text` prints one line per check (`name status seconds`) followed by the excerpt of any failures, and nothing else — `--json` is the default.

## 5. Orca sessions (`orca`)

`orca` participates in the four-layer deep merge (`models` merges key-by-key). It decides what `agent-exec wave run` does with a package whose route resolves to a Claude tier — the `delegate` case — when Orca (a desktop app with an `orca` CLI) is installed:

```yaml
orca:
  enabled: auto            # auto = use when `orca` is on PATH and `orca status` is ok;
                           # true = required (the run stops: "orca unavailable: <reason>"); false = never
  command: "claude --model {model} --permission-mode {permission_mode}"
  models: {light: sonnet, standard: opus, deep: opus}   # used when the dispatch route's model is absent
  permission_mode: auto         # (v0.49.0 default) acceptEdits, auto, default, or plan; auto avoids most stalls
  user_config: isolate     # isolate | inherit; see the herdr notes below (same flags, appended to `command`)
  add_dirs: []             # extra paths; ~ expands and relative paths are repo-relative
  auto_trust: false        # true = accept Claude's folder-trust dialog automatically,
                           # ONLY for worktrees this executor created
  startup_timeout: 90      # seconds to reach the prompt box
  task_timeout: 3600       # seconds per prompt before the package becomes a need
  keep_sessions: true      # keep terminal+worktree alive until the package is integrated;
                           # false = close the terminal after each prompt (the worktree stays)
  setup: skip              # Orca worktree setup: skip, inherit, or run
```

`enabled` accepts YAML booleans or the strings `auto`/`true`/`false`; anything else means `auto`. `{model}` in `command` is replaced by the route's model, else `models.<cls>`, and `{permission_mode}` is replaced by `permission_mode` when present. Only `acceptEdits`, `auto`, `default`, and `plan` are allowed; permission bypass modes and `--dangerously-skip-permissions` are refused. Orca can reach the repository root, the state/spec/preamble directories, and every `add_dirs` entry. Without Orca, or with `enabled: false`, `wave run` behaves exactly as before: the package lands on the needs list as `delegate`. The run-time behaviour is described in `programme.md` §6 ("Orca sessions").

## 6. Managed executor tools (`tools`)

`tools` pins the binaries orchestra depends on. Orchestra installs each one at the pinned version into its own state directory (`$XDG_STATE_HOME/claude-orchestra/tools`, or `~/.local/state/claude-orchestra/tools`), so a personal install of the same tool cannot change worker behavior. (v0.47.0)

```yaml
tools:
  installer: auto          # auto = mise when on PATH, else npm; or mise | npm
  pi:
    source: auto           # auto | managed | system (see resolution order below)
    spec: "npm:@earendil-works/pi-coding-agent"
    version: "1.2.0"
    bin: pi
  herdr:                   # the herdr host (§8)
    source: auto
    spec: "aqua:herdrdev/herdr"
    version: "0.8.2"
    bin: herdr
  srt:                     # sandbox-runtime: the herdr host's network allowlist (§8, v0.50.0)
    source: auto
    spec: "npm:@anthropic-ai/sandbox-runtime"
    version: "0.0.79"
    bin: srt
```

**Resolution order** (`source` per tool, default `auto`):

- `auto`: use the managed copy when its record matches the configured `spec` and `version` and the file is still executable. Otherwise use the binary on PATH. When PATH is used while a `spec` is configured, doctor adds a `hint`.
- `managed`: only the managed copy. When it is missing or stale, the tool is unavailable and doctor gives the install hint.
- `system`: only the binary on PATH.

**Manifest.** Each install writes `<state>/tools/manifest.json` with `spec`, `version`, `installer`, `path`, and `installed_at`. A missing or corrupt manifest counts as empty. A manifest entry is stale when its `spec` or `version` differs from the config, or its `path` is no longer executable. The manifest is written only after a successful install.

**Installers.**

- `mise` runs `mise install <spec>@<version>` in a private mise world. `MISE_DATA_DIR`, `MISE_CONFIG_DIR`, `MISE_GLOBAL_CONFIG_FILE`, `MISE_CACHE_DIR`, and `MISE_STATE_DIR` all point under `<state>/tools/mise/`, and `MISE_CEILING_PATHS` is set. The user's global and project mise config is neither read nor modified.
- `npm` runs `npm install --prefix <state>/tools/npm/<name>/<version> <package>@<version>` and uses `node_modules/.bin/<bin>` from that prefix. It only supports `npm:` specs.

**Doctor fields.** Each executor entry in `agent-exec doctor --json` has `path`, `source` (`managed` or `system`), and `tool_version` (the managed record's version, or null when the binary comes from PATH). A `hint` (`agent-exec tools install <name>`) appears when the managed copy is missing or stale or PATH is being used in place of a configured `spec`. When pi is unavailable, `ready.pi.missing` contains `executor-binary-unavailable`. The run should relay that hint to the user rather than installing silently.

**CLI.**

```bash
agent-exec tools                         # status (same as `tools status`); --json or --text
agent-exec tools install                 # install every configured tool that is not already valid
agent-exec tools install pi --force      # reinstall even when the record is valid
agent-exec tools install --installer npm # override the installer for this call
```

Install reports `installed`, `already`, `failed` (with the tail of the installer's stderr), or `skipped` (no `spec` or `version`) per tool. There is no update command: change `version` in the config, then run `agent-exec tools install <name>`.

## 7. Dedicated pi agent directory (`external_executors.pi.agent_dir`)

By default pi runs with its own agent directory, not the user's `~/.pi/agent`. A user's pi settings, models, `AGENTS.md`, installed packages, and pi version all change worker behavior, and none of them are part of what the routing and review were measured against. (v0.47.0)

```yaml
external_executors:
  pi:
    agent_dir: managed     # managed (default) | user | a path
    context_files: true    # false adds --no-context-files (no AGENTS.md / CLAUDE.md)
```

- `managed` (default): `<state>/pi-agent`, created with mode `0700`. Orchestra sets `PI_CODING_AGENT_DIR` to it for every pi child. pi may write it under every backend (srt, Seatbelt, bubblewrap): its sandbox policy denies the state dir's tamper-protected entries by name (the learned-grants store, `tools/`, `herdr-srt/`, `cli-srt/`, `audit/`) rather than the whole state dir, since a deny wins over every allow below it.
- `user`: pi's own default, `~/.pi/agent`. Orchestra sets no `PI_CODING_AGENT_DIR`.
- a path: that directory (`~` is expanded). `PI_CODING_AGENT_DIR` is set to it.

**Login carry-over (`auth.json`).** When the managed directory has no `auth.json` and the user's pi `auth.json` exists, orchestra creates a symlink `auth.json` in the managed directory that points at the user's file. The source is `$PI_CODING_AGENT_DIR` when that is set and differs from the managed directory, otherwise `~/.pi/agent`. The check runs on every pi call, so the link appears once a login exists. An existing `auth.json` (file or symlink) is never overwritten or removed. Because the link is followed, token refreshes write to the user's own `auth.json`, and pi's sandbox writes to the symlink target's directory (see `external-executors.md` §2a).

To log in for workers, Claude runs `agent-exec pi login` (it drives pi's `/login` in orchestra's own herdr and prints the URL and device code), shows them to the user, then runs `agent-exec pi login --wait`; `--paste TEXT` sends a final redirect URL when the browser could not reach localhost and `--cancel` aborts. Logging in with your own pi also works while the link exists, because the managed directory reads the same `auth.json`.

## 8. Interactive host selection (`interactive_host`, `herdr`)

`wave run` sends a package whose route resolves to a Claude tier to an interactive Claude Code session instead of returning it as a `delegate` need, when an interactive host is available. The host is chosen by the top-level key `interactive_host`. (v0.47.0)

```yaml
interactive_host: auto     # auto | herdr | orca | none
herdr:
  enabled: auto            # auto | true | false, same meaning as orca.enabled
  args: "--model {model} --permission-mode {permission_mode}"   # native claude args, after `--`
  models: {light: sonnet, standard: opus, deep: opus}
  permission_mode: auto    # (v0.49.0) Claude's classifier approves tool calls; acceptEdits would stall on every Bash dialog
  user_config: isolate     # isolate | inherit; isolate keeps the user's MCP servers, hooks and plugins out of the worker
  auto_trust: true         # (v0.49.0) only for worktrees orchestra created, under a sandboxed server
  startup_timeout: 90
  task_timeout: 3600
  idle_grace: 300          # (v0.61.0) seconds idle with no result before one nudge, then a need
  keep_sessions: true
  warm: {enabled: true, ttl_seconds: 3000, min_packages: 2}   # (v0.48.0)
```

- `auto`: herdr when it is available, else Orca when available, else none. `herdr` or `orca` selects only that backend. `none` never uses an interactive host. Each backend's own `enabled` still applies. In `auto`, a backend with `enabled: true` that is unreachable stops the run with `<name> unavailable: <reason>`.
- `herdr` runs each package in its own orchestra-owned worktree (task `pkg-<id>`), with one workspace and one named agent. With either host active, `wave run` prepares package dispatches with `--isolate cli`, so the dispatch creates no tree of its own for a delegate route ahead of the host's. Completion is signalled only by the result file the prompt asks Claude to write.
- **Idle without a result.** A worker that sits idle with an unchanged screen for `idle_grace` seconds (default 300) and no result file is nudged once: a prompt left unsubmitted in the input box (a swallowed Enter, a `[Pasted text …]` placeholder) gets Enter, an empty box gets the prompt again, prefixed with "write the result file now if the task is already done". Idle for another `idle_grace` after that is a `stalled` need. A result file that exists but is not valid (bad JSON, or a status other than `ok`/`escalate`) is an `error` need as soon as the worker is idle, naming the file and the parse error. Both used to wait the full `task_timeout`.
- `args` are passed after `--`. The prompt and result directories and `add_dirs` are appended as `--add-dir`; a directory under another listed one is dropped, since `--add-dir` covers the whole subtree.
- **Start line limit (v0.65.0).** herdr types the `claude …` command into a fresh pane, possibly before the shell's line editor is up; the terminal is then in canonical mode and drops input past `MAX_CANON` (1024 on macOS) together with the newline, so claude never starts and herdr only reports `timed out waiting for agent startup`. A start line (arguments shell-quoted) over 1000 bytes is therefore refused as an `error` need before any workspace is created; shorten it by keeping specs in one directory or putting the state dir on a shorter path. When `agent start` fails for any other reason, the need's detail ends with the tail of the pane's screen.
- `auto_trust: true` accepts Claude's folder-trust dialog only for worktrees the executor created itself, as with `orca`.
- **`user_config` (herdr and orca).** Every start (fresh, warm reader, forks, reopen) gets `--settings '{"disableRemoteControl":true,"promptCacheTtl":"1h","subagentPromptCacheTtl":"1h"}'` in both modes, so the worker registers no Remote Control session (no CLI flag can do that) and keeps its prompt cache for an hour between prompts, its own subagents too (their default is 5m). `isolate` (default) also appends `--strict-mcp-config --setting-sources project,local --plugin-dir <orchestra plugin root> --no-chrome`, puts `disableClaudeAiConnectors` and `autoMemoryEnabled: false` into that one `--settings` JSON, and sets `CLAUDE_CODE_DISABLE_BUNDLED_SKILLS=1` and `CLAUDE_CODE_AUTO_CONNECT_IDE=false` in the worker's env (herdr: the env of each workspace, passed as `workspace create --env` and decided per package tree, so a setting change takes effect for the next package start even on a running herdr server; orca: an `env` prefix on the command). So the worker loads none of the user's MCP servers, user hooks or user-enabled plugins; the repo's own project/local settings and the user's CLAUDE.md still apply, and orchestra's hooks and agents come back through `--plugin-dir`. A flag already present in `args`/`command` is not added again. A `--settings` in `args`/`command` must set `"disableRemoteControl": true` itself, otherwise the start fails; orchestra then adds no `--settings` of its own. `inherit` keeps the user's own setup and adds only the Remote Control setting. `agent-exec doctor` shows `user_config=` for both hosts.
- **Session naming.** Every herdr call carries `--session orchestra-<8 chars>`. The 8 characters come from `CLAUDE_CODE_SESSION_ID`, falling back to the run id. The user's own herdr sessions are never touched.
- **Own herdr home and sandbox (v0.49.0).** orchestra runs its own herdr: every herdr call and the server get `XDG_CONFIG_HOME=<orchestra state dir>/hx` (config, sockets and sessions live under `hx/herdr/`), so the user's herdr is never reached. When that path would exceed the unix socket limit (100 bytes), the home is `/private/tmp/orchestra-<uid>/hx` (macOS) or `/tmp/orchestra-<uid>/hx`; that parent must be a private directory of the user, otherwise the run stops with an `error` need. Before each server start orchestra rewrites `hx/herdr/config.toml` (no onboarding, toasts, sounds or update checks; no agent resume on restore; no pane history). The server runs under the executor `sandbox` (Seatbelt on macOS, bubblewrap on Linux; Landlock does not count), which confines every pane and the Claude inside it: writable are the orchestra worktree base of the repo, the git worktree/object dirs, the orchestra state dir and herdr home, Claude's config dir (and the real path of each symlinked child), `~/.claude.json*`, Claude's cache and state dirs, temp, `sandbox.allow_write` and learned grants; the user's own tree is not. Claude's `settings.json`, `settings.local.json`, `hooks`, `skills`, `commands`, `agents`, `plugins` and `CLAUDE.md` are denied after the allows. The one exception is `<Claude config dir>/plugins/data/orchestra-inline`, the data dir of the orchestra plugin loaded with `--plugin-dir` (its hooks create it on every event): orchestra creates it before the server starts and keeps it writable, while the rest of the config dir's `plugins/` stays read-only (`cache`, `marketplaces`, `repos`, `installed_plugins.json`, `known_marketplaces.json`, `config.json` and other plugins' data dirs). Under srt, which has no write carve-out, this is done by denying every existing entry beside that path plus those names; a new sibling name under `plugins/data/` is not covered. `sandbox.mode: required` without a backend stops the run; `auto` runs unsandboxed and logs `herdr not sandboxed: <reason>`. `agent-exec doctor` shows the herdr home and whether the host would be sandboxed.
- **Network allowlist via sandbox-runtime (v0.50.0).** With `sandbox.network.mode: allowlist` (default), a resolvable `tools.srt` (on Linux also `bwrap` and `socat` on PATH) and a host that would be sandboxed, the server runs as `srt -d -s <state>/herdr-srt/<session>.json --control-fd 3 -c "<herdr server>"` instead of under orchestra's own Seatbelt/bubblewrap wrapping (never both). The settings file (dir 0700, file 0600) is generated from the same host policy: `allowWrite` = the writable set as real paths plus `~/.claude.json*`, `denyWrite` = the tamper denies (including the repo's `.claude/orchestra*.y*ml`), `denyRead` = `deny_read`; `allowUnixSockets` = exactly this session's `herdr.sock` and `herdr-client.sock`, `allowAllUnixSockets: false`, `allowPty: true`; `allowedDomains` = `allowed_domains`, the registry presets of the repo's tracked files (`sandbox.network.ecosystem`) and the learned domains, `deniedDomains` = `denied_domains`. srt's debug output goes to `hx/herdr/sessions/<session>/srt.log`; every `Connection blocked to <host>:<port>` line is appended to `<state>/audit/network.jsonl` (`{ts, session, host, port, decision: "blocked"}`), and a package whose session logged blocked hosts since it started carries `network_denials: [{host, port}]` (de-duplicated) on its result or need, shown by `wave run` and `wave status` — it does not fail the package (hosts matching `quiet_domains` are left out). In a wave each such host is a `net:<host>` item of `agent-exec approvals`; decide it with `agent-exec approvals decide`, or approve directly with `agent-exec sandbox allow-domain <host>`: the executor re-reads the learned domains on every wait poll, scheduling tick and before every prompt and, when they changed, writes one full-config line to srt's control channel, so the running server picks it up without a restart. The channel is a FIFO at `<state>/herdr-srt/<session>.ctl` (0600, dir 0700; no hosted process can write it): the process that starts the server opens it read-write and hands that descriptor to srt as fd 3 (srt replaces it with `/dev/null` for the server, and never sees EOF), so a later `wave run` that attaches to the server an earlier run left running (`keep_sessions`) pushes through it too — the line is written only when the settings differ from `herdr-srt/<session>.json` on disk, which is rewritten after the push. When the FIFO is missing, not a FIFO or nobody reads it (a server started before this mechanism, or whose srt is gone), the settings are stale: with srt usable and no registered session of this state mid-task (its agent `working` or `blocked`) the server is stopped and started afresh, logging `network settings stale: <why>; restarting the herdr server`; otherwise it keeps running and the run logs `network not restricted: network settings stale: <why>`. When srt cannot be used, the server falls back to the plain host sandbox and logs `network not restricted: <why>` (an `orca-session` event with `action: network`); `agent-exec doctor` shows the network mode, the srt path/version and whether the host network would be restricted.
- **Permission bypass (opt-in, v0.50.0).** `permission_mode: bypassPermissions` (or `--dangerously-skip-permissions` in `args`) is accepted only while the server this run started is running under srt with the network allowlist enforced and the host filesystem sandbox applies; otherwise the package ends as an `error` need naming the missing condition. The default stays `auto`; Orca always refuses both.
- **`auto_trust` (default true, v0.49.0).** The folder-trust dialog is accepted automatically only when the worktree was created by the executor AND the server is actually sandboxed (started by this run under an enforced backend). Otherwise the package becomes a `trust` need, exactly as with `auto_trust: false`. An explicit `auto_trust: false` always wins.
- **Server lifecycle.** The herdr server is started detached when it is not already running. `shutdown()` stops it once no workspace is left open in its session, whoever started it; while `keep_sessions` is true and package sessions are registered in `<state>/herdr-sessions.json`, it stays up so `resume` can reach them. A worktree removal that closes the last workspace of a session stops that session's server too.
- **Warm reader (v0.48.0).** When the host is herdr, `warm.enabled` is true, the plan has a non-empty read set, and at least `warm.min_packages` packages of the run start on the same model, the first such package first triggers one warm session per (model, base revision): a dedicated orchestra worktree `warm-<model>` at the integration base, in which a Claude session reads the plan's `preamble` files that live inside the repo plus its top-level `warm_read` (repo-relative paths). Every package on that model then starts with `claude --resume <warm session> --fork-session` and reads the shared files from the prompt cache instead of from disk. The first prompt of a forked package starts with one line telling it its own working tree and that the earlier messages came from the warm worktree. The warm record (`_warm` key of `herdr-sessions.json`) is reused while `now - last_used < ttl_seconds` and no file of the read set changed between its base revision and the current one; otherwise it is rebuilt. If a fork leaves the warm worktree dirty, that package ends as an `error` need (`fork wrote outside its tree`) and the record is rebuilt next time. Any warm failure emits a `warm-failed` event and the packages start fresh, exactly as without warm. Warm worktrees are removed at host shutdown; warm records alone do not keep the herdr server alive.
- **Server environment.** The server is started without `CLAUDE_CODE_*` variables and without `CLAUDECODE`. A child Claude that inherited those markers would stop saving transcripts.

## 9. Wave runner (`wave`)

`agent-exec wave run` reads a top-level `wave` key (v0.49.0). An explicit CLI flag or run option wins over it.

```yaml
wave:
  max_in_flight: auto      # auto = every startable package | positive integer;
                           # --max-in-flight N|auto overrides
  memory_guard: true       # false = never hold new starts for memory pressure
  usage_every: 10          # a `usage` notice every N integrated packages; 0 = none
  budget_usd: 0            # stop new starts at this estimated run cost; 0 = none;
                           # needs --run-id; --budget-usd overrides
  verify_mode: final       # final | gated
  converge_rounds: 2       # fix-up rounds in final mode before handing over
```

- **Rolling scheduling.** Every package that may start runs at once (an integer `max_in_flight` caps them). When one finishes it is integrated right away (one integration at a time) and its slot goes to whatever may start now, so a dependent starts as soon as its dependencies are integrated, without waiting for unrelated packages. There is no per-wave barrier; a wave is one scheduling round that started something.
- **`max_in_flight: auto`** (default). No width: every package whose dependencies are integrated, that is not blocked and whose `files_owned` overlap nothing in flight or already chosen starts now, whatever the machine or host. Heavy commands are admitted machine-wide (§10) and memory pressure holds new starts (`memory_guard`), so the width is never the tool for machine load. An integer from `--max-in-flight` or from this key caps it as is. The value is printed once at start (`max-in-flight: auto (unbounded)` or `max-in-flight: N (config|option)`) and recorded as a `max-in-flight` event.
- **Start order.** Highest `prio` first, then the longest chain of packages that transitively depend on the package (the sum of their plan `est_seconds` when every package below each startable package has one, else their count; one pick never mixes seconds with counts), then plan order. Computed once per plan, again when fix-up packages are added.
- **`memory_guard: true`** (default). Before starting packages the runner asks the OS for memory pressure: darwin `sysctl kern.memorystatus_vm_pressure_level` (1 normal, 2 warn, 4 critical; holds at 2 or more), linux `/proc/pressure/memory` `some avg10` of 20 or more, or without PSI `MemAvailable` below 10 % of `MemTotal`. Under pressure new starts wait and are re-checked every 30 s; packages in flight keep running, verifying and integrating. One `start-held` event (with the reason) marks the start of a hold and one `start-resumed` its end. Another platform or a probe that fails never holds. `false` turns it off.
- **`usage_every: 10`.** Each time the run's integrated count crosses a multiple of N it records a `usage` event (`integrated`, `in_flight`, `pending`, `estimated_usd`: the run's estimated cost when it has a `--run-id`, else null) and runs `--notify-cmd` with `WAVE_EVENT=usage` and the same JSON plus `event`/`at` on stdin. It never pauses the run; `0` sends none.
- **`budget_usd: 0`.** With a positive value and a `--run-id`, the runner stops starting packages once the run's estimated cost — the `cost.estimated_micro_usd` that `agent-exec usage --run <id>` reports, re-read at most once a minute — reaches it: the run stops with reason `budget reached: ~$N of $X` (exit 5), the packages in flight finish and integrate, and nothing new starts, exactly as after `wave stop`. `--budget-usd X` overrides it and requires `--run-id`; a configured budget without a run id is ignored with a warning and a `budget-off` event.
- **`verify_mode: final`** (default). Per package only `checks.items` with `fast: true` run (§4); a red one is recorded in the package detail and a `fast-check` event and blocks neither integration nor dependents. There is no default integration gate (`--gate` still applies). After everything is integrated or held by a need, the full checks — every item without `fast`, then `--full` — run on the integration tree, at most one full run at a time per process. Red failures become `fix-<n>` packages mapped deterministically to the package whose diff or `files_owned` contains a path the failure names (unmatched: one `deep` catch-all), which are scheduled, integrated and re-checked up to `converge_rounds` times; then the run stops with a `post-integration` need. See `programme.md` §6.
- **`verify_mode: gated`.** The pre-v0.49.0 verification: every check item per package with one correction round, the default `agent-exec check` integration gate, and `--full` every `--full-every` integrations, pausing the run on red (bisect, revert, stop).

## 10. Heavy-command admission (`throttle`)

Installs, tests, typechecks, lints and builds run on the local machine, whichever executor started them. Up to about five at once they run at full speed; beyond that each one gets several times slower. So every heavy command is admitted machine-wide, a few at a time, while the number of workers stays high (a worker spends most of its time waiting on the model, not on the machine).

```yaml
throttle:
  heavy_slots: auto        # auto = max(2, (cpus + 1) // 3), 14 cores -> 5; a positive
                           # integer; 0 = off (everything admitted at once)
  heavy_extra: []          # extra regexes for heavy commands, UNIONED across layers,
                           # e.g. ['^make (check|test)\b']
```

- **What is heavy.** Package installs (`pnpm|npm|yarn|bun install|i|add|ci`, bare `yarn`, `uv sync`, `pip install`, `cargo fetch`, `go mod download`, `bundle install`, `poetry install`), tests (`vitest`, `jest`, `mocha`, `pytest`, `tox`, `playwright test`, `go test`, `cargo test`, `pnpm|yarn|bun|npm test`), typechecks (`tsc`, `vue-tsc`, `mypy`, `pyright`, `cargo check`), lints (`eslint`, `biome`, `ruff`, `golangci-lint`, `cargo clippy`) and builds (`esbuild`, `vite build`, `next build`, `cdk synth`, `go build`, `cargo build`), plus the package scripts `test`, `lint`, `typecheck`, `type-check`, `tsc`, `build` (and their `name:suffix` variants) run through `pnpm|yarn|bun [run]`, `npm run` or `turbo [run]`. Env assignments, `cd dir &&`, `time`/`nice`/`timeout`/`env`/`sudo` and runners (`npx`, `pnpm exec`, `uv run`, `python -m`, `mise exec -- …`) are seen through; a compound line is heavy when any part is; quoted strings and heredoc bodies are data. `heavy_extra` patterns are matched against each simple command with its env assignments and wrappers removed. `agent-exec` is the only classifier: the hooks ask it.
- **Who is admitted.** A worker's foreground Bash command (a subagent's, or the main thread of a session a wave hosts in a task worktree) waits in the orchestra hook before the tool starts, so the wait never counts against the Bash timeout. The instructor's own main thread and `run_in_background` commands are never held. Every check command (§4) takes one slot too, and so does agent-exec's own worktree I/O: the checkout of every orchestra worktree (`git gtr new`, `git worktree add`, the Orca host's `orca worktree create`), the `git worktree remove` of a refresh, and the delete of a removed tree (held until its background `rm -rf` exits, or until agent-exec exits). Only that one step holds the slot — never carry, adoption or anything that could take a slot itself.
- **Leases.** A held slot is a lease file in `<state dir>/slots/heavy/` (machine-wide, not per wave) naming the owner pid, start and expiry. The hook's owner is its Claude Code process; a lease whose owner died, or whose TTL (15 min for a hook command; the check's timeout plus a minute for a check; an hour for a worktree step) passed, is reclaimed by the next acquirer. A hook waits at most 9 minutes, then runs the command unadmitted; a worktree step waits as long as it takes; any failure of the admission itself also runs the command (or the step) unadmitted.
- **Seeing it.** Each wait is a trace record (kind `lock`, op `slot heavy`, detail the command's op): `agent-exec trace report --kind lock`. `agent-exec slot status [--json]` lists the pool size and the held leases (pid, age, op).

CLI (used by the hooks): **`agent-exec slot acquire --command CMD [--pool heavy] [--owner-pid N] [--ttl 15m] [--max-wait D]`** prints `{"admitted":false}` at once for a command that is not heavy (or with the pool off), else blocks until a slot is held and prints `{"admitted":true,"token":"…","waited_ms":N,"op":"…"}` (`admitted:false` with a `reason` when `--max-wait` passed or the owner died first). `--owner-pid` defaults to the parent process. **`agent-exec slot release TOKEN`** frees it (an unknown or released token is fine, exit 0). **`agent-exec slot status [--json]`**.
