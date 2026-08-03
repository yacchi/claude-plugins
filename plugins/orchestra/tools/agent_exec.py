# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
"""agent-exec: generic, TTY-agnostic external-executor wrapper.

Lets orchestra dispatch Copilot (and future CLIs) without requiring users to
hand-configure env vars in settings.json. The wrapper injects any required
environment/flags for the target executor internally, so the only Claude Code
permission rule needed is: Bash(agent-exec:*)

Usage:
  agent-exec install            interactive installer
  agent-exec list                print known profile names
  agent-exec config [--json]     print resolved orchestra config
  agent-exec run <profile> ...   normalized, config-driven dispatch
  agent-exec doctor [--json|--text]  structured readiness report
  agent-exec route --class <cls> [--archetype A] [--exhausted a,b] [--json|--text]
                                  resolve a class to a concrete executor
  agent-exec dispatch --class <cls> --prompt-file F --workdir W ...
                                  one-call resolve + dispatch
  agent-exec <profile> [args..]  dispatch to the profile's executor
  agent-exec -h | --help          show this help
"""

import copy
import glob
import json
import os
import platform
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import time
from datetime import datetime, timezone
from pathlib import Path

import yaml

PROFILES = {
    "copilot": {
        "exec": "copilot",
        "env": {},
        "mode": "headless",
        "inject_args": ["--disable-builtin-mcps"],
    },
}

# Registry of executors agent-exec knows about even when they have no CLI
# passthrough profile (e.g. codex, which is dispatch: agent and never gets
# exec'd directly by agent-exec). Used so `doctor` can report presence/
# absence for every known executor, not just ones enabled with dispatch=cli.
KNOWN_EXECUTORS = {
    "copilot": {"binary": "copilot", "default_dispatch": "cli"},
    "codex": {"binary": "codex", "default_dispatch": "agent"},
}

DEFAULTS = {
    "tiers": {
        "light": "haiku",
        "standard": "sonnet",
        "deep": "opus",
        "review": "sonnet",
    },
    # Enabled by default: `route`/`resolve_route` hard-gates every non-claude
    # candidate on real availability (doctor's `ready.<x>.ok`), so a machine
    # without the Copilot CLI or Codex agent still resolves to claude
    # automatically. Defaulting to enabled here only changes behavior on a
    # machine where the executor is actually ready.
    "external_executors": {
        "copilot": {
            "enabled": True,
            "dispatch": "cli",
            "classes": ["light", "standard"],
            "class_policy": {
                "light": {"model": "gpt-5.6-luna", "effort": "medium"},
                "standard": {"model": "gpt-5.6-luna", "effort": "medium"},
            },
        },
        "codex": {
            "enabled": True,
            "dispatch": "agent",
            "agent_type": "codex:codex-rescue",
            "classes": ["standard", "deep", "review"],
            "class_policy": {
                "standard": {"model": "gpt-5.6-luna", "effort": "medium"},
                "deep": {"model": "gpt-5.6-sol", "effort": "xhigh"},
                "review": {"model": "gpt-5.6-sol", "effort": "low"},
            },
            # Escalate a nominally light/standard-class task to `deep` when it
            # needs deep traversal of a large repo — Luna's long-context
            # recall is measurably weak.
            "long_context_escalation": {
                "when": (
                    "task requires deep traversal of a large repo "
                    "(Luna's long-context recall is weak)"
                ),
                "class": "deep",
            },
        },
    },
    # Ordered executor preference per class/role (and, for the implementation
    # classes, per task archetype). See `resolve_route()` for the algorithm
    # and the `run` skill's SKILL.md §9 for the human-readable version this
    # mirrors. Omit `priority.<class>` entirely (not possible via the
    # defaults, but possible via a user override that sets `priority: {}`) to
    # fall back to the legacy `classes`-membership scan.
    "priority": {
        "light": {
            "investigation": ["copilot", "claude"],
            "default": ["copilot", "claude"],
        },
        "standard": {
            "default": ["copilot", "claude", "codex"],
        },
        "deep": {
            "default": ["claude", "codex"],
        },
        "review": {
            "default": ["claude"],
        },
        "independent-review": {
            "default": ["codex"],
        },
    },
    # "off": route/dispatch only ever advise; nothing nudges a light-class
    # task away from Claude. "block": the PreToolUse hook
    # (hooks/enforce-router.sh) nudges a generic Haiku implementation
    # subagent toward `agent-exec dispatch` instead, at most once per
    # session, with documented escape hatches. Consumed by that hook (and by
    # `agent-exec config`/`doctor`'s reflection of resolved config), not
    # enforced by agent-exec's own subcommands.
    # "worker_vcs" guards the other direction and therefore defaults to
    # "block": a subagent must not run destructive VCS commands
    # (checkout --/restore/reset --hard/clean -f/stash) against the user's
    # shared working tree. Consumed by hooks/guard-worker-vcs.sh. Set to
    # "off" to disable that guard entirely.
    # "turn_edits" is the turn-size tripwire threshold consumed by
    # hooks/count-turn-edits.sh: once the main thread has hand-edited this
    # many files inside one turn, the hook asks it once to re-classify into
    # the orchestrated lane. Set to "off" to disable.
    "enforcement": {
        "light_class": "off",
        "worker_vcs": "block",
        "turn_edits": 8,
    },
    # auth and nonzero-exit are transient, not resource exhaustion, so 0 means
    # "no cooldown" for those reasons.
    "cooldown": {
        "enabled": True,
        "path": "~/.claude/orchestra/executor-state.json",
        "seconds": {
            "rate-limit": 900,
            "quota": 3600,
            "credits": 3600,
            "auth": 0,
            "nonzero-exit": 0,
        },
    },
    "telemetry": {
        "enabled": False,
        "dir": "~/.claude/orchestra/telemetry",
    },
}

USAGE = """\
agent-exec: dispatch external agent CLIs with their required env pre-injected.

Usage:
  agent-exec install            interactive installer (adds a shim to PATH)
  agent-exec list                print known profile names, one per line
  agent-exec config [--json]     print resolved orchestra config (deep-merged
                                  defaults + ~/.claude/orchestra.yaml +
                                  ./.claude/orchestra.yaml +
                                  ./.claude/orchestra.local.yaml)
  agent-exec run <profile> --model M --effort E --workdir W --prompt-file F
                                  [--resume SID] [--output FMT] [--cls CLASS]
                                  [--capture]
                                  normalized, config-driven dispatch. With
                                  --capture, runs the executor as a subprocess,
                                  captures + parses its JSONL output, and
                                  prints a normalized result JSON to stdout
                                  (status/answer/session_id/reason/exit_code)
                                  instead of exec-replacing the process. Also
                                  self-logs an anonymized "dispatch" telemetry
                                  record (see `agent-exec telemetry`) if
                                  telemetry is enabled in config.
  agent-exec doctor [--json|--text]
                                  emit a structured readiness report covering
                                  the shim, uv, config, executors, the
                                  Bash(agent-exec:*) permission rule, an
                                  overall ready/missing verdict per executor,
                                  and a `route` preview (the resolved
                                  executor for each of light/standard/deep/
                                  review) (--json is the default)
  agent-exec route --class <light|standard|deep|review|independent-review>
                  [--archetype default|investigation] [--exhausted a,b,c]
                  [--no-cooldown]
                  [--json|--text]
                                  resolve a capability class (+ optional task
                                  archetype) to a concrete executor by
                                  walking config `priority` and gating each
                                  candidate on doctor-report readiness (see
                                  `resolve_route`); prints the resolved route
                                  (--json is the default). `--exhausted`
                                  drops the given comma-separated executor
                                  names as already-tried/unavailable.
  agent-exec dispatch --class <cls> [--archetype A] [--exhausted a,b]
                  [--no-cooldown] [--isolate auto|always|never] [--task ID]
                  --prompt-file F --workdir W [--resume SID] [--capture]
                                  one-call resolve + dispatch: resolves the
                                  route, then runs it. If the winning
                                  executor is `dispatch: cli` (e.g. copilot),
                                  runs it exactly as `run --capture` does and
                                  prints its normalized result plus
                                  executor/model/effort/route. If the winner
                                  is `dispatch: agent` (e.g. codex) or the
                                  `claude` fallback, agent-exec cannot spawn
                                  subagents itself, so it prints
                                  {{"status":"delegate", executor, model,
                                  effort, agent_type, route}} for the caller
                                  to make the Agent-tool call itself. If
                                  nothing resolves, prints
                                  {{"status":"unroutable", route}}. Exits 0 in
                                  all of these cases — status/reason ride in
                                  the payload, same convention as `run
                                  --capture`. Also self-logs a "dispatch"
                                  telemetry record for the cli branch, same
                                  as `run --capture`.
                                  --isolate (default auto) runs the worker in
                                  its own git worktree when the tree is dirty,
                                  so a worker cannot destroy the user's
                                  uncommitted work; needs --task to name the
                                  worktree so retry rounds reuse one. Every
                                  result carries an `isolation` object saying
                                  which tree was used and why.
  agent-exec isolate create --task ID [--repo P] [--backend auto|gtr|git]
                  [--no-carry]
                                  create (or return) that task's worktree,
                                  carrying the user's uncommitted work and
                                  gitignored dependency dirs (node_modules,
                                  .venv, ...) into it. Uses git-worktree-runner
                                  when installed so the user's own gtr copy
                                  patterns and postCreate hooks apply; gtr
                                  config is read, never written.
  agent-exec isolate diff --task ID [--repo P] [--names-only]
                                  what the worker changed, as patch + file
                                  list, measured from the worktree's baseline
                                  so the user's own work never shows up.
  agent-exec isolate remove --task ID [--repo P] [--force]
                                  delete the worktree and branch; refuses while
                                  uncollected changes remain unless --force.
  agent-exec isolate list [--repo P]
                                  orchestra-created worktrees in this repo.
  agent-exec isolate should [--repo P] [--mode auto|always|never]
                                  the isolation verdict for this tree, without
                                  creating anything.
  agent-exec cooldown [--json|--text]
                                  show active executor cooldowns
  agent-exec cooldown clear [<executor>]
                                  clear all, or one, persisted cooldown
  agent-exec telemetry record (--json STR | --file F)
                                  append an anonymized telemetry record
                                  (allowlist-sanitized; enabled/disabled via
                                  config's telemetry.enabled)
  agent-exec telemetry show [--json]
                                  print recorded telemetry (raw JSONL with
                                  --json, else a short aggregate)
  agent-exec telemetry archive [--out FILE]
                                  tar.gz the telemetry directory to FILE
  agent-exec telemetry clear     delete all recorded telemetry
  agent-exec telemetry enable [--scope user|project|local]
                                  surgically set telemetry.enabled: true in
                                  the scope's orchestra.yaml (default: user),
                                  preserving all other content/comments
  agent-exec telemetry disable [--scope user|project|local]
                                  same, but sets telemetry.enabled: false
  agent-exec <profile> [args...] dispatch: inject profile env, exec the
                                  target CLI with args passed through verbatim
  agent-exec -h | --help          show this help

Known profiles: {profiles}
""".format(profiles=", ".join(PROFILES))


def print_usage(stream=sys.stdout):
    stream.write(USAGE)


def cmd_list():
    for name in PROFILES:
        print(name)
    return 0


def cmd_dispatch(profile_name, args):
    profile = PROFILES.get(profile_name)
    if profile is None:
        sys.stderr.write(
            "agent-exec: unknown profile or command: %s (run 'agent-exec list')\n"
            % profile_name
        )
        return 2

    mode = profile["mode"]
    if mode != "headless":
        sys.stderr.write(
            "agent-exec: profile %s uses mode=%s, not yet implemented\n"
            % (profile_name, mode)
        )
        return 3

    exec_name = profile["exec"]
    profile_env = profile["env"]
    inject_args = profile.get("inject_args", [])

    final_args = list(args)
    for inj in inject_args:
        if inj not in final_args:
            final_args = [inj] + final_args

    if os.environ.get("AGENT_EXEC_DRYRUN"):
        print("PROFILE: %s" % profile_name)
        print("MODE: %s" % mode)
        if profile_env:
            print(
                "ENV: " + ", ".join("%s=%s" % (k, v) for k, v in profile_env.items())
            )
        else:
            print("ENV: (none)")
        print("EXEC: %s %s" % (exec_name, " ".join(final_args)))
        return 0

    resolved = shutil.which(exec_name)
    if resolved is None:
        sys.stderr.write(
            "agent-exec: executor '%s' not found on PATH\n" % exec_name
        )
        return 127

    env = dict(os.environ)
    env.update(profile_env)
    os.execvpe(exec_name, [exec_name] + final_args, env)  # never returns


# --- install subcommand -----------------------------------------------------


def _prompt(prompt_text, default=None):
    try:
        raw = input(prompt_text)
    except EOFError:
        return default
    raw = raw.strip()
    if raw == "":
        return default
    return raw


def _resolve_bootstrap():
    candidates = sorted(
        glob.glob(
            os.path.expanduser(
                "~/.claude/plugins/marketplaces/*/plugins/orchestra/tools/agent-exec"
            )
        )
    )

    if len(candidates) == 1:
        bootstrap = candidates[0]
    elif len(candidates) > 1:
        this_file = os.path.abspath(__file__)
        preferred = None
        marker = "/marketplaces/"
        idx = this_file.find(marker)
        if idx != -1:
            rest = this_file[idx + len(marker):]
            mp_name = rest.split("/", 1)[0]
            needle = "/marketplaces/%s/" % mp_name
            for c in candidates:
                if needle in c:
                    preferred = c
                    break
        if preferred is not None:
            bootstrap = preferred
        else:
            print("Multiple orchestra plugin installs found:")
            for i, c in enumerate(candidates, start=1):
                print("  %d) %s" % (i, c))
            choice = _prompt("Choose one [1]: ", default="1")
            try:
                i = int(choice)
                if i < 1 or i > len(candidates):
                    raise ValueError
            except ValueError:
                i = 1
            bootstrap = candidates[i - 1]
    else:
        this_dir = os.path.dirname(os.path.abspath(__file__))
        bootstrap = os.path.join(this_dir, "agent-exec")
        if "/cache/" in bootstrap:
            print(
                "WARNING: no marketplace clone found; falling back to the "
                "version-stamped cache path:\n  %s\n"
                "This shim will BREAK on the next plugin update. Re-run "
                "'agent-exec install' from the marketplace clone once "
                "available." % bootstrap
            )

    if not os.path.isfile(bootstrap):
        sys.stderr.write(
            "agent-exec: install: resolved bootstrap does not exist: %s\n"
            % bootstrap
        )
        return None

    return os.path.abspath(bootstrap)


def _choose_bindir():
    path_dirs = os.environ.get("PATH", "").split(os.pathsep)
    path_dirs_abs = {os.path.abspath(p) for p in path_dirs if p}

    raw_candidates = [
        "~/.local/bin",
        "/usr/local/bin",
        "~/bin",
        "~/.claude/bin",
    ]
    candidates = [os.path.expanduser(c) for c in raw_candidates]

    on_path = [os.path.abspath(c) in path_dirs_abs for c in candidates]

    default_idx = 0
    for i, c in enumerate(candidates):
        if on_path[i] and os.path.abspath(c) == os.path.abspath(
            os.path.expanduser("~/.local/bin")
        ):
            default_idx = i
            break
    else:
        for i in range(len(candidates)):
            if on_path[i]:
                default_idx = i
                break

    print("Choose an install directory for the agent-exec shim:")
    for i, c in enumerate(candidates, start=1):
        tag = "(on PATH)" if on_path[i - 1] else "(NOT on PATH)"
        marker = " [default]" if (i - 1) == default_idx else ""
        print("  %d) %s %s%s" % (i, c, tag, marker))
    print("  %d) enter a custom absolute path" % (len(candidates) + 1))

    choice = _prompt(
        "Choice [%d]: " % (default_idx + 1), default=str(default_idx + 1)
    )

    try:
        i = int(choice)
    except ValueError:
        i = default_idx + 1

    if i == len(candidates) + 1:
        custom = _prompt("Custom absolute path: ", default=None)
        if not custom:
            return candidates[default_idx]
        return os.path.expanduser(custom)

    if 1 <= i <= len(candidates):
        return candidates[i - 1]

    return candidates[default_idx]


def cmd_install():
    bootstrap = _resolve_bootstrap()
    if bootstrap is None:
        return 1

    bindir = _choose_bindir()
    os.makedirs(bindir, exist_ok=True)
    target = os.path.join(bindir, "agent-exec")

    if os.path.exists(target):
        answer = _prompt(
            "%s already exists. Overwrite? (y/N): " % target, default="n"
        )
        if answer.lower() not in ("y", "yes"):
            print("Aborted: not overwriting existing %s" % target)
            return 1

    shim_content = '#!/bin/sh\nexec "%s" "$@"\n' % bootstrap
    with open(target, "w") as f:
        f.write(shim_content)
    os.chmod(target, 0o755)

    print("installed %s -> %s" % (target, bootstrap))

    path_dirs_abs = {
        os.path.abspath(p) for p in os.environ.get("PATH", "").split(os.pathsep) if p
    }
    if os.path.abspath(bindir) not in path_dirs_abs:
        print("")
        print("WARNING: %s is not on your PATH." % bindir)
        print("Add this to your shell rc file:")
        print('  export PATH="%s:$PATH"' % bindir)

    print("")
    print("Required: add \"Bash(agent-exec:*)\" to permissions.allow in")
    print("  .claude/settings.json (project) or ~/.claude/settings.json (user)")
    print("  You can also run /permissions in Claude Code to add it.")
    print("")
    print("Quick test: agent-exec list")

    return 0


# --- config subcommand ------------------------------------------------------


def _deep_merge(base, override):
    """Recursively merge override onto base. Only dict+dict recurses; every
    other type (including lists) is replaced wholesale by override. An
    explicit None in override is a value (replaces), not "reset to default"."""
    if isinstance(base, dict) and isinstance(override, dict):
        result = dict(base)
        for k, v in override.items():
            if k in result:
                result[k] = _deep_merge(result[k], v)
            else:
                result[k] = v
        return result
    return override


def _config_layer_path(directory):
    """Return the path to use for an orchestra.yaml/.yml layer in `directory`,
    preferring .yaml over .yml, or None if neither exists."""
    yaml_path = os.path.join(directory, "orchestra.yaml")
    yml_path = os.path.join(directory, "orchestra.yml")
    if os.path.isfile(yaml_path):
        return yaml_path
    if os.path.isfile(yml_path):
        return yml_path
    return None


_TELEMETRY_TOP_LEVEL_RE = re.compile(r"^telemetry:\s*(#.*)?$")
_TELEMETRY_ENABLED_CHILD_RE = re.compile(r"^(\s+enabled\s*:\s*)(\S+)(.*)$")
_LEADING_WS_RE = re.compile(r"^(\s+)")

_TELEMETRY_STUB_COMMENT = (
    "# orchestra config\n"
    "# telemetry is opt-in and anonymized\n"
)
_TELEMETRY_BLOCK_TEMPLATE = (
    "telemetry:\n"
    "  enabled: %s  # opt-in, anonymized (see orchestra:run SKILL §10)\n"
)


def set_telemetry_enabled_in_text(text, value):
    """Pure helper: given the current content of an orchestra.yaml layer file
    (or None if the file does not exist yet), return the new file content
    with telemetry.enabled surgically set to `value` (bool).

    Does a LINE-ORIENTED edit rather than a yaml.safe_load+dump round-trip,
    so every other line, comment, key, and formatting detail is preserved
    byte-for-byte. Only a zero-indent `telemetry:` key is treated as the
    block anchor; occurrences indented under another key, or inside
    comments, are ignored."""
    bool_str = "true" if value else "false"

    if text is None:
        return _TELEMETRY_STUB_COMMENT + "\n" + (_TELEMETRY_BLOCK_TEMPLATE % bool_str)

    lines = text.splitlines(keepends=True)

    telemetry_idx = None
    for i, line in enumerate(lines):
        stripped = line.rstrip("\r\n")
        if _TELEMETRY_TOP_LEVEL_RE.match(stripped):
            telemetry_idx = i
            break

    if telemetry_idx is None:
        new_text = text
        if new_text and not new_text.endswith("\n"):
            new_text += "\n"
        new_text += "\n" + (_TELEMETRY_BLOCK_TEMPLATE % bool_str)
        return new_text

    # Block runs from just after the telemetry: line until the next
    # zero-indent, non-blank line (or EOF).
    end_idx = len(lines)
    for j in range(telemetry_idx + 1, len(lines)):
        raw = lines[j].rstrip("\r\n")
        if raw.strip() == "":
            continue
        if raw[0] not in (" ", "\t"):
            end_idx = j
            break

    enabled_idx = None
    for j in range(telemetry_idx + 1, end_idx):
        raw = lines[j].rstrip("\r\n")
        if _TELEMETRY_ENABLED_CHILD_RE.match(raw):
            enabled_idx = j
            break

    if enabled_idx is not None:
        line = lines[enabled_idx]
        if line.endswith("\r\n"):
            body, eol = line[:-2], "\r\n"
        elif line.endswith("\n"):
            body, eol = line[:-1], "\n"
        else:
            body, eol = line, ""
        m = _TELEMETRY_ENABLED_CHILD_RE.match(body)
        lines[enabled_idx] = m.group(1) + bool_str + m.group(3) + eol
        return "".join(lines)

    child_indent = "  "
    for j in range(telemetry_idx + 1, end_idx):
        raw = lines[j].rstrip("\r\n")
        if raw.strip() == "":
            continue
        m2 = _LEADING_WS_RE.match(raw)
        if m2:
            child_indent = m2.group(1)
        break

    insert_line = "%senabled: %s\n" % (child_indent, bool_str)
    lines.insert(telemetry_idx + 1, insert_line)
    return "".join(lines)


def _telemetry_scope_target_path(scope):
    """Return the on-disk path to edit for `agent-exec telemetry
    enable|disable --scope <scope>`. Prefers .yaml; falls back to an
    existing .yml only if the .yaml variant is absent. If neither exists,
    returns the .yaml path (a new file will be created there)."""
    if scope == "user":
        directory = os.path.expanduser("~/.claude")
        yaml_name, yml_name = "orchestra.yaml", "orchestra.yml"
    elif scope == "project":
        directory = os.path.join(".", ".claude")
        yaml_name, yml_name = "orchestra.yaml", "orchestra.yml"
    elif scope == "local":
        directory = os.path.join(".", ".claude")
        yaml_name, yml_name = "orchestra.local.yaml", "orchestra.local.yml"
    else:
        raise ValueError("unknown scope: %s" % scope)

    yaml_path = os.path.join(directory, yaml_name)
    yml_path = os.path.join(directory, yml_name)
    if os.path.isfile(yaml_path):
        return yaml_path
    if os.path.isfile(yml_path):
        return yml_path
    return yaml_path


def _cmd_telemetry_toggle(value, args):
    scope = "user"
    i = 0
    while i < len(args):
        tok = args[i]
        if tok == "--scope":
            if i + 1 >= len(args):
                sys.stderr.write("agent-exec: telemetry: missing value for --scope\n")
                return 2
            scope = args[i + 1]
            if scope not in ("user", "project", "local"):
                sys.stderr.write(
                    "agent-exec: telemetry: invalid --scope: %s\n" % scope
                )
                return 2
            i += 2
        else:
            sys.stderr.write("agent-exec: telemetry: unknown option: %s\n" % tok)
            return 2

    path = _telemetry_scope_target_path(scope)

    text = None
    if os.path.isfile(path):
        with open(path, "r", encoding="utf-8") as f:
            text = f.read()
    else:
        os.makedirs(os.path.dirname(path), exist_ok=True)

    new_text = set_telemetry_enabled_in_text(text, value)

    with open(path, "w", encoding="utf-8") as f:
        f.write(new_text)

    print(
        "telemetry %s -> %s"
        % ("enabled" if value else "disabled", os.path.abspath(path))
    )
    return 0


def _ordered_layer_paths():
    """Ordered, de-duplicated orchestra config layer files (user, project,
    project-local), lowest->highest precedence. When two logical layers resolve
    to the same physical file (e.g. cwd is $HOME, so ./.claude == ~/.claude),
    the file is included once, at its first (lower-precedence) position."""
    raw = []
    for directory in (os.path.expanduser("~/.claude"),
                      os.path.join(".", ".claude")):
        p = _config_layer_path(directory)
        if p is not None:
            raw.append(p)
    for local_name in ("orchestra.local.yaml", "orchestra.local.yml"):
        p = os.path.join(".", ".claude", local_name)
        if os.path.isfile(p):
            raw.append(p)
            break
    deduped = []
    seen = set()
    for p in raw:
        key = os.path.realpath(p)
        if key in seen:
            continue
        seen.add(key)
        deduped.append(p)
    return deduped


def _load_yaml_layer(path):
    """Load a YAML layer file. Returns (dict_or_None, error_message_or_None)."""
    try:
        with open(path, "r", encoding="utf-8") as f:
            data = yaml.safe_load(f)
    except yaml.YAMLError as e:
        return None, str(e)
    except OSError as e:
        return None, str(e)
    if data is None:
        return {}, None
    return data, None


_LEGACY_TIER_KEYS = ("worker", "hard_worker", "verifier")


def _detect_config_warnings():
    """Detect legacy config artifacts (pre-0.4 vocabulary, legacy
    orchestra.json) across the same deduped layer files used for merge, plus
    the legacy JSON locations. Never raises; malformed/unreadable files are
    skipped."""
    warnings = []

    # legacy JSON: orchestra.json with no sibling orchestra.yaml/.yml
    json_seen = set()
    for directory in (os.path.expanduser("~/.claude"), os.path.join(".", ".claude")):
        jpath = os.path.join(directory, "orchestra.json")
        if not os.path.isfile(jpath):
            continue
        key = os.path.realpath(jpath)
        if key in json_seen:
            continue
        json_seen.add(key)
        if _config_layer_path(directory) is None:
            warnings.append({"type": "legacy_json", "file": os.path.abspath(jpath)})

    # pre-0.4 vocabulary in any layer file (raw yaml, independent of merge)
    for path in _ordered_layer_paths():
        try:
            with open(path, "r", encoding="utf-8") as f:
                data = yaml.safe_load(f)
        except (yaml.YAMLError, OSError):
            continue
        if not isinstance(data, dict):
            continue

        keys_found = []
        if "role_priority" in data:
            keys_found.append("role_priority")

        external = data.get("external_executors")
        if isinstance(external, dict):
            for name, cfg in external.items():
                if not isinstance(cfg, dict):
                    continue
                if "model_policy" in cfg:
                    keys_found.append("external_executors.%s.model_policy" % name)
                if "roles" in cfg:
                    keys_found.append("external_executors.%s.roles" % name)

        tiers = data.get("tiers")
        if isinstance(tiers, dict):
            for tier_key in _LEGACY_TIER_KEYS:
                if tier_key in tiers:
                    keys_found.append("tiers.%s" % tier_key)

        if keys_found:
            warnings.append(
                {
                    "type": "legacy_vocab",
                    "file": os.path.abspath(path),
                    "keys": keys_found,
                }
            )

    return warnings


def _builtin_exec_for(name):
    profile = PROFILES.get(name)
    if profile is None:
        return None
    return profile["exec"]


def resolve_config():
    """Resolve the 4-layer config. Returns (resolved_dict_or_None,
    error_message_or_None)."""
    resolved = copy.deepcopy(DEFAULTS)

    for path in _ordered_layer_paths():
        data, err = _load_yaml_layer(path)
        if err is not None:
            return None, "agent-exec: invalid YAML in %s: %s" % (path, err)
        resolved = _deep_merge(resolved, data)

    external = resolved.get("external_executors") or {}
    if isinstance(external, dict):
        for name, cfg in external.items():
            if not isinstance(cfg, dict):
                continue
            if cfg.get("enabled") is True and cfg.get("dispatch") == "cli":
                builtin_exec = _builtin_exec_for(name)
                if builtin_exec is None:
                    cfg["available"] = None
                else:
                    cfg["available"] = shutil.which(builtin_exec) is not None

    # Normalize enforcement.light_class to always be one of the two known
    # strings. YAML 1.1 (what yaml.safe_load implements) parses a bareword
    # `off`/`on`/`yes`/`no` as a bool, not a string -- so a config author
    # writing unquoted `off` gets Python `False` here, not `"off"`. Fail
    # safe: any non-"block" value (False, None, True, a stray bool from a
    # future on/yes/no typo, or any other unrecognized string) normalizes to
    # "off" rather than being silently treated as "block". An ambiguous
    # value must never silently mean "block".
    enforcement = resolved.get("enforcement")
    if not isinstance(enforcement, dict):
        enforcement = {}
        resolved["enforcement"] = enforcement
    if enforcement.get("light_class") != "block":
        enforcement["light_class"] = "off"
    # worker_vcs normalizes the opposite way: it protects the user's
    # uncommitted work, so an ambiguous value must leave the guard ON. Only an
    # explicit "off" -- as a string, or as the bareword YAML 1.1 loads as
    # False -- turns it off.
    worker_vcs = enforcement.get("worker_vcs")
    if worker_vcs is False or (isinstance(worker_vcs, str) and worker_vcs.strip().lower() == "off"):
        enforcement["worker_vcs"] = "off"
    else:
        enforcement["worker_vcs"] = "block"
    # turn_edits is either the string "off" or a positive int. Anything
    # unusable falls back to the default rather than disabling the tripwire
    # silently.
    turn_edits = enforcement.get("turn_edits")
    if turn_edits is False or (isinstance(turn_edits, str) and turn_edits.strip().lower() == "off"):
        enforcement["turn_edits"] = "off"
    elif isinstance(turn_edits, bool) or not isinstance(turn_edits, int) or turn_edits <= 0:
        enforcement["turn_edits"] = DEFAULTS["enforcement"]["turn_edits"]

    return resolved, None


def cmd_config(args):
    # --json is accepted (and is the default/only format for now).
    for a in args:
        if a not in ("--json",):
            sys.stderr.write("agent-exec: config: unknown option: %s\n" % a)
            return 2

    resolved, err = resolve_config()
    if err is not None:
        sys.stderr.write(err + "\n")
        return 1

    output = dict(resolved)
    output["warnings"] = _detect_config_warnings()
    print(json.dumps(output, indent=2, ensure_ascii=False))
    return 0


# --- telemetry -----------------------------------------------------------
#
# Crash-dump-style anonymized telemetry. Enforcement is by ALLOWLIST in code:
# sanitize_telemetry_record only ever copies a fixed, fully-enumerated set of
# keys whose values pass an exact-match / type check into a brand new dict.
# There is no code path that copies caller-supplied free text (prompts, task
# text, file names, paths, task ids, code, error message text) into a record.

_TELEMETRY_EVENTS = ("run_summary", "dispatch")
_TELEMETRY_LANES = ("express", "orchestrated")
_TELEMETRY_EXECUTORS = ("claude", "copilot", "codex")
_TELEMETRY_CLASSES = ("light", "standard", "deep", "review")
_TELEMETRY_STATUSES = ("ok", "unavailable")
_TELEMETRY_REASONS = (
    "quota",
    "rate-limit",
    "credits",
    "auth",
    "nonzero-exit",
    "error",
)
_TELEMETRY_VERSION_RE = re.compile(r"^\d+\.\d+\.\d+$")
_TELEMETRY_ROUND_KEY_RE = re.compile(r"^[1-9][0-9]*$")

_TELEMETRY_MAX_LINES = 10000


def _is_nonneg_int(v):
    # bool is a subclass of int in Python; explicitly reject it here so a
    # stray True/False is never mistaken for 0/1.
    return isinstance(v, int) and not isinstance(v, bool) and v >= 0


def _sanitize_count_dict(value, allowed_keys):
    if not isinstance(value, dict):
        return None
    out = {}
    for k, v in value.items():
        if k in allowed_keys and _is_nonneg_int(v):
            out[k] = v
    return out


def sanitize_telemetry_record(raw):
    """Pure function: raw (caller-supplied) dict -> a NEW dict containing
    ONLY allowlisted keys with valid values, or None if the record must be
    rejected outright (missing/invalid event).

    This is the sole enforcement point for the "no free text ever leaks"
    guarantee: every key copied out is named explicitly below, and every
    value is checked against an exact enum/type before being copied. Any key
    not listed here, and any value that fails its check, is silently
    dropped rather than passed through."""
    if not isinstance(raw, dict):
        return None

    event = raw.get("event")
    if event not in _TELEMETRY_EVENTS:
        return None

    out = {
        "event": event,
        "schema_version": 1,
        "ts": datetime.now(timezone.utc).isoformat(),
        "os": platform.system().lower(),
    }

    v = raw.get("orchestra_version")
    if isinstance(v, str) and _TELEMETRY_VERSION_RE.match(v):
        out["orchestra_version"] = v

    v = raw.get("lane")
    if v in _TELEMETRY_LANES:
        out["lane"] = v

    v = raw.get("executor")
    if v in _TELEMETRY_EXECUTORS:
        out["executor"] = v

    v = raw.get("cls")
    if v in _TELEMETRY_CLASSES:
        out["cls"] = v

    v = raw.get("status")
    if v in _TELEMETRY_STATUSES:
        out["status"] = v

    v = raw.get("reason")
    if v is not None and v in _TELEMETRY_REASONS:
        out["reason"] = v

    if "resumed" in raw:
        v = raw.get("resumed")
        if isinstance(v, bool):
            out["resumed"] = v

    for key in ("task_count", "pass", "fail", "exhausted", "fallbacks"):
        if key in raw and _is_nonneg_int(raw.get(key)):
            out[key] = raw[key]

    if "classes" in raw:
        sanitized = _sanitize_count_dict(raw.get("classes"), _TELEMETRY_CLASSES)
        if sanitized is not None:
            out["classes"] = sanitized

    if "executors_used" in raw:
        sanitized = _sanitize_count_dict(
            raw.get("executors_used"), _TELEMETRY_EXECUTORS
        )
        if sanitized is not None:
            out["executors_used"] = sanitized

    if "rounds" in raw:
        value = raw.get("rounds")
        if isinstance(value, dict):
            sanitized = {}
            for k, v in value.items():
                if isinstance(k, str) and _TELEMETRY_ROUND_KEY_RE.match(k) and _is_nonneg_int(v):
                    sanitized[k] = v
            out["rounds"] = sanitized

    if "external_enabled" in raw:
        value = raw.get("external_enabled")
        if isinstance(value, dict):
            sanitized = {}
            for k, v in value.items():
                if k in ("copilot", "codex") and isinstance(v, bool):
                    sanitized[k] = v
            out["external_enabled"] = sanitized

    return out


def _telemetry_dir_from_cfg(cfg):
    telemetry_cfg = (cfg or {}).get("telemetry")
    if not isinstance(telemetry_cfg, dict):
        telemetry_cfg = {}
    directory = telemetry_cfg.get("dir") or DEFAULTS["telemetry"]["dir"]
    return os.path.expanduser(directory)


def _telemetry_enabled(cfg):
    telemetry_cfg = (cfg or {}).get("telemetry")
    if not isinstance(telemetry_cfg, dict):
        return False
    return telemetry_cfg.get("enabled") is True


def telemetry_append(record, cfg):
    """Sanitize + append a telemetry record as one compact JSON line, subject
    to a hard size cap. Never raises to the caller: any failure (disk full,
    permission error, bad input) is swallowed after being sanitized away."""
    try:
        if not _telemetry_enabled(cfg):
            return
        sanitized = sanitize_telemetry_record(record)
        if sanitized is None:
            return

        directory = _telemetry_dir_from_cfg(cfg)
        os.makedirs(directory, exist_ok=True)
        path = os.path.join(directory, "records.jsonl")

        with open(path, "a", encoding="utf-8") as f:
            f.write(json.dumps(sanitized, ensure_ascii=False) + "\n")

        _telemetry_enforce_cap(path)
    except Exception:
        # Telemetry must never break the caller.
        pass


def _telemetry_enforce_cap(path):
    try:
        with open(path, "r", encoding="utf-8") as f:
            lines = f.readlines()
    except OSError:
        return
    if len(lines) > _TELEMETRY_MAX_LINES:
        keep = lines[-_TELEMETRY_MAX_LINES:]
        with open(path, "w", encoding="utf-8") as f:
            f.writelines(keep)


def build_dispatch_record(profile_name, result, resume, cls):
    """Pure helper: build the (pre-sanitize) telemetry record for a single
    `run --capture` dispatch. `result` is the dict returned by
    parse_copilot_jsonl; only its status/reason fields (already
    enum-constrained) are used — never `answer` or any other field that
    could contain free text."""
    record = {
        "event": "dispatch",
        "executor": profile_name,
        "status": result.get("status"),
        "reason": result.get("reason"),
        "resumed": resume is not None,
    }
    if cls is not None:
        record["cls"] = cls
    return record


def _read_telemetry_lines(path):
    if not os.path.isfile(path):
        return []
    records = []
    with open(path, "r", encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            try:
                records.append(json.loads(line))
            except (ValueError, TypeError):
                continue
    return records


def cmd_telemetry(args):
    if len(args) == 0:
        sys.stderr.write("agent-exec: telemetry: missing subcommand\n")
        return 2

    sub = args[0]
    rest = args[1:]

    if sub == "enable":
        return _cmd_telemetry_toggle(True, rest)
    if sub == "disable":
        return _cmd_telemetry_toggle(False, rest)

    resolved, err = resolve_config()
    cfg = resolved if err is None and isinstance(resolved, dict) else DEFAULTS
    directory = _telemetry_dir_from_cfg(cfg)
    path = os.path.join(directory, "records.jsonl")

    if sub == "record":
        json_str = None
        file_path = None
        i = 0
        while i < len(rest):
            tok = rest[i]
            if tok == "--json":
                if i + 1 >= len(rest):
                    sys.stderr.write("agent-exec: telemetry: missing value for --json\n")
                    return 2
                json_str = rest[i + 1]
                i += 2
            elif tok == "--file":
                if i + 1 >= len(rest):
                    sys.stderr.write("agent-exec: telemetry: missing value for --file\n")
                    return 2
                file_path = rest[i + 1]
                i += 2
            else:
                sys.stderr.write("agent-exec: telemetry: unknown option: %s\n" % tok)
                return 2

        if json_str is None and file_path is None:
            sys.stderr.write("agent-exec: telemetry: record requires --json or --file\n")
            return 2

        try:
            if file_path is not None:
                with open(file_path, "r", encoding="utf-8") as f:
                    raw = json.load(f)
            else:
                raw = json.loads(json_str)
        except (OSError, ValueError):
            sys.stderr.write("agent-exec: telemetry: invalid JSON input\n")
            return 2

        if not isinstance(raw, dict) or raw.get("event") not in _TELEMETRY_EVENTS:
            sys.stderr.write("agent-exec: telemetry: record missing valid 'event'\n")
            return 2

        telemetry_append(raw, cfg)
        return 0

    if sub == "show":
        as_json = "--json" in rest
        records = _read_telemetry_lines(path)
        if as_json:
            for r in records:
                print(json.dumps(r, ensure_ascii=False))
            return 0

        by_event = {}
        by_status = {}
        by_reason = {}
        for r in records:
            if not isinstance(r, dict):
                continue
            ev = r.get("event")
            if ev is not None:
                by_event[ev] = by_event.get(ev, 0) + 1
            st = r.get("status")
            if st is not None:
                by_status[st] = by_status.get(st, 0) + 1
            rs = r.get("reason")
            if rs is not None:
                by_reason[rs] = by_reason.get(rs, 0) + 1

        print("total records: %d" % len(records))
        print("by event: %s" % json.dumps(by_event, ensure_ascii=False))
        print("by status: %s" % json.dumps(by_status, ensure_ascii=False))
        print("by reason: %s" % json.dumps(by_reason, ensure_ascii=False))
        return 0

    if sub == "archive":
        out_path = None
        i = 0
        while i < len(rest):
            tok = rest[i]
            if tok == "--out":
                if i + 1 >= len(rest):
                    sys.stderr.write("agent-exec: telemetry: missing value for --out\n")
                    return 2
                out_path = rest[i + 1]
                i += 2
            else:
                sys.stderr.write("agent-exec: telemetry: unknown option: %s\n" % tok)
                return 2

        if not os.path.isdir(directory):
            sys.stderr.write(
                "agent-exec: telemetry: directory does not exist: %s\n" % directory
            )
            return 1

        if out_path is None:
            out_path = os.path.join(
                os.path.dirname(os.path.abspath(directory)),
                "orchestra-telemetry-archive.tgz",
            )
        out_path = os.path.abspath(out_path)

        with tarfile.open(out_path, "w:gz") as tar:
            tar.add(directory, arcname=os.path.basename(directory.rstrip("/")))

        print(out_path)
        return 0

    if sub == "clear":
        try:
            os.remove(path)
        except OSError:
            pass
        return 0

    sys.stderr.write("agent-exec: telemetry: unknown subcommand: %s\n" % sub)
    return 2


def cmd_cooldown(args):
    fmt = "--json"
    clear = False
    executor = None
    for tok in args:
        if tok in ("--json", "--text"):
            fmt = tok
        elif tok == "clear":
            clear = True
        elif clear and executor is None:
            executor = tok
        else:
            sys.stderr.write("agent-exec: cooldown: unknown option: %s\n" % tok)
            return 2

    resolved, err = resolve_config()
    cfg = resolved if err is None and isinstance(resolved, dict) else DEFAULTS
    path = cooldown_state_path(cfg)
    if clear:
        if executor is None:
            try:
                os.remove(path)
            except OSError:
                pass
        else:
            state = load_cooldown_state(path)
            if executor in state:
                del state[executor]
                save_cooldown_state(path, state, time.time())
        return 0

    now = time.time()
    state = load_cooldown_state(path)
    active = active_cooldowns(state, now)
    if fmt == "--json":
        output = {
            "path": path,
            "now": now,
            "active": {
                name: {
                    "reason": entry.get("reason", "unknown"),
                    "until": entry.get("until"),
                    "remaining_seconds": int(entry.get("until") - now),
                }
                for name, entry in active.items()
            },
        }
        print(json.dumps(output, indent=2, ensure_ascii=False))
    else:
        print("cooldowns: %s" % path)
        if not active:
            print("none active")
        else:
            for name, entry in active.items():
                print(
                    "- %s: %s (until %s, %ss remaining)"
                    % (
                        name,
                        entry.get("reason", "unknown"),
                        entry.get("until"),
                        int(entry.get("until") - now),
                    )
                )
    return 0


# --- run subcommand ----------------------------------------------------------


_UNAVAILABLE_PATTERNS = [
    ("quota", re.compile(r"quota|usage limit|premium request", re.IGNORECASE)),
    ("rate-limit", re.compile(r"rate limit|rate-limit| 429", re.IGNORECASE)),
    ("credits", re.compile(r"insufficient|credit|out of credits", re.IGNORECASE)),
    (
        "auth",
        re.compile(
            r"unauthor|authenticat| 401|not logged in|login", re.IGNORECASE
        ),
    ),
]


def _is_error_bearing_event(event):
    """True iff a parsed JSONL event is a source of genuine executor-health
    signal, i.e. it is plausibly the CLI reporting on itself rather than
    relaying worker/tool text. This is the default-deny gate for the
    availability scan below: an event type we don't recognize is NOT
    scanned, on the theory that new/unknown event shapes are far more likely
    to be worker output (tool calls, tool results, reasoning, request
    echoes) than a new way for copilot to report quota/auth failure. The
    three cases let through:

      1. `type` contains "error" (e.g. "session.error", "auth.error") --
         copilot's own naming convention for CLI-level failures.
      2. a truthy top-level `error` field -- present on failure regardless
         of `type`.
      3. an explicit terminal/result event (`type == "result"`) whose own
         fields say the run did not succeed (`success`/`ok` is False, or an
         `isError` flag is truthy). A `result` event that doesn't say this
         (e.g. one only carrying a `sessionId`) is not error-bearing.

    Tool-call bodies, tool-result bodies, reasoning traces and request
    echoes are deliberately NOT matched here even though they can contain
    the literal words the patterns look for -- that text is the worker's
    prompt/output, not the executor reporting on itself."""
    etype = event.get("type")
    if isinstance(etype, str) and "error" in etype.lower():
        return True
    if event.get("error"):
        return True
    if etype == "result":
        if event.get("success") is False or event.get("ok") is False:
            return True
        if event.get("isError"):
            return True
    return False


def parse_copilot_jsonl(stdout_text, stderr_text, exit_code):
    """Pure parser: copilot JSONL stdout + stderr + subprocess exit code ->
    normalized result dict. No I/O, no subprocess calls; unit-testable with
    fixtures.

    The `_UNAVAILABLE_PATTERNS` scan is default-deny: only stderr, stdout
    lines that failed to parse as JSON, and parsed JSON events that
    `_is_error_bearing_event` recognizes as carrying genuine executor-health
    signal are in scope. `assistant.message` events, and every other event
    type/shape this parser doesn't specifically recognize as error-bearing
    (tool calls, tool results, reasoning, request echoes, unknown event
    types), are never scanned -- they are worker text, not executor
    health."""
    last_content = None
    last_final_content = None
    session_id = None
    # Lines eligible for the availability scan below: everything EXCEPT the
    # assistant's own answer and other worker-text-bearing events. A worker
    # asked to implement a rate limiter, fix a login flow, or price out
    # credits legitimately echoes those words back -- in its answer, in a
    # tool call it makes, or in a tool result it receives -- and scanning
    # any of that would flag a perfectly good run as `unavailable`, which
    # makes the caller mark the executor exhausted for the rest of the run.
    scannable_lines = []

    for line in (stdout_text or "").splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            event = json.loads(line)
        except (ValueError, TypeError):
            # Unparseable output is exactly where a plain-text quota/auth
            # error surfaces, so it stays in scope for the scan.
            scannable_lines.append(line)
            continue
        if not isinstance(event, dict):
            # A bare JSON scalar/list has no `type` to judge -- default-deny
            # means we don't guess at its meaning.
            continue

        etype = event.get("type")
        data = event.get("data")

        if _is_error_bearing_event(event):
            scannable_lines.append(line)

        if etype == "assistant.message" and isinstance(data, dict):
            content = data.get("content")
            if isinstance(content, str) and content != "":
                last_content = content
                if data.get("phase") == "final_answer":
                    last_final_content = content

        if etype == "result":
            sid = event.get("sessionId")
            if isinstance(sid, str) and sid != "":
                session_id = sid

    answer = last_final_content if last_final_content is not None else last_content

    combined_text = "\n".join(scannable_lines) + "\n" + (stderr_text or "")
    reason = None
    for candidate_reason, pattern in _UNAVAILABLE_PATTERNS:
        if pattern.search(combined_text):
            reason = candidate_reason
            break

    if reason is not None:
        status = "unavailable"
    elif exit_code != 0:
        status = "unavailable"
        reason = "nonzero-exit"
    else:
        status = "ok"

    return {
        "status": status,
        "answer": answer,
        "session_id": session_id,
        "reason": reason,
        "exit_code": exit_code,
    }


def _build_copilot_argv(exec_name, model, effort, workdir, prompt_value, resume, output_fmt):
    """Build the copilot CLI argv for a single headless invocation. Shared by
    `run`'s dry-run preview / exec-replace path and `_run_copilot_capture`
    (used by both `run --capture` and `dispatch`'s cli branch), so there is
    exactly one place that knows copilot's flag shape."""
    argv = [exec_name, "--disable-builtin-mcps"]
    if resume is not None:
        argv.append("--resume=%s" % resume)
    argv += [
        "-p", prompt_value,
        "--model", model,
        "--effort", effort,
        "--add-dir", workdir,
        "--output-format", output_fmt,
    ]
    return argv


def _run_copilot_capture(profile_name, model, effort, workdir, prompt_file, resume, output_fmt="json"):
    """Shared core of `run --capture` / `dispatch` (cli-dispatch route): build
    the executor's argv, run it as a subprocess, and parse its JSONL output.

    Returns (exit_code, result_or_None):
      - (127, None) if the executor binary is not on PATH (mirrors the
        existing 127 convention for a missing executor).
      - (0, result_dict) otherwise, where result_dict is the normalized
        dict from parse_copilot_jsonl. Its own "status" field (ok/
        unavailable), not this exit code, carries the executor's own
        availability signal.

    Does not print anything and does not touch telemetry — callers do both,
    since `dispatch` needs to fold in extra keys (executor/model/effort/
    route) before printing."""
    profile = PROFILES[profile_name]
    exec_name = profile["exec"]

    resolved = shutil.which(exec_name)
    if resolved is None:
        return 127, None

    with open(prompt_file, "r", encoding="utf-8") as f:
        prompt_text = f.read()

    argv = _build_copilot_argv(exec_name, model, effort, workdir, prompt_text, resume, output_fmt)
    env = dict(os.environ)

    proc = subprocess.run(
        argv,
        env=env,
        capture_output=True,
        text=True,
    )
    result = parse_copilot_jsonl(proc.stdout, proc.stderr, proc.returncode)
    return 0, result


def cmd_run(args):
    if len(args) == 0:
        sys.stderr.write("agent-exec: run: missing profile\n")
        return 2

    profile_name = args[0]
    rest = args[1:]

    profile = PROFILES.get(profile_name)
    if profile is None:
        sys.stderr.write(
            "agent-exec: run: unknown profile: %s (run 'agent-exec list')\n"
            % profile_name
        )
        return 2

    opts = {
        "--model": None,
        "--effort": None,
        "--workdir": None,
        "--prompt-file": None,
        "--resume": None,
        "--output": "json",
        "--cls": None,
    }
    capture = False

    i = 0
    while i < len(rest):
        tok = rest[i]
        if tok == "--capture":
            capture = True
            i += 1
        elif tok in opts:
            if i + 1 >= len(rest):
                sys.stderr.write("agent-exec: run: missing value for %s\n" % tok)
                return 2
            opts[tok] = rest[i + 1]
            i += 2
        else:
            sys.stderr.write("agent-exec: run: unknown option: %s\n" % tok)
            return 2

    if capture:
        opts["--output"] = "json"

    required = ["--model", "--effort", "--workdir", "--prompt-file"]
    for r in required:
        if opts[r] is None:
            sys.stderr.write("agent-exec: run: missing required option: %s\n" % r)
            return 2

    mode = profile["mode"]
    if mode != "headless":
        sys.stderr.write(
            "agent-exec: run: profile %s uses mode=%s, not yet implemented\n"
            % (profile_name, mode)
        )
        return 3

    exec_name = profile["exec"]
    model = opts["--model"]
    effort = opts["--effort"]
    workdir = opts["--workdir"]
    prompt_file = opts["--prompt-file"]
    resume = opts["--resume"]
    output_fmt = opts["--output"]
    cls = opts["--cls"]

    def build_argv(prompt_value):
        return _build_copilot_argv(exec_name, model, effort, workdir, prompt_value, resume, output_fmt)

    if os.environ.get("AGENT_EXEC_DRYRUN"):
        argv = build_argv("@%s" % prompt_file)
        print("PROFILE: %s" % profile_name)
        print("MODE: %s" % mode)
        print("ENV: (none)")
        print("EXEC: %s" % " ".join(argv))
        if capture:
            print(
                "CAPTURE: yes (would subprocess-run copilot and emit "
                "normalized JSON)"
            )
        return 0

    if capture:
        exit_code, result = _run_copilot_capture(
            profile_name, model, effort, workdir, prompt_file, resume, output_fmt
        )
        if result is None:
            sys.stderr.write(
                "agent-exec: executor '%s' not found on PATH\n" % exec_name
            )
            return exit_code
        print(json.dumps(result, ensure_ascii=False))
        try:
            telemetry_cfg, telemetry_err = resolve_config()
            if telemetry_err is not None or not isinstance(telemetry_cfg, dict):
                telemetry_cfg = DEFAULTS
            if result.get("status") == "unavailable":
                record_unavailable_cooldown(
                    telemetry_cfg,
                    profile_name,
                    result.get("reason"),
                    time.time(),
                    exit_code=result.get("exit_code"),
                    answer=result.get("answer"),
                )
            record = build_dispatch_record(profile_name, result, resume, cls)
            telemetry_append(record, telemetry_cfg)
        except Exception:
            # Telemetry must never break dispatch.
            pass
        return 0

    resolved = shutil.which(exec_name)
    if resolved is None:
        sys.stderr.write(
            "agent-exec: executor '%s' not found on PATH\n" % exec_name
        )
        return 127

    with open(prompt_file, "r", encoding="utf-8") as f:
        prompt_text = f.read()

    argv = build_argv(prompt_text)
    env = dict(os.environ)

    os.execvpe(exec_name, argv, env)  # never returns


# --- doctor subcommand -------------------------------------------------------


_KNOWN_BINDIRS = ["~/.local/bin", "/usr/local/bin", "~/bin", "~/.claude/bin"]

_SHIM_EXEC_RE = re.compile(r'exec\s+"([^"]+)"\s+"\$@"')


def _is_executable_file(path):
    try:
        return os.path.isfile(path) and os.access(path, os.X_OK)
    except OSError:
        return False


def _candidate_shim_dirs():
    path_dirs = [p for p in os.environ.get("PATH", "").split(os.pathsep) if p]
    known = [os.path.expanduser(p) for p in _KNOWN_BINDIRS]
    seen = set()
    ordered = []
    for d in path_dirs + known:
        key = os.path.abspath(d)
        if key in seen:
            continue
        seen.add(key)
        ordered.append(d)
    return ordered


def _classify_anchor(target):
    if not target:
        return None
    if "/marketplaces/" in target:
        return "marketplace-clone"
    if "/cache/" in target:
        return "cache"
    return "other"


def _find_shim():
    """Search PATH dirs + known bindirs for an executable 'agent-exec'.

    Returns a dict: installed, path, target, anchored_to, on_path."""
    path_dirs_abs = {
        os.path.abspath(p) for p in os.environ.get("PATH", "").split(os.pathsep) if p
    }

    for d in _candidate_shim_dirs():
        candidate = os.path.join(d, "agent-exec")
        if not _is_executable_file(candidate):
            continue
        target = None
        try:
            with open(candidate, "r", encoding="utf-8", errors="replace") as f:
                content = f.read()
            m = _SHIM_EXEC_RE.search(content)
            if m:
                target = m.group(1)
        except OSError:
            pass
        on_path = os.path.abspath(d) in path_dirs_abs
        return {
            "installed": True,
            "path": os.path.abspath(candidate),
            "target": target,
            "anchored_to": _classify_anchor(target),
            "on_path": on_path,
        }

    return {
        "installed": False,
        "path": None,
        "target": None,
        "anchored_to": None,
        "on_path": False,
    }


def _scan_permission_rule():
    """Best-effort scan of known settings.json files for the
    Bash(agent-exec:*) permission rule. Never raises."""
    rule = "Bash(agent-exec:*)"
    candidates = [
        os.path.expanduser("~/.claude/settings.json"),
        os.path.join(".", ".claude", "settings.json"),
        os.path.join(".", ".claude", "settings.local.json"),
    ]
    sources = []
    seen = set()
    for path in candidates:
        if not os.path.isfile(path):
            continue
        key = os.path.realpath(path)
        if key in seen:
            continue
        seen.add(key)
        try:
            with open(path, "r", encoding="utf-8") as f:
                data = json.load(f)
        except (OSError, ValueError):
            continue
        if not isinstance(data, dict):
            continue
        allow = (data.get("permissions") or {}).get("allow")
        if isinstance(allow, list) and rule in allow:
            sources.append(os.path.abspath(path))

    return {
        "rule": rule,
        "found": len(sources) > 0,
        "sources": sources,
        "best_effort_note": (
            "best-effort scan of ~/.claude/settings.json and "
            "./.claude/settings.json(.local); does not see enterprise "
            "policy or CLI --allowedTools"
        ),
    }


def _build_doctor_report():
    shim = _find_shim()
    uv_present = shutil.which("uv") is not None
    permission = _scan_permission_rule()

    resolved, err = resolve_config()
    doctor_now = time.time()
    doctor_cfg = resolved if err is None and isinstance(resolved, dict) else DEFAULTS
    doctor_state_path = cooldown_state_path(doctor_cfg)
    doctor_cooldowns = {}
    cooldown_cfg = doctor_cfg.get("cooldown")
    if isinstance(cooldown_cfg, dict) and cooldown_cfg.get("enabled") is True:
        doctor_cooldowns = active_cooldowns(
            load_cooldown_state(doctor_state_path), doctor_now
        )
    config_section = {
        "layers_found": [],
        "resolved": err is None,
        "error": err,
        "warnings": _detect_config_warnings(),
        # Fully-resolved 4-layer config (tiers / external_executors with
        # `available` annotated / priority), so the instructor gets both the
        # readiness verdict AND the resolved model policy from a single doctor
        # call instead of also running `agent-exec config`. None on resolve
        # failure (the `error` field carries the reason).
        "values": resolved if err is None else None,
    }
    executors = {}
    ready = {}

    if err is None and isinstance(resolved, dict):
        config_section["layers_found"] = [
            os.path.abspath(p) for p in _ordered_layer_paths()
        ]

        external = resolved.get("external_executors") or {}
        external_dict = external if isinstance(external, dict) else {}

        names = set(external_dict.keys()) | set(KNOWN_EXECUTORS.keys())
        for name in sorted(names):
            cfg = external_dict.get(name)
            cfg = cfg if isinstance(cfg, dict) else {}
            known = KNOWN_EXECUTORS.get(name, {})

            enabled = cfg.get("enabled") is True
            dispatch = cfg.get("dispatch")
            if dispatch is None:
                dispatch = known.get("default_dispatch")

            binary = known.get("binary")
            if binary is None:
                binary = _builtin_exec_for(name)
            if binary is None:
                binary = name

            available = shutil.which(binary) is not None

            entry = {
                "enabled": enabled,
                "dispatch": dispatch,
                "binary": binary,
                "available": available,
            }
            if dispatch == "agent":
                entry["note"] = (
                    "dispatch: agent — CLI binary presence is informational; "
                    "actual availability is the subagent's session "
                    "resolution, which agent-exec cannot determine"
                )
            executors[name] = entry

            if enabled and dispatch == "cli":
                missing = []
                if not shim.get("installed"):
                    missing.append("shim-not-installed")
                if not shim.get("on_path"):
                    missing.append("agent-exec-not-on-path")
                if not permission.get("found"):
                    missing.append("permission-rule-absent")
                if available is not True:
                    missing.append("executor-binary-unavailable")

                ok = bool(
                    available is True
                    and permission.get("found")
                    and shim.get("installed")
                    and shim.get("on_path")
                )
                ready[name] = {"ok": ok, "missing": missing}

    route_preview = {}
    if err is None and isinstance(resolved, dict):
        # Reuse the `ready` section just computed above rather than calling
        # `_build_doctor_report()` again (which would recurse forever) — a
        # synthetic mini-report with only `ready` is all `resolve_route`
        # needs.
        interim_report = {"ready": ready}
        for preview_cls in ("light", "standard", "deep", "review"):
            route_preview[preview_cls] = resolve_route(
                resolved,
                interim_report,
                preview_cls,
                cooldowns=doctor_cooldowns,
            )

    return {
        "shim": shim,
        "uv": {"present": uv_present},
        "config": config_section,
        "executors": executors,
        "permission": permission,
        "ready": ready,
        "route": route_preview,
        "cooldowns": {"path": doctor_state_path, "active": doctor_cooldowns},
    }


def _print_doctor_text(report):
    shim = report["shim"]
    print("agent-exec doctor")
    print("-----------------")
    if shim["installed"]:
        print("shim: installed at %s (on PATH: %s)" % (shim["path"], shim["on_path"]))
        if shim["target"]:
            print("  target: %s (%s)" % (shim["target"], shim["anchored_to"]))
    else:
        print("shim: NOT installed")

    print("uv: %s" % ("present" if report["uv"]["present"] else "NOT found"))

    cfg = report["config"]
    if cfg["resolved"]:
        print("config: resolved (%d layer(s) found)" % len(cfg["layers_found"]))
        for p in cfg["layers_found"]:
            print("  - %s" % p)
    else:
        print("config: FAILED to resolve: %s" % cfg["error"])
    for w in cfg.get("warnings", []):
        if w["type"] == "legacy_json":
            print("  WARNING: legacy orchestra.json found: %s" % w["file"])
        elif w["type"] == "legacy_vocab":
            print(
                "  WARNING: pre-0.4 config vocabulary in %s (keys: %s)"
                % (w["file"], ", ".join(w["keys"]))
            )

    perm = report["permission"]
    print("permission rule %s: %s" % (perm["rule"], "found" if perm["found"] else "NOT found"))
    for s in perm["sources"]:
        print("  - %s" % s)

    if report["executors"]:
        print("executors:")
        for name, info in report["executors"].items():
            print(
                "  - %s: enabled=%s dispatch=%s binary=%s available=%s"
                % (name, info["enabled"], info["dispatch"], info["binary"], info["available"])
            )
            if info.get("note"):
                print("      note: %s" % info["note"])
    else:
        print("executors: none known")

    if report["ready"]:
        print("ready:")
        for name, info in report["ready"].items():
            status = "OK" if info["ok"] else "NOT READY"
            print("  - %s: %s%s" % (name, status, (" (missing: %s)" % ", ".join(info["missing"])) if info["missing"] else ""))

    if report.get("route"):
        print("route (preview):")
        for cls_name, route in report["route"].items():
            if route.get("executor"):
                print(
                    "  - %s: %s (dispatch=%s model=%s effort=%s source=%s)"
                    % (
                        cls_name,
                        route["executor"],
                        route["dispatch"],
                        route["model"],
                        route["effort"],
                        route["source"],
                    )
                )
            else:
                print("  - %s: UNROUTABLE" % cls_name)
    cooldowns = report.get("cooldowns", {})
    if cooldowns.get("active"):
        print("cooldowns:")
        for name, entry in cooldowns.get("active", {}).items():
            print(
                "  - %s: %s (until %s)"
                % (name, entry.get("reason", "unknown"), entry.get("until"))
            )


def cmd_doctor(args):
    fmt = "--json"
    for a in args:
        if a in ("--json", "--text"):
            fmt = a
        else:
            sys.stderr.write("agent-exec: doctor: unknown option: %s\n" % a)
            return 2

    report = _build_doctor_report()

    if fmt == "--text":
        _print_doctor_text(report)
    else:
        print(json.dumps(report, indent=2, ensure_ascii=False))

    return 0


# --- isolate subcommand ------------------------------------------------------
#
# WHY THIS EXISTS. Transcript analysis of 7 weeks of runs found 35 destructive
# VCS commands (`git checkout --`, `git restore`, `git reset --hard`) run by
# *workers* against the user's shared working tree -- spread across every model
# tier, not just the cheap ones. The instructor's response had been to escalate
# the prohibition in prose ("HARD SAFETY RULES: never run git checkout ...",
# then "The ONE exception is ...", then "THE WORKING TREE IS ALREADY DIRTY AND
# THAT IS EXPECTED"). It kept happening anyway, because a worker's only view of
# the world is `git status`, and a diff it did not author reads as contamination
# no matter what the prompt says.
#
# The fix is structural, not textual: give the worker a tree in which no other
# task's changes exist. Then "clean up the stray diff" is a no-op instead of an
# accident. Prose prohibitions stay (see the worker agent definitions) but they
# are no longer the only thing standing between a worker and the user's work.
#
# WHY IT LIVES IN `agent-exec`. The Workflow tool's `agent()` already accepts
# `isolation: 'worktree'`, but that only covers Claude subagents -- the light
# class, where most implementation work actually runs, is dispatched to an
# external CLI executor that never sees that option. Isolation implemented
# here covers both, because both go through `agent-exec dispatch`.
#
# GTR INTEGRATION. If `git gtr` (git-worktree-runner) is installed we create
# worktrees through it rather than reimplementing its job, which means the
# user's existing `gtr.copy.include` patterns and `gtr.hook.postCreate` setup
# steps apply to orchestra's worktrees too. We never *write* gtr config:
# anything orchestra needs is injected for the duration of one subprocess via
# `GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_n`/`GIT_CONFIG_VALUE_n`, which git merges
# into its config lookup without touching `.git/config`, `~/.gitconfig`, or the
# repo-root `.gtrconfig` (the one gtr layer that would be committed). A user's
# own gtr configuration is read, never edited.

# Directories that hold installed dependencies or build caches. They are
# expensive to recreate and (being gitignored) invisible to `git worktree add`,
# so a fresh worktree without them makes every worker re-run an install --
# minutes of wall clock and a large pile of tokens each. Copying them is ~2s.
CARRY_DIR_NAMES = (
    "node_modules", ".venv", "venv", ".tox", "vendor", "target",
    ".next", ".nuxt", ".svelte-kit", ".gradle", "Pods", ".bundle", ".dart_tool",
)

# How deep to look for the directories above. 3 covers the usual monorepo
# shapes (`packages/<pkg>/node_modules`) without walking a whole large tree.
CARRY_SCAN_MAX_DEPTH = 3

ISOLATE_BRANCH_PREFIX = "orchestra/"

# Only used when the user has configured no copy patterns of their own; their
# settings win outright when present.
GTR_FALLBACK_COPY_INCLUDE = (".env", ".env.*", "*.local")

_BASELINE_FILE = "orchestra-baseline"


def git_config_env(pairs, base_env=None):
    """Return an env dict that adds `pairs` to git's config lookup.

    Uses git's `GIT_CONFIG_COUNT` protocol so the settings live for exactly one
    subprocess and no configuration file is created or modified. An existing
    count in `base_env` is extended rather than overwritten.
    """
    env = dict(os.environ if base_env is None else base_env)
    if not pairs:
        return env
    try:
        start = int(env.get("GIT_CONFIG_COUNT", "0"))
        if start < 0:
            start = 0
    except (TypeError, ValueError):
        start = 0
    for offset, (key, value) in enumerate(pairs):
        env["GIT_CONFIG_KEY_%d" % (start + offset)] = key
        env["GIT_CONFIG_VALUE_%d" % (start + offset)] = value
    env["GIT_CONFIG_COUNT"] = str(start + len(pairs))
    return env


def sanitize_task_id(raw):
    """Turn a task id into something safe as both a branch name and a path.

    Path separators split into segments; `.`/`..`/empty segments are dropped so
    traversal cannot survive. Everything outside `[A-Za-z0-9._-]` becomes `-`.
    """
    text = raw if isinstance(raw, str) else ""
    segments = []
    for seg in re.split(r"[/\\]+", text):
        seg = re.sub(r"[^A-Za-z0-9._-]", "-", seg)
        seg = re.sub(r"-{2,}", "-", seg).strip("-")
        if seg in ("", ".", ".."):
            continue
        segments.append(seg)
    out = re.sub(r"-{2,}", "-", "-".join(segments)).strip("-.")
    if not out:
        raise ValueError("task id %r has no usable characters" % (raw,))
    return out


def isolate_branch(task_id):
    return ISOLATE_BRANCH_PREFIX + sanitize_task_id(task_id)


def _git(cwd, *args, **kwargs):
    """Run git, returning (returncode, stdout). Never raises on git failure."""
    env = kwargs.pop("env", None)
    try:
        proc = subprocess.run(
            ["git"] + [a for a in args],
            cwd=cwd,
            capture_output=True,
            text=True,
            env=env,
        )
    except (OSError, ValueError):
        return 1, ""
    return proc.returncode, proc.stdout


def repo_root(path):
    """Absolute path to the working tree root, or None outside a repository."""
    rc, out = _git(path, "rev-parse", "--show-toplevel")
    if rc != 0:
        return None
    root = out.strip()
    return root or None


def tree_is_dirty(path):
    """True when the tree holds uncommitted work worth protecting.

    Ignored files (`node_modules` and friends) deliberately do not count --
    otherwise every repo with dependencies installed would look dirty and
    `auto` would isolate unconditionally.
    """
    root = repo_root(path)
    if root is None:
        return False
    rc, out = _git(root, "status", "--porcelain")
    if rc != 0:
        return False
    return bool(out.strip())


def should_isolate(path, mode):
    """Decide whether a dispatch into `path` should get its own worktree."""
    if mode not in ("auto", "always", "never"):
        raise ValueError("unknown isolation mode: %r" % (mode,))
    if mode == "never":
        return {"isolate": False, "mode": mode, "reason": "isolation disabled (--isolate never)"}
    root = repo_root(path)
    if root is None:
        # Without a repository there is no worktree to make. Say so loudly:
        # the caller asked for isolation and is not getting it.
        return {
            "isolate": False,
            "mode": mode,
            "reason": "not a git repository, cannot isolate",
        }
    if mode == "always":
        return {"isolate": True, "mode": mode, "reason": "isolation forced (--isolate always)", "repo": root}
    if tree_is_dirty(root):
        return {
            "isolate": True,
            "mode": mode,
            "reason": "working tree is dirty; the user's uncommitted work must not be reachable by a worker",
            "repo": root,
        }
    return {
        "isolate": False,
        "mode": mode,
        "reason": "working tree is clean; nothing for a worker to destroy",
        "repo": root,
    }


def _is_git_ignored(root, relpath):
    rc, _ = _git(root, "check-ignore", "-q", "--", relpath)
    return rc == 0


def detect_carry_dirs(root):
    """Find gitignored dependency/build directories worth copying into a worktree.

    Only ignored directories qualify: a tracked `vendor/` already arrives with
    the worktree, and copying an untracked-but-not-ignored directory would
    smuggle files into the diff handed back to the user.
    """
    found = []
    root = os.path.abspath(root)

    def walk(current, depth):
        try:
            entries = sorted(os.listdir(current))
        except OSError:
            return
        for name in entries:
            if name == ".git":
                continue
            full = os.path.join(current, name)
            if not os.path.isdir(full) or os.path.islink(full):
                continue
            rel = os.path.relpath(full, root)
            if name in CARRY_DIR_NAMES and _is_git_ignored(root, rel):
                # Do not descend: nested copies (node_modules/.pnpm/*/node_modules)
                # ride along inside the parent copy.
                found.append(rel)
                continue
            if depth < CARRY_SCAN_MAX_DEPTH:
                walk(full, depth + 1)

    walk(root, 1)
    return found


def copy_tree_fast(src, dst):
    """Copy a directory, preferring a copy-on-write clone. False if it failed.

    APFS (`cp -c`) and reflink-capable Linux filesystems make this near-free in
    both time and disk; elsewhere it degrades to a normal recursive copy.
    """
    if not os.path.isdir(src):
        return False
    parent = os.path.dirname(os.path.abspath(dst))
    try:
        os.makedirs(parent, exist_ok=True)
    except OSError:
        return False
    attempts = []
    if sys.platform == "darwin":
        attempts.append(["cp", "-Rc", src, dst])
    else:
        attempts.append(["cp", "-R", "--reflink=auto", src, dst])
    attempts.append(["cp", "-R", src, dst])
    for argv in attempts:
        try:
            proc = subprocess.run(argv, capture_output=True, text=True)
        except (OSError, ValueError):
            continue
        if proc.returncode == 0:
            return True
        if os.path.exists(dst):
            shutil.rmtree(dst, ignore_errors=True)
    try:
        shutil.copytree(src, dst, symlinks=True)
        return True
    except (OSError, shutil.Error):
        return False


def isolate_home(root):
    """Where plain-git worktrees go: a sibling of the repo, never inside it.

    Inside the repo they would show up as untracked noise in the very tree we
    are trying to keep clean.
    """
    root = os.path.abspath(root)
    return os.path.join(os.path.dirname(root), os.path.basename(root) + "-orchestra")


def _worktree_gitdir(path):
    rc, out = _git(path, "rev-parse", "--absolute-git-dir")
    if rc != 0:
        return None
    return out.strip() or None


def _write_baseline(worktree, sha):
    gitdir = _worktree_gitdir(worktree)
    if not gitdir:
        return
    try:
        with open(os.path.join(gitdir, _BASELINE_FILE), "w") as fh:
            fh.write(sha + "\n")
    except OSError:
        pass


def _read_baseline(worktree):
    gitdir = _worktree_gitdir(worktree)
    if gitdir:
        try:
            with open(os.path.join(gitdir, _BASELINE_FILE)) as fh:
                sha = fh.read().strip()
            if sha:
                return sha
        except OSError:
            pass
    # Fall back to the branch tip: correct as long as nobody committed, and
    # workers are forbidden from committing.
    rc, out = _git(worktree, "rev-parse", "HEAD")
    return out.strip() if rc == 0 else None


def gtr_available():
    rc, _ = _git(None, "gtr", "version")
    return rc == 0


def _gtr_config_pairs(root):
    """Settings to lend gtr for one invocation, without persisting anything."""
    rc, out = _git(root, "config", "--get-all", "gtr.copy.include")
    if rc == 0 and out.strip():
        return []  # the user configured their own patterns; leave them alone
    return [("gtr.copy.include", p) for p in GTR_FALLBACK_COPY_INCLUDE]


def _carry_uncommitted(root, worktree):
    """Reproduce the user's uncommitted state inside the fresh worktree.

    A worktree starts at HEAD, so without this the worker would silently work
    against stale code whenever the user has work in progress.
    """
    rc, patch = _git(root, "diff", "HEAD", "--binary")
    if rc == 0 and patch.strip():
        try:
            subprocess.run(
                ["git", "apply", "--binary", "--whitespace=nowarn", "-"],
                cwd=worktree, input=patch, capture_output=True, text=True,
            )
        except (OSError, ValueError):
            pass
    rc, out = _git(root, "ls-files", "--others", "--exclude-standard")
    if rc != 0:
        return
    for rel in out.splitlines():
        rel = rel.strip()
        if not rel:
            continue
        src = os.path.join(root, rel)
        dst = os.path.join(worktree, rel)
        if not os.path.isfile(src):
            continue
        try:
            os.makedirs(os.path.dirname(dst), exist_ok=True)
            shutil.copy2(src, dst)
        except OSError:
            pass


def _create_via_gtr(root, branch, pairs):
    env = git_config_env(pairs)
    rc, _ = _git(root, "gtr", "new", branch, "--from-current", "--yes", "--no-fetch", env=env)
    if rc != 0:
        return None
    rc, out = _git(root, "gtr", "go", branch, env=env)
    if rc != 0:
        return None
    path = out.strip()
    return path if path and os.path.isdir(path) else None


def _create_via_git(root, branch, task):
    path = os.path.join(isolate_home(root), task)
    try:
        os.makedirs(os.path.dirname(path), exist_ok=True)
    except OSError:
        return None
    rc, _ = _git(root, "worktree", "add", "-q", "-b", branch, path, "HEAD")
    if rc != 0:
        return None
    return path if os.path.isdir(path) else None


def _worktree_for_branch(root, branch):
    for entry in _worktree_entries(root):
        if entry.get("branch") == branch:
            return entry.get("path")
    return None


def _worktree_entries(root):
    rc, out = _git(root, "worktree", "list", "--porcelain")
    if rc != 0:
        return []
    entries = []
    current = {}
    for line in out.splitlines() + [""]:
        if not line.strip():
            if current.get("path"):
                entries.append(current)
            current = {}
            continue
        if line.startswith("worktree "):
            current["path"] = line[len("worktree "):].strip()
        elif line.startswith("branch "):
            ref = line[len("branch "):].strip()
            current["branch"] = ref[len("refs/heads/"):] if ref.startswith("refs/heads/") else ref
    return entries


def isolate_create(root, task, backend="auto", carry=True):
    """Create (or return) the worktree for `task`. Idempotent per task."""
    root = repo_root(root)
    if root is None:
        return {"status": "error", "reason": "not a git repository"}
    task = sanitize_task_id(task)
    branch = ISOLATE_BRANCH_PREFIX + task

    existing = _worktree_for_branch(root, branch)
    if existing:
        return {
            "status": "exists", "task": task, "branch": branch, "path": existing,
            "backend": "existing", "carried": [],
        }

    if backend == "auto":
        backend = "gtr" if gtr_available() else "git"

    path = None
    if backend == "gtr":
        path = _create_via_gtr(root, branch, _gtr_config_pairs(root))
        if path is None:
            backend = "git"  # gtr refused; a worktree still beats a shared tree
    if path is None:
        path = _create_via_git(root, branch, task)
    if path is None:
        return {"status": "error", "reason": "could not create a worktree for %s" % branch}

    _carry_uncommitted(root, path)

    # Commit the user's state as this worktree's baseline, so every later diff
    # shows the worker's changes and nothing else.
    _git(path, "add", "-A")
    _git(path, "-c", "user.email=orchestra@local", "-c", "user.name=orchestra",
         "commit", "-q", "--allow-empty", "-m", "orchestra baseline for %s" % task)
    rc, head = _git(path, "rev-parse", "HEAD")
    baseline = head.strip() if rc == 0 else None
    if baseline:
        _write_baseline(path, baseline)

    carried = []
    if carry:
        for rel in detect_carry_dirs(root):
            if copy_tree_fast(os.path.join(root, rel), os.path.join(path, rel)):
                carried.append(rel)

    return {
        "status": "created", "task": task, "branch": branch, "path": path,
        "backend": backend, "baseline": baseline, "carried": carried,
    }


def isolate_list(root):
    root = repo_root(root)
    if root is None:
        return []
    out = []
    for entry in _worktree_entries(root):
        branch = entry.get("branch") or ""
        if not branch.startswith(ISOLATE_BRANCH_PREFIX):
            continue
        out.append({
            "task": branch[len(ISOLATE_BRANCH_PREFIX):],
            "branch": branch,
            "path": entry.get("path"),
        })
    return out


def isolate_diff(root, task):
    """What the worker changed in its worktree, as a patch plus a file list."""
    root = repo_root(root)
    if root is None:
        return {"status": "error", "reason": "not a git repository"}
    task = sanitize_task_id(task)
    branch = ISOLATE_BRANCH_PREFIX + task
    path = _worktree_for_branch(root, branch)
    if not path:
        return {"status": "absent", "task": task, "files": [], "patch": ""}
    baseline = _read_baseline(path)
    if not baseline:
        return {"status": "error", "reason": "no baseline recorded for %s" % task}
    # `-N` makes new files visible to `git diff` without staging their content.
    # Ignored directories (the carried node_modules) stay invisible.
    _git(path, "add", "-A", "-N")
    rc, names = _git(path, "diff", "--name-only", baseline)
    files = [f for f in names.splitlines() if f.strip()] if rc == 0 else []
    rc, patch = _git(path, "diff", "--binary", baseline)
    return {
        "status": "ok", "task": task, "path": path, "baseline": baseline,
        "files": files, "patch": patch if rc == 0 else "",
    }


def isolate_remove(root, task, force=False):
    """Delete a task's worktree and branch, refusing to discard unreviewed work."""
    root = repo_root(root)
    if root is None:
        return {"status": "error", "reason": "not a git repository"}
    task = sanitize_task_id(task)
    branch = ISOLATE_BRANCH_PREFIX + task
    path = _worktree_for_branch(root, branch)
    if not path:
        return {"status": "absent", "task": task}

    if not force:
        diff = isolate_diff(root, task)
        if diff.get("files"):
            return {
                "status": "dirty", "task": task, "path": path, "files": diff["files"],
                "reason": "worktree holds %d changed file(s) not yet collected; "
                          "collect the diff or pass --force" % len(diff["files"]),
            }

    argv = ["worktree", "remove", path]
    if force:
        argv.append("--force")
    rc, _ = _git(root, *argv)
    if rc != 0:
        return {"status": "error", "task": task, "reason": "git worktree remove failed"}
    _git(root, "branch", "-D", branch)
    return {"status": "removed", "task": task, "path": path}


def _isolate_usage(stream=sys.stderr):
    stream.write(
        "usage: agent-exec isolate {create|list|diff|remove|should} [options]\n"
        "  create --task <id> [--repo <path>] [--backend auto|gtr|git] [--no-carry]\n"
        "  list   [--repo <path>]\n"
        "  diff   --task <id> [--repo <path>] [--names-only]\n"
        "  remove --task <id> [--repo <path>] [--force]\n"
        "  should [--repo <path>] [--mode auto|always|never]\n"
    )


def cmd_isolate(args):
    if not args:
        _isolate_usage()
        return 2
    sub = args[0]
    if sub not in ("create", "list", "diff", "remove", "should"):
        sys.stderr.write("agent-exec: isolate: unknown subcommand: %s\n" % sub)
        _isolate_usage()
        return 2

    task = None
    directory = os.getcwd()
    backend = "auto"
    mode = "auto"
    carry = True
    force = False
    names_only = False

    i = 1
    while i < len(args):
        tok = args[i]
        if tok in ("--task", "--repo", "--backend", "--mode"):
            if i + 1 >= len(args):
                sys.stderr.write("agent-exec: isolate: missing value for %s\n" % tok)
                return 2
            value = args[i + 1]
            if tok == "--task":
                task = value
            elif tok == "--repo":
                directory = value
            elif tok == "--backend":
                backend = value
            else:
                mode = value
            i += 2
        elif tok == "--no-carry":
            carry = False
            i += 1
        elif tok == "--force":
            force = True
            i += 1
        elif tok == "--names-only":
            names_only = True
            i += 1
        else:
            sys.stderr.write("agent-exec: isolate: unknown option: %s\n" % tok)
            return 2

    if sub in ("create", "diff", "remove") and task is None:
        sys.stderr.write("agent-exec: isolate %s: missing required option: --task\n" % sub)
        return 2
    if backend not in ("auto", "gtr", "git"):
        sys.stderr.write("agent-exec: isolate: unknown backend: %s\n" % backend)
        return 2

    try:
        if sub == "create":
            result = isolate_create(directory, task, backend=backend, carry=carry)
        elif sub == "list":
            result = {"worktrees": isolate_list(directory)}
        elif sub == "diff":
            result = isolate_diff(directory, task)
            if names_only:
                result.pop("patch", None)
        elif sub == "remove":
            result = isolate_remove(directory, task, force=force)
        else:
            result = should_isolate(directory, mode)
    except ValueError as exc:
        sys.stderr.write("agent-exec: isolate: %s\n" % exc)
        return 2

    print(json.dumps(result, ensure_ascii=False))
    return 0 if result.get("status") != "error" else 1


# --- route / dispatch subcommands --------------------------------------------
#
# `resolve_route` moves "which executor should handle this class of work"
# out of the instructor's judgment (previously pure English prose in
# SKILL.md) and into code: it walks the same `priority` list the skill
# documents, dropping each candidate on the same reactive-fallback signals
# (disabled / misconfigured / not actually ready) the skill's prose already
# describes, and returns the first survivor. `claude` is always the terminal
# fallback, so a machine with no external executors configured/ready still
# resolves every class.


def _legacy_classes_scan(cfg, cls):
    """Pre-`priority` fallback: scan `external_executors` for any executor
    whose own `classes` list names `cls`, in config order, then append
    `claude` as the terminal fallback. This is the "external executors woven
    in ad hoc per their own classes list" legacy behavior the example config
    documents for when `priority` is omitted entirely."""
    external = cfg.get("external_executors")
    candidates = []
    if isinstance(external, dict):
        for name, ecfg in external.items():
            if not isinstance(ecfg, dict):
                continue
            classes = ecfg.get("classes")
            if isinstance(classes, list) and cls in classes:
                candidates.append(name)
    candidates.append("claude")
    return candidates


def _route_candidates(cfg, cls, archetype):
    """Return (ordered_candidate_names, source) for `cls`/`archetype`:
    `priority[cls][archetype]`, falling back to `priority[cls]["default"]`,
    then to the legacy classes-membership scan (when `external_executors` is
    at least present to scan), then to bare `claude`."""
    priority = cfg.get("priority")
    if isinstance(priority, dict):
        cls_priority = priority.get(cls)
        if isinstance(cls_priority, dict):
            candidates = cls_priority.get(archetype)
            if isinstance(candidates, list) and candidates:
                return list(candidates), "priority"
            candidates = cls_priority.get("default")
            if isinstance(candidates, list) and candidates:
                return list(candidates), "priority"

    external_executors = cfg.get("external_executors")
    if isinstance(external_executors, dict) and external_executors:
        return _legacy_classes_scan(cfg, cls), "classes-legacy"

    return ["claude"], "tiers-default"


def _route_skip_reason(
    name, cls, exhausted_set, external_executors, ready, cooldown_map=None
):
    """Return a skip reason string for candidate `name`, or None if it
    survives and should win. Order matters: exhaustion first (cheapest,
    caller-supplied), then configuration checks, then live readiness."""
    if name in exhausted_set:
        return "exhausted"

    if cooldown_map and name in cooldown_map:
        entry = cooldown_map.get(name)
        reason = entry.get("reason") if isinstance(entry, dict) else None
        return "cooldown:" + (reason or "unknown")

    if name == "claude":
        return None

    ecfg = external_executors.get(name)
    if not isinstance(ecfg, dict):
        return "not-configured"

    if ecfg.get("enabled") is not True:
        return "disabled"

    class_policy = ecfg.get("class_policy")
    policy = class_policy.get(cls) if isinstance(class_policy, dict) else None
    if not isinstance(policy, dict):
        return "no-class-policy"

    dispatch = ecfg.get("dispatch")
    if dispatch == "cli":
        entry = ready.get(name)
        if not isinstance(entry, dict) or entry.get("ok") is not True:
            missing = entry.get("missing") if isinstance(entry, dict) else None
            if isinstance(missing, list) and missing:
                return "not-ready:%s" % ",".join(missing)
            return "not-ready:unknown"
        return None

    if dispatch == "agent":
        # Binary presence is only informational for agent-dispatch executors
        # (see the doctor report's own note on this) — real availability is
        # the subagent's session resolution, which agent-exec cannot
        # determine. Do not hard-gate on it.
        return None

    return "unknown-dispatch:%s" % dispatch


def cooldown_seconds_for(cfg, reason):
    """Pure helper: return the configured positive cooldown for `reason`."""
    if not isinstance(cfg, dict):
        return 0
    cooldown = cfg.get("cooldown")
    if not isinstance(cooldown, dict) or cooldown.get("enabled") is not True:
        return 0
    if not isinstance(reason, str):
        return 0
    seconds = cooldown.get("seconds")
    if not isinstance(seconds, dict) or reason not in seconds:
        return 0
    value = seconds.get(reason)
    if not isinstance(value, int) or isinstance(value, bool) or value <= 0:
        return 0
    return value


def apply_cooldown(state, executor, reason, now, cfg):
    """Pure helper: return a new state with one unavailable executor recorded."""
    result = dict(state) if isinstance(state, dict) else {}
    secs = cooldown_seconds_for(cfg, reason)
    if secs <= 0 or not isinstance(executor, str) or not executor:
        return result

    until = now + secs
    existing = result.get(executor)
    existing_until = existing.get("until") if isinstance(existing, dict) else None
    if (
        isinstance(existing_until, (int, float))
        and not isinstance(existing_until, bool)
        and existing_until > until
    ):
        return result
    result[executor] = {"reason": reason, "until": until}
    return result


def active_cooldowns(state, now):
    """Pure helper: return only well-formed cooldown entries still active."""
    if not isinstance(state, dict):
        return {}
    return {
        executor: entry
        for executor, entry in state.items()
        if isinstance(entry, dict)
        and isinstance(entry.get("until"), (int, float))
        and not isinstance(entry.get("until"), bool)
        and entry.get("until") > now
    }


def cooldown_state_path(cfg):
    """Return the expanded absolute cooldown state path, failing safe."""
    path = None
    if isinstance(cfg, dict):
        cooldown = cfg.get("cooldown")
        if isinstance(cooldown, dict) and isinstance(cooldown.get("path"), str):
            if cooldown.get("path"):
                path = cooldown.get("path")
    if path is None:
        path = DEFAULTS["cooldown"]["path"]
    return os.path.abspath(os.path.expanduser(path))


def load_cooldown_state(path):
    """Best-effort state load; a broken file must never block routing."""
    try:
        with open(path, "r", encoding="utf-8") as f:
            state = json.load(f)
        return state if isinstance(state, dict) else {}
    except (OSError, TypeError, ValueError):
        return {}


def save_cooldown_state(path, state, now):
    """Best-effort atomic state write with expiry pruning."""
    temporary_path = None
    try:
        pruned = {}
        if isinstance(state, dict):
            for executor, entry in state.items():
                until = entry.get("until") if isinstance(entry, dict) else None
                if (
                    isinstance(until, (int, float))
                    and not isinstance(until, bool)
                    and until > now
                ):
                    pruned[executor] = entry
        directory = os.path.dirname(os.path.abspath(path))
        os.makedirs(directory, exist_ok=True)
        fd, temporary_path = tempfile.mkstemp(
            prefix=".executor-state-", dir=directory
        )
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            json.dump(pruned, f, ensure_ascii=False)
        os.replace(temporary_path, path)
        temporary_path = None
        return True
    except (OSError, TypeError, ValueError):
        return False
    finally:
        if temporary_path is not None:
            try:
                os.remove(temporary_path)
            except OSError:
                pass


def record_unavailable_cooldown(cfg, executor, reason, now, exit_code=None, answer=None):
    """Record one unavailable executor -- unless the run it's being asked to
    penalize evidently succeeded.

    `exit_code`/`answer` are the observed outcome of the same run whose
    `status` triggered this call (from `parse_copilot_jsonl`'s result dict,
    when there was an actual subprocess run to observe). A process that
    exited 0 AND produced a real (non-empty) answer completed and answered
    -- that is direct evidence it was NOT out of quota/credits/rate-limited/
    unauthenticated, whatever an availability-scan false positive might have
    concluded upstream. This is a belt-and-braces guard: it exists so that
    any future gap in that scan can't silently cost the executor an hour of
    availability, not as a substitute for getting the scan right. Callers
    that have no run to observe (e.g. a route that never got as far as
    invoking the executor) simply omit `exit_code`/`answer`, and this
    behaves exactly as before.

    Never allowed to raise or change the caller's outcome -- always returns
    a bool, fails closed to False (no cooldown written) on any error."""
    if exit_code == 0 and isinstance(answer, str) and answer.strip() != "":
        return False
    try:
        path = cooldown_state_path(cfg)
        state = load_cooldown_state(path)
        updated = apply_cooldown(state, executor, reason, now, cfg)
        if updated == state and cooldown_seconds_for(cfg, reason) <= 0:
            return False
        return save_cooldown_state(path, updated, now)
    except (OSError, TypeError, ValueError):
        return False


def resolve_route(cfg, doctor_report, cls, archetype="default", exhausted=(), cooldowns=None):
    """Pure resolver: map a capability class (+ optional task archetype) to a
    concrete executor.

    Walks `priority[cls][archetype]` (falling back to `priority[cls]
    ["default"]`, then to the legacy `classes`-membership scan when
    `priority` has nothing for that class, then to bare `claude`). For each
    candidate in order, skips it (with a recorded reason) if it is already
    `exhausted`, unconfigured, disabled, missing a `class_policy` entry for
    `cls`, or (for `dispatch: cli` only) not actually ready per
    `doctor_report["ready"]`. The first survivor wins; `claude` never gets a
    readiness/config check of its own (model = `tiers[cls]`, effort left
    None), so it is the terminal fallback UNLESS the caller explicitly lists
    it in `exhausted` too -- in which case this returns unroutable
    (executor/dispatch None) rather than looping. That exhaustion check is
    intentionally evaluated before the `claude`-is-always-fine special case:
    it is what lets a caller-side retry loop (e.g. `dispatchClass()` in the
    `run` skill) terminate after actually trying every candidate, instead of
    recursing forever because claude "always" resolves.

    `cfg` is a resolved orchestra config dict (as returned by
    `resolve_config()`). `doctor_report` is a dict with a `"ready"` key
    shaped like `_build_doctor_report()`'s `ready` section (only that key is
    read). Returns a dict; `executor`/`dispatch` are None when nothing
    resolves at all (unroutable)."""
    if not isinstance(cfg, dict):
        cfg = {}
    exhausted_set = set(exhausted or ())

    candidates, source = _route_candidates(cfg, cls, archetype)

    external_executors = cfg.get("external_executors")
    if not isinstance(external_executors, dict):
        external_executors = {}
    tiers = cfg.get("tiers")
    if not isinstance(tiers, dict):
        tiers = {}
    ready = {}
    if isinstance(doctor_report, dict):
        ready_section = doctor_report.get("ready")
        if isinstance(ready_section, dict):
            ready = ready_section

    def walk(cooldown_map):
        skipped = []
        winner = None
        winner_idx = None
        for idx, name in enumerate(candidates):
            reason = _route_skip_reason(
                name, cls, exhausted_set, external_executors, ready, cooldown_map
            )
            if reason is None:
                winner = name
                winner_idx = idx
                break
            skipped.append({"executor": name, "reason": reason})
        return winner, winner_idx, skipped

    winner, winner_idx, skipped = walk(cooldowns)
    cooldown_bypassed = False
    if winner is None and cooldowns:
        winner, winner_idx, skipped = walk(None)
        cooldown_bypassed = True

    result = {
        "class": cls,
        "archetype": archetype,
        "candidates": list(candidates),
        "skipped": skipped,
        "source": source,
        "cooldowns_applied": [
            {
                "executor": item["executor"],
                "reason": cooldowns[item["executor"]].get("reason", "unknown"),
                "until": cooldowns[item["executor"]].get("until"),
            }
            for item in skipped
            if item["reason"].startswith("cooldown:")
            and isinstance(cooldowns.get(item["executor"]), dict)
        ] if cooldowns else [],
        "cooldown_bypassed": cooldown_bypassed,
    }

    if winner is None:
        result.update({
            "executor": None,
            "dispatch": None,
            "model": None,
            "effort": None,
            "agent_type": None,
            "remaining": [],
        })
        return result

    result["remaining"] = list(candidates[winner_idx + 1:])

    if winner == "claude":
        result.update({
            "executor": "claude",
            "dispatch": "claude",
            "model": tiers.get(cls),
            "effort": None,
            "agent_type": None,
        })
        return result

    ecfg = external_executors.get(winner) or {}
    dispatch = ecfg.get("dispatch")
    class_policy = ecfg.get("class_policy")
    policy = class_policy.get(cls) if isinstance(class_policy, dict) else None
    if not isinstance(policy, dict):
        policy = {}
    result.update({
        "executor": winner,
        "dispatch": dispatch,
        "model": policy.get("model"),
        "effort": policy.get("effort"),
        "agent_type": ecfg.get("agent_type") if dispatch == "agent" else None,
    })
    return result


def _print_route_text(route):
    print("agent-exec route")
    print("-----------------")
    print("class: %s (archetype: %s)" % (route.get("class"), route.get("archetype")))
    print("candidates: %s" % ", ".join(route.get("candidates", [])))
    if route.get("executor"):
        print(
            "resolved: %s (dispatch=%s model=%s effort=%s)"
            % (route.get("executor"), route.get("dispatch"), route.get("model"), route.get("effort"))
        )
    else:
        print("resolved: UNROUTABLE")
    if route.get("skipped"):
        print("skipped:")
        for s in route.get("skipped", []):
            print("  - %s: %s" % (s.get("executor"), s.get("reason")))
    for item in route.get("cooldowns_applied", []):
        print(
            "cooldown: %s (%s) until %s"
            % (item.get("executor"), item.get("reason"), item.get("until"))
        )
    if route.get("cooldown_bypassed"):
        print("cooldown: BYPASSED (all candidates were cooled down)")
    if route.get("remaining"):
        print("remaining: %s" % ", ".join(route.get("remaining", [])))
    print("source: %s" % route.get("source"))


def cmd_route(args):
    cls = None
    archetype = "default"
    exhausted = []
    fmt = "--json"
    no_cooldown = False

    i = 0
    while i < len(args):
        tok = args[i]
        if tok == "--class":
            if i + 1 >= len(args):
                sys.stderr.write("agent-exec: route: missing value for --class\n")
                return 2
            cls = args[i + 1]
            i += 2
        elif tok == "--archetype":
            if i + 1 >= len(args):
                sys.stderr.write("agent-exec: route: missing value for --archetype\n")
                return 2
            archetype = args[i + 1]
            i += 2
        elif tok == "--exhausted":
            if i + 1 >= len(args):
                sys.stderr.write("agent-exec: route: missing value for --exhausted\n")
                return 2
            exhausted = [x for x in args[i + 1].split(",") if x]
            i += 2
        elif tok in ("--json", "--text"):
            fmt = tok
            i += 1
        elif tok == "--no-cooldown":
            no_cooldown = True
            i += 1
        else:
            sys.stderr.write("agent-exec: route: unknown option: %s\n" % tok)
            return 2

    if cls is None:
        sys.stderr.write("agent-exec: route: missing required option: --class\n")
        return 2

    resolved, err = resolve_config()
    if err is not None:
        sys.stderr.write(err + "\n")
        return 1

    doctor_report = _build_doctor_report()
    cooldowns = None
    cooldown_cfg = resolved.get("cooldown")
    if not no_cooldown and isinstance(cooldown_cfg, dict) and cooldown_cfg.get("enabled") is True:
        cooldowns = active_cooldowns(
            load_cooldown_state(cooldown_state_path(resolved)), time.time()
        )
    route = resolve_route(
        resolved,
        doctor_report,
        cls,
        archetype=archetype,
        exhausted=exhausted,
        cooldowns=cooldowns,
    )

    if fmt == "--text":
        _print_route_text(route)
    else:
        print(json.dumps(route, indent=2, ensure_ascii=False))

    return 0


def cmd_dispatch_route(args):
    cls = None
    archetype = "default"
    exhausted = []
    prompt_file = None
    workdir = None
    resume = None
    # Accepted for parity with `run --capture`; the cli branch below always
    # runs in capture style (never exec-replaces) since it must print the
    # combined result+route JSON rather than replace this process.
    capture = False
    no_cooldown = False
    # Default `auto`: isolate exactly when there is uncommitted work a worker
    # could destroy. See the isolate section above for why this exists.
    isolate_mode = "auto"
    task = None

    i = 0
    while i < len(args):
        tok = args[i]
        if tok == "--capture":
            capture = True
            i += 1
        elif tok == "--isolate":
            if i + 1 >= len(args):
                sys.stderr.write("agent-exec: dispatch: missing value for --isolate\n")
                return 2
            isolate_mode = args[i + 1]
            if isolate_mode not in ("auto", "always", "never"):
                sys.stderr.write(
                    "agent-exec: dispatch: --isolate must be auto|always|never\n"
                )
                return 2
            i += 2
        elif tok == "--task":
            if i + 1 >= len(args):
                sys.stderr.write("agent-exec: dispatch: missing value for --task\n")
                return 2
            task = args[i + 1]
            i += 2
        elif tok == "--class":
            if i + 1 >= len(args):
                sys.stderr.write("agent-exec: dispatch: missing value for --class\n")
                return 2
            cls = args[i + 1]
            i += 2
        elif tok == "--archetype":
            if i + 1 >= len(args):
                sys.stderr.write("agent-exec: dispatch: missing value for --archetype\n")
                return 2
            archetype = args[i + 1]
            i += 2
        elif tok == "--exhausted":
            if i + 1 >= len(args):
                sys.stderr.write("agent-exec: dispatch: missing value for --exhausted\n")
                return 2
            exhausted = [x for x in args[i + 1].split(",") if x]
            i += 2
        elif tok == "--prompt-file":
            if i + 1 >= len(args):
                sys.stderr.write("agent-exec: dispatch: missing value for --prompt-file\n")
                return 2
            prompt_file = args[i + 1]
            i += 2
        elif tok == "--workdir":
            if i + 1 >= len(args):
                sys.stderr.write("agent-exec: dispatch: missing value for --workdir\n")
                return 2
            workdir = args[i + 1]
            i += 2
        elif tok == "--resume":
            if i + 1 >= len(args):
                sys.stderr.write("agent-exec: dispatch: missing value for --resume\n")
                return 2
            resume = args[i + 1]
            i += 2
        elif tok == "--no-cooldown":
            no_cooldown = True
            i += 1
        else:
            sys.stderr.write("agent-exec: dispatch: unknown option: %s\n" % tok)
            return 2

    if cls is None:
        sys.stderr.write("agent-exec: dispatch: missing required option: --class\n")
        return 2
    if prompt_file is None:
        sys.stderr.write("agent-exec: dispatch: missing required option: --prompt-file\n")
        return 2
    if workdir is None:
        sys.stderr.write("agent-exec: dispatch: missing required option: --workdir\n")
        return 2

    resolved, err = resolve_config()
    if err is not None:
        sys.stderr.write(err + "\n")
        return 1

    doctor_report = _build_doctor_report()
    cooldowns = None
    cooldown_cfg = resolved.get("cooldown")
    if not no_cooldown and isinstance(cooldown_cfg, dict) and cooldown_cfg.get("enabled") is True:
        cooldowns = active_cooldowns(
            load_cooldown_state(cooldown_state_path(resolved)), time.time()
        )
    route = resolve_route(
        resolved,
        doctor_report,
        cls,
        archetype=archetype,
        exhausted=exhausted,
        cooldowns=cooldowns,
    )

    if route["executor"] is None:
        print(json.dumps({"status": "unroutable", "route": route}, ensure_ascii=False))
        return 0

    # Resolve isolation before handing the work anywhere. Every branch below
    # reports `isolation` so the caller always knows which tree was used and
    # where to collect the diff from. Failing to isolate degrades to the shared
    # tree with a stated reason -- never aborts the dispatch.
    isolation = should_isolate(workdir, isolate_mode)
    if isolation["isolate"]:
        if task is None:
            isolation = {
                "isolate": False,
                "mode": isolate_mode,
                "reason": "isolation wanted (%s) but no --task id was given; "
                          "an id is what lets retry rounds share one worktree"
                          % isolation["reason"],
            }
        else:
            try:
                created = isolate_create(workdir, task)
            except ValueError as exc:
                created = {"status": "error", "reason": str(exc)}
            if created.get("status") in ("created", "exists") and created.get("path"):
                workdir = created["path"]
                isolation = {
                    "isolate": True,
                    "mode": isolate_mode,
                    "reason": isolation["reason"],
                    "path": created["path"],
                    "branch": created.get("branch"),
                    "backend": created.get("backend"),
                    "carried": created.get("carried", []),
                    "created": created.get("status") == "created",
                }
            else:
                isolation = {
                    "isolate": False,
                    "mode": isolate_mode,
                    "reason": "could not isolate (%s); running in the shared tree"
                              % created.get("reason", "unknown error"),
                }

    if route["dispatch"] != "cli":
        # agent (codex) or claude: agent-exec cannot spawn subagents itself
        # -- the caller (the instructor) makes the Agent-tool call.
        output = {
            "status": "delegate",
            "executor": route["executor"],
            "model": route["model"],
            "effort": route["effort"],
            "agent_type": route["agent_type"],
            "route": route,
            "isolation": isolation,
        }
        print(json.dumps(output, ensure_ascii=False))
        return 0

    profile_name = route["executor"]
    model = route["model"]
    effort = route["effort"]

    profile = PROFILES.get(profile_name)
    if profile is None:
        # A user config can in principle declare a `dispatch: cli` executor
        # agent-exec has no built-in invocation profile for (only `copilot`
        # has one today). Surface as unavailable rather than crashing.
        output = {
            "status": "unavailable",
            "answer": None,
            "session_id": None,
            "reason": None,
            "exit_code": None,
            "executor": profile_name,
            "model": model,
            "effort": effort,
            "route": route,
            "isolation": isolation,
        }
        print(json.dumps(output, ensure_ascii=False))
        record_unavailable_cooldown(
            resolved, profile_name, output.get("reason"), time.time()
        )
        return 0

    exec_name = profile["exec"]

    if os.environ.get("AGENT_EXEC_DRYRUN"):
        argv = _build_copilot_argv(exec_name, model, effort, workdir, "@%s" % prompt_file, resume, "json")
        print("PROFILE: %s" % profile_name)
        print("MODE: headless")
        print("ENV: (none)")
        print("EXEC: %s" % " ".join(argv))
        print(
            "CAPTURE: yes (would subprocess-run %s and emit normalized "
            "JSON + route)" % profile_name
        )
        return 0

    exit_code, result = _run_copilot_capture(profile_name, model, effort, workdir, prompt_file, resume, "json")
    if result is None:
        sys.stderr.write("agent-exec: executor '%s' not found on PATH\n" % exec_name)
        return exit_code

    try:
        if result.get("status") == "unavailable":
            record_unavailable_cooldown(
                resolved,
                profile_name,
                result.get("reason"),
                time.time(),
                exit_code=result.get("exit_code"),
                answer=result.get("answer"),
            )
        record = build_dispatch_record(profile_name, result, resume, cls)
        telemetry_cfg = resolved if isinstance(resolved, dict) else DEFAULTS
        telemetry_append(record, telemetry_cfg)
    except Exception:
        # Telemetry must never break dispatch.
        pass

    output = dict(result)
    output["executor"] = profile_name
    output["model"] = model
    output["effort"] = effort
    output["route"] = route
    output["isolation"] = isolation
    print(json.dumps(output, ensure_ascii=False))
    return 0


def main(argv):
    if len(argv) == 0 or argv[0] in ("-h", "--help"):
        print_usage()
        return 0

    tok = argv[0]

    if tok == "install":
        return cmd_install()

    if tok == "list":
        return cmd_list()

    if tok == "config":
        return cmd_config(argv[1:])

    if tok == "run":
        return cmd_run(argv[1:])

    if tok == "doctor":
        return cmd_doctor(argv[1:])

    if tok == "route":
        return cmd_route(argv[1:])

    if tok == "dispatch":
        return cmd_dispatch_route(argv[1:])

    if tok == "isolate":
        return cmd_isolate(argv[1:])

    if tok == "telemetry":
        return cmd_telemetry(argv[1:])

    if tok == "cooldown":
        return cmd_cooldown(argv[1:])

    return cmd_dispatch(tok, argv[1:])


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
