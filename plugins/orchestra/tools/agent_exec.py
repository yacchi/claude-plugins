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
    "external_executors": {},
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
                                  Bash(agent-exec:*) permission rule, and an
                                  overall ready/missing verdict per executor
                                  (--json is the default)
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


def parse_copilot_jsonl(stdout_text, stderr_text, exit_code):
    """Pure parser: copilot JSONL stdout + stderr + subprocess exit code ->
    normalized result dict. No I/O, no subprocess calls; unit-testable with
    fixtures."""
    last_content = None
    last_final_content = None
    session_id = None

    for line in (stdout_text or "").splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            event = json.loads(line)
        except (ValueError, TypeError):
            continue
        if not isinstance(event, dict):
            continue

        etype = event.get("type")
        data = event.get("data")

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

    combined_text = (stdout_text or "") + "\n" + (stderr_text or "")
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

    if capture:
        proc = subprocess.run(
            argv,
            env=env,
            capture_output=True,
            text=True,
        )
        result = parse_copilot_jsonl(proc.stdout, proc.stderr, proc.returncode)
        print(json.dumps(result, ensure_ascii=False))
        try:
            record = build_dispatch_record(profile_name, result, resume, cls)
            telemetry_cfg, telemetry_err = resolve_config()
            if telemetry_err is not None or not isinstance(telemetry_cfg, dict):
                telemetry_cfg = DEFAULTS
            telemetry_append(record, telemetry_cfg)
        except Exception:
            # Telemetry must never break dispatch.
            pass
        return 0

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

    return {
        "shim": shim,
        "uv": {"present": uv_present},
        "config": config_section,
        "executors": executors,
        "permission": permission,
        "ready": ready,
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

    if tok == "telemetry":
        return cmd_telemetry(argv[1:])

    return cmd_dispatch(tok, argv[1:])


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
