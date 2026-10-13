"""OS sandbox for CLI executor children (defense in depth on top of worktree
isolation).

A sandboxed worker may only WRITE inside its task's tree (plus temp, caches
and its own executor state), may not READ well-known secret stores, and may
not signal processes outside its sandbox. Network stays open (model API).

One policy (`build_policy`) feeds every backend:

- macOS ``seatbelt``: ``sandbox-exec -D ... -p <SBPL> -- argv``.
- Linux ``bwrap`` (preferred, only when a real probe succeeds -- Ubuntu >=
  24.04 blocks unprivileged user namespaces through AppArmor by default).
- Linux ``landlock`` (fallback): agent-exec re-executes itself as
  ``<python> agent_exec.py _sandbox-exec --write W ... -- argv``; only that
  child restricts itself, then execs argv. Landlock cannot hide a subtree
  under a readable parent, so deny_read (and, below ABI 6, signal scoping)
  is reported in ``limits`` instead of being silently claimed.
- ``codex-native``: codex applies its own Seatbelt/Landlock and nesting
  sandbox-exec fails, so it is never wrapped.
- ``none``: mode off, or nothing usable on this host.

Every path is realpath'd before it reaches a rule: on macOS ``/tmp`` is
``/private/tmp`` and ``$TMPDIR`` lives under ``/private/var/folders``, and a
Seatbelt rule on the unresolved spelling silently never matches.
"""

import ctypes
import glob
import json
import os
import re
import shutil
import struct
import subprocess
import sys
import tempfile
from datetime import datetime, timezone

MODES = ("auto", "required", "off")
NETWORK_MODES = ("allowlist", "off")
# The only host a hosted Claude Code session needs.
DEFAULT_ALLOWED_DOMAINS = ("api.anthropic.com",)
# Background traffic of Claude Code and herdr that no worker task needs
# (MCP connector proxy, auto-update, telemetry, herdr's update check). Still
# blocked and still in the audit log, but never surfaced as a denial to
# approve - listing them would invite granting them.
DEFAULT_QUIET_DOMAINS = (
    "mcp-proxy.anthropic.com", "downloads.claude.ai", "statsig.anthropic.com",
    "datadoghq.com", "*.datadoghq.com", "sentry.io", "*.sentry.io", "herdr.dev",
)

# Secret stores no worker has a reason to read.
DEFAULT_DENY_READ = (
    "~/.ssh",
    "~/.aws",
    "~/.gnupg",
    "~/.config/gh",
    "~/.netrc",
    "~/.docker/config.json",
    "~/.kube",
    "~/Library/Keychains",
)

# DEFAULT_DENY_READ entries the herdr host must be able to read (see host_policy).
HOST_READABLE = ("~/Library/Keychains",)

# Package/build caches every toolchain a worker runs writes into.
# NOT `~/.local/share/uv/tools`: that holds uv's INSTALLED tool environments,
# code that runs later outside any sandbox. A sandboxed child gets
# UV_TOOL_DIR pointed into temp instead (`child_env`).
SEED_CACHE_WRITES = (
    "~/.cache",            # uv, pip, pre-commit, generic XDG cache
    "~/.npm",              # npm / npx
    "~/go/pkg/mod",        # go module cache (GOMODCACHE)
)
SEED_CACHE_WRITES_DARWIN = (
    "~/Library/Caches",    # go-build cache, Homebrew, pip on macOS
)

# Shared package caches a denied write may be AUTO-granted under (root + one
# more path component of the denied path). Allowlist, never a denylist.
# Shared caches are a cache-poisoning vector the user accepted in exchange
# for cache reuse across worktrees; what keeps that acceptable is that each
# ecosystem verifies what it pulls out of the cache against a lockfile or
# checksum (go.sum, npm/pnpm/yarn integrity, Cargo checksums, Gradle/Maven
# checksums, nuget/pub/composer hashes, Terraform's .terraform.lock.hcl). A
# root is only eligible if its ecosystem verifies cached artifacts. Toolchain
# installs, bin dirs and config dirs are code-execution or persistence paths
# and are never on this list.
CACHE_ROOTS = (
    "~/.cache",
    "~/Library/Caches",
    "~/.npm",
    "~/.pnpm-store",
    "~/Library/pnpm/store",
    "~/.local/share/pnpm/store",
    "~/.yarn/berry/cache",
    "~/go/pkg/mod",
    "~/.cargo/registry",
    "~/.cargo/git",
    "~/.gradle/caches",
    "~/.m2/repository",
    "~/.bun/install/cache",
    "~/.nuget/packages",
    "~/.pub-cache",
    "~/.composer/cache",
    "~/.ivy2/cache",
    "~/.terraform.d/plugin-cache",
)

# Per-machine grants learned from denials (`auto`) or added by the user with
# `agent-exec sandbox allow` (`user`). Every run adds them to the writable set.

# Paths no worker may write whatever the writable set says (tamper
# protection): the learned grants, orchestra's user config, its executor
# state (cooldowns) and uv's installed tools (run later, unsandboxed). Plus
# every `.claude/orchestra*.y*ml` in the task's repo.
TAMPER_FILES = (
    "~/.claude/orchestra.yaml",
    "~/.claude/orchestra.yml",
    "~/.claude/orchestra/executor-state.json",
    "~/.local/share/uv/tools",
)

# pi's state dir stays writable (lock files, auth refresh), but what pi LOADS
# as code or instructions on a later, unsandboxed run must not be plantable.
PI_DENY_SUBDIRS = ("extensions", "skills", "prompts", "themes", "packages",
                   "npm", "git", "bin", "install")
PI_DENY_FILES = ("settings.json", "mcp.json", "models.json", "AGENTS.md",
                 "SYSTEM.md", "APPEND_SYSTEM.md", "keybindings.json")

UV_TOOL_DIR_NAME = "orchestra-uv-tools"

# Test hook: AGENT_EXEC_SANDBOX_BACKEND=none makes every backend probe fail,
# so `mode: required` refusal can be exercised on any host.
FORCE_ENV = "AGENT_EXEC_SANDBOX_BACKEND"

SANDBOX_EXEC = "/usr/bin/sandbox-exec"


def _expand(path, home):
    if path == "~" or path.startswith("~/"):
        path = home + path[1:]
    return os.path.realpath(path)


def _dedupe(paths):
    out = []
    for p in paths:
        if p and p not in out:
            out.append(p)
    return out


def _under(path, directory):
    return path == directory or path.startswith(directory.rstrip("/") + "/")


def _home(home=None):
    return os.path.realpath(home or os.path.expanduser("~"))


# --- classification ----------------------------------------------------------


def cache_roots(home=None):
    home = _home(home)
    return [_expand(r, home) for r in CACHE_ROOTS]


def grant_for(path, home=None):
    """The auto-grant for a denied `path`, or None if it is not eligible.

    This is the one eligibility rule. `path` is realpath'd first (a
    `~/.cache/x -> ~/.ssh` symlink resolves to `~/.ssh` and no longer
    matches), then matched with a component boundary (`~/.cache-evil` is not
    under `~/.cache`). The grant is the matched root plus ONE further
    component of the denied path, or the root itself when the denial was on
    or directly inside the root. A candidate overlapping a tamper-protected
    path (e.g. the grant store) is never returned."""
    if not isinstance(path, str) or not path:
        return None
    real = os.path.realpath(path)
    protected = tamper_paths(home)
    for root in cache_roots(home):
        candidate = None
        if real == root:
            candidate = root
        elif _under(real, root):
            relative = real[len(root.rstrip("/")) + 1:]
            first, separator, _rest = relative.partition("/")
            if first in ("", ".", ".."):
                continue
            candidate = root if not separator else os.path.join(root, first)
        if candidate and not any(_under(candidate, t) or _under(t, candidate)
                                for t in protected):
            return candidate
    return None


def tamper_paths(home=None):
    home = _home(home)
    # learned_path is resolved here too, so the active store is protected even
    # when XDG_STATE_HOME points somewhere other than ~/.local/state.
    paths = [_expand(p, home) for p in TAMPER_FILES]
    store = learned_path(home)
    paths.extend((store, os.path.dirname(store)))
    return _dedupe(paths)


def user_grant_refusal(path, deny_read=(), home=None):
    """Why a `source: user` grant on `path` is refused, or None if allowed."""
    home = _home(home)
    real = os.path.realpath(path)
    if real == "/":
        return "refusing to grant /"
    if real == home:
        return "refusing to grant the home directory itself"
    lexical = os.path.abspath(path)
    for claude in (_expand("~/.claude", home), os.path.join(home, ".claude")):
        if any(_under(p, claude) or _under(claude, p) for p in (real, lexical)):
            return "refusing to grant anything under ~/.claude"
    for r in deny_read:
        if _under(real, r) or _under(r, real):
            return "path overlaps deny_read entry %s" % r
    for t in tamper_paths(home):
        if _under(t, real) or _under(real, t):
            return "path overlaps tamper-protected %s" % t
    return None


# --- learned grants store ----------------------------------------------------


STATE_DIR = "~/.local/state/claude-orchestra"


def state_dir(home=None):
    """The machine-local orchestra state directory, resolved in one place."""
    home = _home(home)
    state_home = os.environ.get("XDG_STATE_HOME")
    if state_home and os.path.isabs(state_home):
        return os.path.realpath(os.path.join(state_home, "claude-orchestra"))
    return _expand(STATE_DIR, home)


def learned_path(home=None):
    """Resolve the machine-local grant store in one place."""
    return os.path.join(state_dir(home), "sandbox-learned.json")


def _valid_grant(entry, home, deny_read):
    if not isinstance(entry, dict):
        return False
    path = entry.get("path")
    if not isinstance(path, str) or not os.path.isabs(path):
        return False
    source = entry.get("source")
    if source == "auto":
        return grant_for(path, home) is not None
    if source == "user":
        return user_grant_refusal(path, deny_read, home) is None
    return False


def load_learned(home=None, deny_read=None):
    """(grants, warning). Tolerant: a missing file is ([], None); an
    unreadable or corrupt one is ([], <warning>). Entries that fail their
    source's rules are dropped with a warning, never trusted."""
    home = _home(home)
    if deny_read is None:
        deny_read = [_expand(p, home) for p in DEFAULT_DENY_READ]
    path = learned_path(home)
    try:
        with open(path, "r", encoding="utf-8") as fh:
            data = json.load(fh)
    except FileNotFoundError:
        return [], None
    except (OSError, ValueError) as exc:
        return [], "learned sandbox grants unreadable (%s): %s" % (path, exc)
    grants = data.get("grants") if isinstance(data, dict) else None
    if not isinstance(grants, list):
        return [], "learned sandbox grants corrupt (%s): no grants list" % path
    good = [g for g in grants if _valid_grant(g, home, deny_read)]
    warning = None
    if len(good) != len(grants):
        warning = "learned sandbox grants: ignored %d invalid entr%s in %s" % (
            len(grants) - len(good), "y" if len(grants) - len(good) == 1 else "ies", path)
    return [dict(g) for g in good], warning


def _raw_store(home=None):
    try:
        with open(learned_path(home), "r", encoding="utf-8") as fh:
            data = json.load(fh)
    except (OSError, ValueError):
        return {}
    return data if isinstance(data, dict) else {}


def save_learned(grants, home=None, domains=None):
    """Atomic write: temp file in the same dir, 0600, os.replace. The other
    list of the store (path grants / domain grants) is kept as it is unless
    given."""
    path = learned_path(home)
    directory = os.path.dirname(path)
    os.makedirs(directory, mode=0o700, exist_ok=True)
    raw = _raw_store(home)
    if grants is None:
        grants = raw.get("grants") if isinstance(raw.get("grants"), list) else []
    if domains is None:
        domains = raw.get("domains") if isinstance(raw.get("domains"), list) else []
    data = {"version": 1, "grants": grants}
    if domains:
        data["domains"] = domains
    payload = json.dumps(data, indent=2, ensure_ascii=False) + "\n"
    fd, tmp = tempfile.mkstemp(prefix=".sandbox-learned-", dir=directory)
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            fh.write(payload)
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(tmp, path)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


def _now_iso():
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def add_grant(path, source, example=None, home=None):
    """Persist one grant; returns the stored entry. Raises ValueError when
    the grant is not allowed for `source`. Idempotent per realpath."""
    home = _home(home)
    real = os.path.realpath(path)
    if source == "auto":
        if grant_for(real, home) is None:
            raise ValueError("not an eligible cache grant: %s" % real)
    elif source == "user":
        reason = user_grant_refusal(real, [_expand(p, home) for p in DEFAULT_DENY_READ], home)
        if reason:
            raise ValueError(reason)
    else:
        raise ValueError("unknown grant source: %s" % source)
    grants, _ = load_learned(home)
    for g in grants:
        if g["path"] == real:
            return g
    entry = {"path": real, "added": _now_iso(), "source": source,
             "example": os.path.realpath(example) if example else real}
    grants.append(entry)
    save_learned(grants, home)
    return entry


def forget_grant(path, home=None):
    """Remove a grant; True if one was removed."""
    home = _home(home)
    real = os.path.realpath(path)
    grants, _ = load_learned(home)
    kept = [g for g in grants if g["path"] != real]
    if len(kept) == len(grants):
        return False
    save_learned(kept, home)
    return True


# --- learned network domains -------------------------------------------------
#
# Domains the user approved with `agent-exec sandbox allow-domain`, stored in
# the learned-grants file under their own `domains` key (never mixed with the
# path grants) and so under the same tamper protection.

_DOMAIN_RE = re.compile(r"^(\*\.)?([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)*"
                        r"[a-z0-9]([a-z0-9-]*[a-z0-9])?(:[0-9]{1,5})?$")


def _is_ip(text):
    import ipaddress
    host = text
    if host.startswith("[") and "]" in host:
        host = host[1:host.index("]")]
    elif host.count(":") == 1:
        host = host.split(":", 1)[0]
    try:
        ipaddress.ip_address(host)
    except ValueError:
        return False
    return True


def domain_refusal(domain):
    """Why a domain grant on `domain` is refused, or None if allowed."""
    if not isinstance(domain, str) or not domain.strip():
        return "refusing an empty domain"
    text = domain.strip().lower()
    if text == "*" or text.startswith("*:"):
        return "refusing the wildcard *"
    if _is_ip(text):
        return "refusing an IP literal: %s" % text
    if not _DOMAIN_RE.match(text):
        return "not a host name or *.wildcard: %s" % text
    host = text.rsplit(":", 1)[0] if ":" in text else text
    bare = host[2:] if host.startswith("*.") else host
    if bare == "localhost" or bare.endswith(".localhost"):
        return "refusing localhost"
    if host.startswith("*.") and "." not in bare:
        return "refusing a top-level-domain wildcard: %s" % host
    if host.replace(".", "").isdigit():
        return "refusing an IP literal: %s" % text
    return None


def load_domains(home=None):
    """(domain grants, warning): entries failing `domain_refusal` are
    dropped, never trusted."""
    raw = _raw_store(home)
    entries = raw.get("domains")
    if entries is None:
        return [], None
    if not isinstance(entries, list):
        return [], "learned domain grants corrupt (%s)" % learned_path(home)
    good = [e for e in entries if isinstance(e, dict)
            and domain_refusal(e.get("domain")) is None]
    warning = None
    if len(good) != len(entries):
        warning = "learned domain grants: ignored %d invalid entr%s" % (
            len(entries) - len(good), "y" if len(entries) - len(good) == 1 else "ies")
    return [dict(e) for e in good], warning


def learned_domains(home=None):
    return _dedupe(e["domain"].strip().lower() for e in load_domains(home)[0])


def add_domain(domain, home=None):
    """Persist one domain grant; returns the stored entry. Raises ValueError
    when refused. Idempotent."""
    reason = domain_refusal(domain)
    if reason:
        raise ValueError(reason)
    text = domain.strip().lower()
    domains, _ = load_domains(home)
    for entry in domains:
        if entry["domain"].strip().lower() == text:
            return entry
    entry = {"domain": text, "added": _now_iso(), "source": "user"}
    domains.append(entry)
    save_learned(None, home, domains=domains)
    return entry


def forget_domain(domain, home=None):
    """Remove a domain grant; True if one was removed."""
    text = (domain or "").strip().lower()
    domains, _ = load_domains(home)
    kept = [e for e in domains if e["domain"].strip().lower() != text]
    if len(kept) == len(domains):
        return False
    save_learned(None, home, domains=kept)
    return True


# --- sandbox-runtime (srt) for the herdr host ---------------------------------

SRT_SETTINGS_DIR = "herdr-srt"
AUDIT_DIR = "audit"
NETWORK_AUDIT = "network.jsonl"

_SRT_BLOCKED_RE = re.compile(r"Connection blocked to (\S+)")


def parse_srt_denial(line):
    """(host, port) of a `Connection blocked to <host>:<port>` srt debug
    line, or None. IPv6 hosts come bracketed (`[::1]:443`) or bare
    (`::1:443`, the last component is the port)."""
    match = _SRT_BLOCKED_RE.search(line or "")
    if not match:
        return None
    target = match.group(1).rstrip(".,;")
    if target.startswith("["):
        host, _, rest = target[1:].partition("]")
        port = rest[1:] if rest.startswith(":") else ""
    else:
        host, sep, port = target.rpartition(":")
        if not sep or not port.isdigit():
            host, port = target, ""
    if not host:
        return None
    return host, (int(port) if port.isdigit() else None)


def _real(path):
    return os.path.realpath(path) if os.path.lexists(path) else path


def _regex_globs(rx):
    """srt has globs, not regexes: the repo-config regex as globs."""
    match = re.match(r"^\^(.*)/\(\.\*/\)\?\\\.claude/orchestra", rx)
    if not match:
        return []
    root = re.sub(r"\\(.)", r"\1", match.group(1))
    return [root + "/.claude/orchestra*.y*ml", root + "/**/.claude/orchestra*.y*ml"]


def srt_settings(policy, sockets, allowed_domains, denied_domains, home=None):
    """The sandbox-runtime settings for the herdr server, from `host_policy`.
    Paths are realpaths where they exist (srt follows no symlink for an
    allow); `writable_regex` (~/.claude.json and its siblings) becomes the
    glob `<home>/.claude.json*`; a `readonly` carve-out becomes a deny of
    its entries that hold no writable path."""
    home = _home(home)
    allow = _dedupe(_real(p) for p in policy.get("writable") or [])
    if policy.get("writable_regex"):
        allow.append(os.path.join(home, ".claude.json*"))
    deny = [_real(p) for p in policy.get("deny_write") or []]
    for rx in policy.get("deny_write_regex") or []:
        deny += _regex_globs(rx)
    for ro in policy.get("readonly") or []:
        inner = [w for w in policy.get("writable") or [] if w != ro and _under(w, ro)]
        try:
            names = sorted(os.listdir(ro))
        except OSError:
            names = []
        for name in names:
            entry = os.path.join(ro, name)
            if not any(_under(w, entry) for w in inner):
                deny.append(_real(entry))
    return {
        "network": {
            "allowedDomains": _dedupe(allowed_domains),
            "deniedDomains": _dedupe(denied_domains),
            "allowUnixSockets": list(sockets),
            "allowAllUnixSockets": False,
        },
        "filesystem": {
            "allowWrite": allow,
            "denyWrite": _dedupe(deny),
            "denyRead": _dedupe(_real(p) for p in policy.get("deny_read") or []),
        },
        "allowPty": True,
    }


# --- denial detection --------------------------------------------------------

DENIAL_MARKERS = ("operation not permitted", "permission denied",
                  "read-only file system")
_MARKER_RE = re.compile("|".join(re.escape(m) for m in DENIAL_MARKERS), re.IGNORECASE)
# GNU coreutils quotes names as 'x' in the C locale but as \u2018x\u2019 under any
# UTF-8 locale (an ordinary Linux desktop): both, or a Linux denial is missed.
_QUOTED_RE = re.compile("'([^'\\n]+)'|\"([^\"\\n]+)\"|\u2018([^\u2019\\n]+)\u2019")
# Tools/syscalls that only READ. The sandbox never denies a read outside
# deny_read, so such a failure is never a sandbox write denial (macOS TCC
# dirs, root-only system dirs under `find /`).
_READ_PREFIX_RE = re.compile(
    r"^\s*(?:find|ls|du|cat|grep|egrep|fgrep|rg|ag|stat|head|tail|less|more|"
    r"wc|tree|file|readlink|realpath|md5|shasum|sha256sum|diff)\s*:", re.IGNORECASE)
_READ_SYSCALL_RE = re.compile(
    "\\b(?:scandir|opendir|readdir|lstat|stat|access|readlink|realpath)\\b\\s*['\u2018]",
    re.IGNORECASE)


def _line_op(line):
    if _READ_PREFIX_RE.search(line) or _READ_SYSCALL_RE.search(line):
        return "read"
    return "write"


def _candidates(line):
    out = []
    for m in _QUOTED_RE.finditer(line):
        tok = (m.group(1) or m.group(2) or m.group(3)).strip()
        if tok:
            out.append(tok)
    stripped = _QUOTED_RE.sub(" ", line)
    for tok in stripped.split():
        tok = tok.rstrip(":,;")
        if tok.startswith(("/", "./", "../")) and len(tok) > 1 or tok == "/":
            out.append(tok)
    return out


def _nearest_existing(path):
    p = path
    while p and not os.path.lexists(p):
        parent = os.path.dirname(p)
        if parent == p:
            break
        p = parent
    return p or "/"


def _event_texts(event):
    """Text a pi `tool_execution_end` record carries."""
    if not isinstance(event, dict):
        return []
    result = event.get("result")
    texts = []
    if isinstance(result, dict):
        content = result.get("content")
        if isinstance(content, list):
            for block in content:
                if isinstance(block, dict) and isinstance(block.get("text"), str):
                    texts.append(block["text"])
        elif isinstance(content, str):
            texts.append(content)
    elif isinstance(result, str):
        texts.append(result)
    return texts


def detect_denials(result_events, stderr, cwd, policy, access=os.access):
    """Sandbox denials in a finished pi run: [{path, op, source[, intentional]}].

    Sources: `tool_execution_end` records' `result.content[].text` and the
    child's stderr. A candidate path is a sandbox denial only if, judged
    from this UNSANDBOXED parent, it is outside the writable set AND its
    nearest existing ancestor is writable by the user (otherwise it is an
    ordinary permission error). A candidate inside deny_read is reported
    with `intentional: True` (never granted, never escalated)."""
    policy = policy or {}
    writable = policy.get("writable") or []
    deny_read = policy.get("deny_read") or []
    deny_write = policy.get("deny_write") or []
    deny_write_rx = [re.compile(r) for r in policy.get("deny_write_regex") or []]
    cwd = os.path.realpath(cwd or os.getcwd())
    sources = []
    for ev in result_events or []:
        for text in _event_texts(ev):
            sources.append(("tool", text))
    if stderr:
        sources.append(("stderr", stderr))
    seen = {}
    out = []
    for source, text in sources:
        for line in text.splitlines():
            if not _MARKER_RE.search(line):
                continue
            op = _line_op(line)
            for cand in _candidates(line):
                path = cand if os.path.isabs(cand) else os.path.join(cwd, cand)
                real = os.path.realpath(path)
                if real in seen:
                    continue
                if any(_under(real, r) for r in deny_read):
                    seen[real] = True
                    out.append({"path": real, "op": op, "source": source,
                                "intentional": True})
                    continue
                if op == "read":
                    continue
                if (any(_under(real, d) for d in deny_write)
                        or any(rx.search(real) for rx in deny_write_rx)):
                    # tamper protection / pi config: denied on purpose
                    seen[real] = True
                    out.append({"path": real, "op": op, "source": source,
                                "intentional": True})
                    continue
                if any(_under(real, w) for w in writable):
                    continue
                if not access(_nearest_existing(real), os.W_OK):
                    continue
                seen[real] = True
                out.append({"path": real, "op": op, "source": source})
    return out


# --- config ------------------------------------------------------------------


def normalize_config(raw, union=None):
    """Return a well-formed `sandbox` config. `union` carries the
    allow_write/deny_read lists already unioned across config layers by the
    caller; they replace whatever the plain merge produced."""
    raw = raw if isinstance(raw, dict) else {}
    mode = raw.get("mode")
    if mode is False:  # YAML 1.1 loads a bareword `off` as False
        mode = "off"
    if isinstance(mode, str) and mode.strip().lower() in MODES:
        mode = mode.strip().lower()
    else:
        mode = "auto"
    out = {"mode": mode}
    for key in ("allow_write", "deny_read"):
        values = (union or {}).get(key, raw.get(key))
        out[key] = _dedupe(v for v in (values or []) if isinstance(v, str))
    out["network"] = normalize_network(raw.get("network"), (union or {}).get("network"))
    return out


def normalize_network(raw, union=None):
    """A well-formed `sandbox.network`: mode allowlist|off and the domain
    lists (already unioned across layers by the caller when `union` is set).
    Missing lists fall back to the defaults (api.anthropic.com only)."""
    raw = raw if isinstance(raw, dict) else {}
    mode = raw.get("mode")
    if mode is False:  # YAML 1.1 `off`
        mode = "off"
    mode = mode.strip().lower() if isinstance(mode, str) else ""
    out = {"mode": mode if mode in NETWORK_MODES else "allowlist"}
    for key, default in (("allowed_domains", DEFAULT_ALLOWED_DOMAINS),
                         ("denied_domains", ()),
                         ("quiet_domains", DEFAULT_QUIET_DOMAINS)):
        values = (union or {}).get(key, raw.get(key, list(default)))
        out[key] = _dedupe(v.strip().lower() for v in (values or [])
                           if isinstance(v, str) and v.strip())
    return out


def domain_matches(host, patterns):
    """`host` equals a pattern, or a `*.suffix` pattern matches a subdomain."""
    host = (host or "").strip().lower().rstrip(".")
    for pattern in patterns or ():
        if pattern.startswith("*."):
            if host.endswith(pattern[1:]):
                return True
        elif host == pattern:
            return True
    return False


# --- policy ------------------------------------------------------------------


def _git_dirs(tree):
    """(own git dir, common git dir) of `tree`, or (None, None) outside git."""
    try:
        proc = subprocess.run(
            ["git", "-C", tree, "rev-parse", "--absolute-git-dir",
             "--git-common-dir"],
            capture_output=True, text=True, timeout=30,
        )
    except (OSError, subprocess.SubprocessError):
        return None, None
    lines = proc.stdout.splitlines()
    if proc.returncode != 0 or len(lines) < 2:
        return None, None
    own = os.path.realpath(lines[0])
    common = lines[1]
    if not os.path.isabs(common):
        common = os.path.join(tree, common)
    return own, os.path.realpath(common)


def executor_state_dirs(executor, home, env):
    if executor == "pi":
        # pi mkdirs `auth.json.lock` / `settings.json.lock` NEXT TO those
        # files on every run (and rewrites auth.json on token refresh); with
        # only `sessions/` writable the credential read fails with EPERM and
        # the run aborts. Measured with pi 1.1.0.
        base = env.get("PI_CODING_AGENT_DIR") or "~/.pi/agent"
        dirs = [base]
        # A managed agent dir symlinks auth.json to the user's real file; pi's
        # lockfile and token refresh write next to the real file.
        auth = os.path.join(_expand(base, home), "auth.json")
        if os.path.islink(auth):
            real_dir = os.path.dirname(os.path.realpath(auth))
            if real_dir not in dirs:
                dirs.append(real_dir)
        return dirs
    if executor == "codex":
        return [env.get("CODEX_HOME") or "~/.codex"]
    return []


def _repo_root(tree):
    try:
        proc = subprocess.run(["git", "-C", tree, "rev-parse", "--show-toplevel"],
                              capture_output=True, text=True, timeout=30)
    except (OSError, subprocess.SubprocessError):
        return None
    if proc.returncode != 0 or not proc.stdout.strip():
        return None
    return os.path.realpath(proc.stdout.strip())


def _rx_escape(text):
    # Only POSIX ERE metacharacters: Python's re.escape also escapes `-`,
    # which SBPL's regex engine need not accept.
    return re.sub(r"([.^$*+?()\[\]{}|\\])", r"\\\1", text)


def repo_config_regex(root):
    """SBPL/Python regex matching every `.claude/orchestra*.y*ml` under `root`."""
    return "^%s/(.*/)?\\.claude/orchestra[^/]*\\.y[^/]*ml$" % _rx_escape(root.rstrip("/"))


def _repo_config_files(root, limit=200):
    """Existing `.claude/orchestra*.y*ml` files under `root` (for backends
    that can only protect what exists), skipping .git and node_modules."""
    found = []
    rx = re.compile(repo_config_regex(root))
    for dirpath, dirnames, _files in os.walk(root):
        dirnames[:] = [d for d in dirnames if d not in (".git", "node_modules")]
        if os.path.basename(dirpath) == ".claude":
            for name in sorted(os.listdir(dirpath)):
                full = os.path.join(dirpath, name)
                if rx.search(full) and os.path.isfile(full):
                    found.append(full)
                    if len(found) >= limit:
                        return found
    return found


def pi_deny_paths(state_dir):
    state_dir = os.path.realpath(state_dir)
    return ([os.path.join(state_dir, d) for d in PI_DENY_SUBDIRS]
            + [os.path.join(state_dir, f) for f in PI_DENY_FILES])


def build_policy(tree, executor, cfg, home=None, env=None, platform=None,
                 learned=None):
    """The one policy every backend enforces.

    Returns {"writable", "readonly", "deny_read", "deny_write",
    "deny_write_regex", "learned"}, paths realpath'd. `readonly` lists
    carve-outs INSIDE a writable path that stay read-only (a non-isolated
    checkout's own `.git`); writable entries under a carve-out (its
    `objects`) are re-allowed after it. `deny_write` (subpaths) and
    `deny_write_regex` are emitted AFTER every allow and win over all of
    them: tamper protection for orchestra's own state/config and pi's
    code-loading config. `learned` overrides the per-machine grants store
    (tests); None reads it from `home`."""
    env = os.environ if env is None else env
    home = home or os.path.expanduser("~")
    platform = platform or sys.platform
    cfg = cfg if isinstance(cfg, dict) else {}
    tree = os.path.realpath(tree)

    writable = [tree]
    readonly = []
    own_git, common_git = _git_dirs(tree)
    if common_git is not None:
        # A linked worktree's own `.git/worktrees/<name>` (index, HEAD) and
        # the shared object store. The rest of the common dir -- refs,
        # config, hooks -- stays read-only.
        if own_git != common_git:
            writable.append(own_git)
        writable.append(os.path.join(common_git, "objects"))

    tmp = env.get("TMPDIR")
    if tmp:
        writable.append(os.path.realpath(tmp))
    writable.append(os.path.realpath("/private/tmp" if platform == "darwin" else "/tmp"))
    writable.append("/dev")

    deny_write = list(tamper_paths(home))
    for p in executor_state_dirs(executor, home, env):
        state = _expand(p, home)
        writable.append(state)
        if executor == "pi":
            deny_write += pi_deny_paths(state)
    seeds = list(SEED_CACHE_WRITES)
    if platform == "darwin":
        seeds += SEED_CACHE_WRITES_DARWIN
    for p in seeds:
        writable.append(_expand(p, home))
    for p in cfg.get("allow_write") or []:
        writable.append(_expand(p, home))
    deny_read = [_expand(p, home) for p in DEFAULT_DENY_READ]
    deny_read += [_expand(p, home) for p in cfg.get("deny_read") or []]
    if learned is None:
        learned, _warning = load_learned(home, deny_read)
    for g in learned:
        if isinstance(g, dict) and isinstance(g.get("path"), str):
            writable.append(os.path.realpath(g["path"]))

    deny_write_regex = []
    root = _repo_root(tree)
    if root is not None:
        deny_write_regex.append(repo_config_regex(root))

    # The common git dir must stay read-only even when it sits inside
    # something writable (a non-isolated checkout's own `.git`, or a repo
    # that lives under temp): carve it out, re-allowing only the entries
    # above that live inside it.
    if common_git is not None and any(
            _under(common_git, w) for w in writable if w != common_git):
        readonly.append(common_git)

    return {
        "writable": _dedupe(writable),
        "readonly": _dedupe(readonly),
        "deny_read": _dedupe(deny_read),
        "deny_write": _dedupe(deny_write),
        "deny_write_regex": deny_write_regex,
        "tamper": tamper_paths(home),
        "repo_root": root,
        "learned": [g["path"] for g in learned if isinstance(g, dict) and g.get("path")],
    }


# --- the herdr host ----------------------------------------------------------

# What Claude Code must not be able to plant in its own config dir (and in
# the shared dirs its symlinked children point into): loaded as settings,
# code or instructions by a later, unsandboxed session.
CLAUDE_TAMPER_NAMES = ("settings.json", "settings.local.json", "hooks", "skills",
                       "commands", "agents", "plugins", "CLAUDE.md")


def claude_paths(home=None, env=None):
    """(writable, deny_write) for a Claude Code session: the realpath of its
    config dir and of every symlinked child of it (a managed config dir links
    projects/sessions/... into a shared dir; a missing one makes transcript
    writes fail with EPERM), and the code/instruction names under those dirs."""
    env = os.environ if env is None else env
    home = _home(home)
    real = os.path.realpath(_expand(env.get("CLAUDE_CONFIG_DIR") or "~/.claude", home))
    writable, parents = [real], [real]
    if os.path.isdir(real):
        for entry in sorted(os.listdir(real)):
            path = os.path.join(real, entry)
            if os.path.islink(path):
                target = os.path.realpath(path)
                writable.append(target)
                parents.append(os.path.dirname(target))
    deny = []
    for parent in _dedupe(parents):
        for name in CLAUDE_TAMPER_NAMES:
            path = os.path.join(parent, name)
            deny.append(path)
            if os.path.lexists(path):
                deny.append(os.path.realpath(path))
    return _dedupe(writable), _dedupe(deny)


def host_policy(repo, worktree_base, herdr_home, cfg, extra_writable=(), home=None,
                env=None, platform=None, learned=None):
    """The policy for the herdr SERVER, which confines every pane and the
    Claude Code inside. Same shape as `build_policy`. Writable: the orchestra
    worktree base of `repo` (never the repo's own tree), the git pieces a
    linked worktree needs, orchestra's state and herdr home, Claude's own
    state, temp and `sandbox.allow_write`. The tamper denies come last."""
    env = os.environ if env is None else env
    home = os.path.realpath(home or os.path.expanduser("~"))
    platform = platform or sys.platform
    cfg = cfg if isinstance(cfg, dict) else {}
    base = os.path.realpath(worktree_base)

    writable = [base, os.path.realpath(state_dir(home)), os.path.realpath(herdr_home)]
    writable += [os.path.realpath(p) for p in extra_writable if p]
    readonly = []
    _own, common_git = _git_dirs(repo)
    if common_git is not None:
        # New linked worktrees' own dirs and the shared object store; refs,
        # config and hooks stay read-only.
        writable += [os.path.join(common_git, "worktrees"),
                     os.path.join(common_git, "objects")]
    claude_writable, claude_deny = claude_paths(home, env)
    writable += claude_writable
    writable.append(os.path.join(home, ".local", "state", "claude"))
    # The same shared package caches a CLI executor may write (~/.cache
    # covers ~/.cache/claude): without them `npm install` or a go build in a
    # hosted pane fails with EPERM.
    seeds = list(SEED_CACHE_WRITES)
    if platform == "darwin":
        seeds += SEED_CACHE_WRITES_DARWIN
    writable += [_expand(p, home) for p in seeds]
    tmp = env.get("TMPDIR")
    if tmp:
        writable.append(os.path.realpath(tmp))
    writable.append(os.path.realpath("/private/tmp" if platform == "darwin" else "/tmp"))
    writable.append("/dev")
    for p in cfg.get("allow_write") or []:
        writable.append(_expand(p, home))
    # Claude Code keeps its login token in the macOS Keychain; denying the
    # keychain files leaves every hosted session "Not logged in". Only the
    # host drops this one entry - CLI executors keep it.
    deny_read = [_expand(p, home) for p in DEFAULT_DENY_READ
                 if p not in HOST_READABLE]
    deny_read += [_expand(p, home) for p in cfg.get("deny_read") or []]
    if learned is None:
        learned, _warning = load_learned(home, deny_read)
    for g in learned:
        if isinstance(g, dict) and isinstance(g.get("path"), str):
            writable.append(os.path.realpath(g["path"]))

    # `~/.claude.json` and its lock/temp siblings sit next to nothing else
    # Claude owns; Seatbelt matches them by regex, the others by what exists.
    writable_regex = ["^%s/\\.claude\\.json.*$" % _rx_escape(home)]
    if platform != "darwin":
        writable += glob.glob(os.path.join(glob.escape(home), ".claude.json*"))

    # The state dir itself stays writable (result files); only the learned
    # store inside it is protected, not the directory around it.
    state = os.path.realpath(state_dir(home))
    deny_write = [p for p in tamper_paths(home) if os.path.realpath(p) != state]
    deny_write += claude_deny
    # Binaries and config orchestra later runs (outside this sandbox's scope):
    # managed tools and the dedicated pi agent dir must not be replaceable.
    deny_write += [os.path.join(state, "tools"), os.path.join(state, "pi-agent")]
    # The network sandbox's settings and its audit log: written by the
    # executor outside the sandbox, never by a pane.
    deny_write += [os.path.join(state, SRT_SETTINGS_DIR), os.path.join(state, AUDIT_DIR)]
    deny_write_regex = []
    for tree in (repo, base):
        root = _repo_root(tree) if os.path.isdir(tree) else None
        deny_write_regex.append(repo_config_regex(root or os.path.realpath(tree)))

    if common_git is not None and any(
            _under(common_git, w) for w in writable if w != common_git):
        readonly.append(common_git)
    return {
        "writable": _dedupe(writable),
        "writable_regex": writable_regex,
        "readonly": _dedupe(readonly),
        "deny_read": _dedupe(deny_read),
        "deny_write": _dedupe(deny_write),
        "deny_write_regex": deny_write_regex,
        "tamper": _dedupe(deny_write),
        "repo_root": _repo_root(repo),
        "learned": [g["path"] for g in learned if isinstance(g, dict) and g.get("path")],
    }


def prepare_host(cfg, repo, worktree_base, herdr_home, extra_writable=(),
                 probe_results=None, home=None, env=None, platform=None, learned=None):
    """Like `prepare`, for the herdr server: Seatbelt or bubblewrap. Landlock
    cannot keep the learned-grants store read-only inside the writable state
    dir, so it counts as no backend."""
    cfg = normalize_config(cfg)
    mode = cfg["mode"]
    spec = {"backend": "none", "enforced": False, "reason": "", "limits": [],
            "writable": [], "refuse": False, "policy": None, "mode": mode}
    if mode == "off":
        spec["reason"] = "sandbox.mode is off"
        return spec
    results = probe_results if probe_results is not None else probes()
    backend = next((b for b in ("seatbelt", "bwrap") if results[b]["ok"]), None)
    if backend is None:
        detail = "; ".join("%s: %s" % (k, results[k]["detail"]) for k in ("seatbelt", "bwrap"))
        spec["reason"] = ("no sandbox backend usable for the herdr host (%s)" % detail)
        if mode == "required":
            spec["refuse"] = True
        return spec
    policy = host_policy(repo, worktree_base, herdr_home, cfg, extra_writable, home,
                         env, platform, learned)
    spec.update(backend=backend, enforced=True, policy=policy,
                writable=list(policy["writable"]),
                reason=("macOS Seatbelt (sandbox-exec)" if backend == "seatbelt"
                        else "bubblewrap (user namespaces)"))
    if backend == "bwrap":
        spec["limits"] = bwrap_limits(policy)
    return spec


# --- probes (cached per process) --------------------------------------------

_PROBES = None

# Landlock UAPI (include/uapi/linux/landlock.h). Syscall numbers are the
# same on every architecture (generic table).
_SYS_CREATE_RULESET = 444
_SYS_ADD_RULE = 445
_SYS_RESTRICT_SELF = 446
_CREATE_RULESET_VERSION = 1 << 0
_RULE_PATH_BENEATH = 1
_PR_SET_NO_NEW_PRIVS = 38
_SCOPE_SIGNAL = 1 << 1

FS_EXECUTE = 1 << 0
FS_WRITE_FILE = 1 << 1
FS_READ_FILE = 1 << 2
FS_READ_DIR = 1 << 3
# Rights that apply to a regular file (a rule on a file with any other right
# is EINVAL): execute, write, read, truncate (ABI 3), ioctl_dev (ABI 5).
_FS_FILE_RIGHTS = FS_EXECUTE | FS_WRITE_FILE | FS_READ_FILE | (1 << 14) | (1 << 15)


def landlock_fs_rights(abi):
    """Every filesystem right the given ABI knows about."""
    bits = 13            # ABI 1: EXECUTE .. MAKE_SYM
    if abi >= 2:
        bits = 14        # REFER
    if abi >= 3:
        bits = 15        # TRUNCATE
    if abi >= 5:
        bits = 16        # IOCTL_DEV
    return (1 << bits) - 1


def _libc():
    return ctypes.CDLL(None, use_errno=True)


def landlock_abi(libc=None):
    """Kernel's Landlock ABI version, or 0 when unsupported/disabled."""
    if not sys.platform.startswith("linux"):
        return 0
    try:
        libc = libc or _libc()
        abi = libc.syscall(_SYS_CREATE_RULESET, None, ctypes.c_size_t(0),
                           ctypes.c_uint32(_CREATE_RULESET_VERSION))
    except (OSError, AttributeError):
        return 0
    return abi if isinstance(abi, int) and abi > 0 else 0


def _probe_seatbelt():
    if sys.platform != "darwin":
        return {"ok": False, "detail": "not macOS"}
    if not os.path.exists(SANDBOX_EXEC):
        return {"ok": False, "detail": "%s not found" % SANDBOX_EXEC}
    try:
        proc = subprocess.run(
            [SANDBOX_EXEC, "-p", "(version 1)(allow default)", "--", "/usr/bin/true"],
            capture_output=True, text=True, timeout=15,
        )
    except (OSError, subprocess.SubprocessError) as exc:
        return {"ok": False, "detail": "probe failed: %s" % exc}
    if proc.returncode != 0:
        # e.g. agent-exec is itself already inside a sandbox: sandbox_apply EPERM
        return {"ok": False, "detail": "probe exited %d: %s"
                % (proc.returncode, proc.stderr.strip()[:200])}
    return {"ok": True, "detail": "sandbox-exec probe ok"}


def _probe_bwrap():
    if not sys.platform.startswith("linux"):
        return {"ok": False, "detail": "not Linux"}
    exe = shutil.which("bwrap")
    if exe is None:
        return {"ok": False, "detail": "bwrap not on PATH"}
    try:
        proc = subprocess.run(
            [exe, "--ro-bind", "/", "/", "true"],
            capture_output=True, text=True, timeout=15,
        )
    except (OSError, subprocess.SubprocessError) as exc:
        return {"ok": False, "detail": "probe failed: %s" % exc}
    if proc.returncode != 0:
        # Ubuntu >= 24.04: AppArmor restricts unprivileged user namespaces.
        return {"ok": False, "detail": "probe exited %d: %s"
                % (proc.returncode, proc.stderr.strip()[:200])}
    return {"ok": True, "detail": "bwrap probe ok", "path": exe}


def _probe_landlock():
    if not sys.platform.startswith("linux"):
        return {"ok": False, "abi": 0, "detail": "not Linux"}
    abi = landlock_abi()
    if abi <= 0:
        return {"ok": False, "abi": 0, "detail": "kernel reports no Landlock support"}
    return {"ok": True, "abi": abi, "detail": "Landlock ABI %d" % abi}


def probes():
    """Backend availability on this host. Probed once per process; the
    force-env hook is re-read on every call so tests can flip it."""
    global _PROBES
    if os.environ.get(FORCE_ENV) == "none":
        off = {"ok": False, "detail": "forced unavailable by %s=none" % FORCE_ENV}
        return {"seatbelt": dict(off), "bwrap": dict(off),
                "landlock": dict(off, abi=0)}
    if _PROBES is None:
        _PROBES = {
            "seatbelt": _probe_seatbelt(),
            "bwrap": _probe_bwrap(),
            "landlock": _probe_landlock(),
        }
    return _PROBES


# --- spec --------------------------------------------------------------------


def prepare(executor, tree, cfg, probe_results=None, env=None):
    """Decide how `executor` will be sandboxed for a run in `tree`.

    Returns the internal spec; `report(spec)` is its public projection and
    `wrap_argv(argv, cwd, spec)` the argv to spawn. `spec["refuse"]` is True
    when `mode: required` found no backend -- the caller must not spawn."""
    cfg = normalize_config(cfg)
    mode = cfg["mode"]
    spec = {"backend": "none", "enforced": False, "reason": "", "limits": [],
            "writable": [], "refuse": False, "policy": None, "mode": mode}
    if mode == "off":
        spec["reason"] = "sandbox.mode is off"
        return spec
    if executor == "codex":
        spec.update(backend="codex-native", enforced=True, writable=None,
                    reason="codex applies its own sandbox (workspace-write); "
                           "nesting another one fails")
        return spec

    results = probe_results if probe_results is not None else probes()
    policy = build_policy(tree, executor, cfg, env=env)
    if results["seatbelt"]["ok"]:
        spec.update(backend="seatbelt", enforced=True,
                    reason="macOS Seatbelt (sandbox-exec)")
    elif results["bwrap"]["ok"]:
        spec.update(backend="bwrap", enforced=True,
                    reason="bubblewrap (user namespaces)",
                    limits=bwrap_limits(policy))
    elif results["landlock"]["ok"]:
        abi = results["landlock"]["abi"]
        limits = ["deny_read"]
        if abi < 6:
            limits.append("signal")
        if policy["readonly"]:
            limits.append("readonly")
        policy, extra = landlock_policy(policy, os.path.realpath(tree))
        limits += extra
        spec.update(backend="landlock", enforced=True, limits=limits, abi=abi,
                    reason="Landlock ABI %d (bwrap unavailable: %s)"
                           % (abi, results["bwrap"]["detail"]))
    else:
        detail = "; ".join("%s: %s" % (k, results[k]["detail"])
                           for k in ("seatbelt", "bwrap", "landlock"))
        if mode == "required":
            spec.update(refuse=True,
                        reason="sandbox.mode is required and no backend is "
                               "usable (%s)" % detail)
        else:
            spec["reason"] = "no sandbox backend usable; running unsandboxed (%s)" % detail
        return spec
    spec["policy"] = policy
    spec["writable"] = list(policy["writable"])
    return spec


def landlock_policy(policy, tree):
    """Landlock cannot deny inside an allowed subtree, so: drop every
    writable root other than the task tree that contains one of orchestra's
    tamper-protected files (the child's startup assertion then holds), and
    report what stays unprotected: pi's config inside its (necessarily
    writable) state dir, and the repo's `.claude/orchestra*.y*ml` inside the
    task tree. Returns (policy, extra_limits)."""
    policy = dict(policy)
    tamper = policy.get("tamper") or tamper_paths()
    policy["writable"] = [
        w for w in policy["writable"]
        if w == tree or not any(_under(t, w) or _under(w, t) for t in tamper)
    ]
    limits = []
    pi_config = [d for d in policy.get("deny_write") or [] if d not in tamper]
    if any(_under(d, w) for d in pi_config for w in policy["writable"]):
        limits.append("pi-config")
    if policy.get("deny_write_regex"):
        limits.append("orchestra-config")
    return policy, limits


def bwrap_limits(policy, exists=os.path.exists):
    """bwrap can only re-bind read-only what exists; a protected path that
    does not exist yet inside a writable root can still be created."""
    limits = []
    writable = policy.get("writable") or []
    tamper = set(policy.get("tamper") or tamper_paths())
    missing = [t for t in policy.get("deny_write") or []
               if not exists(t) and any(_under(t, w) for w in writable)]
    if any(t not in tamper for t in missing):
        limits.append("pi-config")
    if any(t in tamper for t in missing):
        limits.append("tamper")
    return limits


def child_env(spec, env):
    """Env additions for a sandboxed child: `uvx` must build its throwaway
    tool envs in temp, never in `~/.local/share/uv/tools` (installed tools,
    run later unsandboxed). An explicit UV_TOOL_DIR is left alone."""
    env = dict(env)
    if (spec or {}).get("backend") in ("seatbelt", "bwrap", "landlock") \
            and spec.get("policy") is not None and "UV_TOOL_DIR" not in env:
        tmp = env.get("TMPDIR") or "/tmp"
        env["UV_TOOL_DIR"] = os.path.join(os.path.realpath(tmp), UV_TOOL_DIR_NAME)
    return env


def unwrapped(reason):
    """Spec for a site that spawns nothing itself (delegated agents etc.)."""
    return {"backend": "none", "enforced": False, "reason": reason,
            "limits": [], "writable": [], "refuse": False, "policy": None}


def report(spec):
    return {k: spec.get(k) for k in ("backend", "enforced", "reason", "limits", "writable")}


# --- argv builders -----------------------------------------------------------


def seatbelt_profile(policy):
    """(SBPL text, -D params). Paths travel as params, so no quoting.

    `(allow default)` first, then denies: SBPL is last-match-wins, so each
    deny below overrides the blanket allow, and the carve-out re-allow comes
    after the deny it narrows."""
    params = []

    def param(prefix, path):
        key = "%s%d" % (prefix, len([p for p in params if p[0].startswith(prefix)]))
        params.append((key, path))
        return '(subpath (param "%s"))' % key

    rules = ["(version 1)", "(allow default)"]
    allow = [param("W", w) for w in policy["writable"]]
    allow += ['(regex #"%s")' % rx for rx in policy.get("writable_regex") or []
              if '"' not in rx]
    rules.append("(deny file-write* (require-not (require-any %s)))" % " ".join(allow))
    for ro in policy.get("readonly") or []:
        inner = [w for w in policy["writable"] if w != ro and _under(w, ro)]
        ro_rule = param("RO", ro)
        if inner:
            rules.append("(deny file-write* (require-all %s (require-not "
                         "(require-any %s))))"
                         % (ro_rule, " ".join(param("RW", w) for w in inner)))
        else:
            rules.append("(deny file-write* %s)" % ro_rule)
    if policy["deny_read"]:
        rules.append("(deny file-read* %s)"
                     % " ".join(param("R", r) for r in policy["deny_read"]))
    rules.append("(deny signal (require-not (target same-sandbox)))")
    # Tamper protection + pi config: after every allow, so no grant,
    # seed or allow_write entry can re-open them.
    deny = [param("D", d) for d in policy.get("deny_write") or []]
    for rx in policy.get("deny_write_regex") or []:
        if '"' not in rx:
            deny.append('(regex #"%s")' % rx)
    if deny:
        rules.append("(deny file-write* %s)" % " ".join(deny))
    return " ".join(rules), params


def _seatbelt_argv(argv, policy):
    sbpl, params = seatbelt_profile(policy)
    out = [SANDBOX_EXEC]
    for key, value in params:
        out += ["-D", "%s=%s" % (key, value)]
    return out + ["-p", sbpl, "--"] + list(argv)


def _bwrap_argv(argv, cwd, policy, exists=os.path.exists, isdir=os.path.isdir):
    out = ["bwrap", "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc"]
    readonly = policy.get("readonly") or []
    late = []
    for w in policy["writable"]:
        if w == "/dev" or not exists(w):
            continue
        if any(_under(w, ro) and w != ro for ro in readonly):
            late.append(w)
        else:
            out += ["--bind", w, w]
    for ro in readonly:
        if exists(ro):
            out += ["--ro-bind", ro, ro]
    for w in late:
        out += ["--bind", w, w]
    protected = list(policy.get("deny_write") or [])
    if policy.get("repo_root") and policy.get("deny_write_regex"):
        protected += _repo_config_files(policy["repo_root"])
    for d in protected:
        if exists(d):
            out += ["--ro-bind", d, d]
    for r in policy["deny_read"]:
        if not exists(r):
            continue
        if isdir(r):
            out += ["--tmpfs", r]
        else:
            out += ["--ro-bind", "/dev/null", r]
    out += ["--unshare-pid", "--die-with-parent", "--chdir", cwd, "--"]
    return out + list(argv)


def _landlock_argv(argv, policy, python=None, script=None):
    script = script or os.path.join(os.path.dirname(os.path.abspath(__file__)),
                                    "agent_exec.py")
    out = [python or sys.executable, script, "_sandbox-exec"]
    for w in policy["writable"]:
        out += ["--write", w]
    for t in policy.get("tamper") or tamper_paths():
        out += ["--protect", t]
    return out + ["--"] + list(argv)


def wrap_argv(argv, cwd, spec):
    """The argv to actually spawn for `argv` under `spec`. Every spawn path
    (capture, exec, DRYRUN preview) goes through here."""
    backend = (spec or {}).get("backend")
    policy = (spec or {}).get("policy")
    if policy is None or backend not in ("seatbelt", "bwrap", "landlock"):
        return list(argv)
    if backend == "seatbelt":
        return _seatbelt_argv(argv, policy)
    if backend == "bwrap":
        return _bwrap_argv(argv, os.path.realpath(cwd or os.getcwd()), policy)
    return _landlock_argv(argv, policy)


# --- landlock child (`agent_exec.py _sandbox-exec`) --------------------------


def ruleset_attr_bytes(handled_fs, scoped, abi):
    """struct landlock_ruleset_attr {u64 handled_access_fs; u64
    handled_access_net; u64 scoped;}, truncated to the fields `abi` knows."""
    return struct.pack("=QQQ", handled_fs, 0, scoped)[:_ruleset_attr_size(abi)]


def path_beneath_bytes(access, fd):
    """struct landlock_path_beneath_attr {u64 allowed_access; s32
    parent_fd;} __attribute__((packed))."""
    return struct.pack("=Qi", access, fd)


def _ruleset_attr_size(abi):
    # Pass only the fields this ABI knows; a larger size with unknown
    # non-zero fields is E2BIG on older kernels. net (ABI 4) stays unhandled
    # on purpose: network is allowed.
    if abi >= 6:
        return 24
    if abi >= 4:
        return 16
    return 8


def landlock_restrict(write_paths, libc=None, abi=None, open_fn=os.open,
                      close_fn=os.close, isdir=os.path.isdir):
    """Restrict THIS process: read+execute on `/`, full access on
    `write_paths`, signals scoped to the domain when ABI >= 6. Raises
    OSError on failure. Only ever called in the re-exec'd child."""
    libc = libc or _libc()
    abi = abi if abi is not None else landlock_abi(libc)
    if abi <= 0:
        raise OSError("Landlock is not supported by this kernel")
    handled = landlock_fs_rights(abi)
    attr = ruleset_attr_bytes(handled, _SCOPE_SIGNAL if abi >= 6 else 0, abi)
    ruleset = libc.syscall(_SYS_CREATE_RULESET, ctypes.create_string_buffer(attr, len(attr)),
                           ctypes.c_size_t(len(attr)), ctypes.c_uint32(0))
    if ruleset < 0:
        raise OSError(ctypes.get_errno(), "landlock_create_ruleset failed")
    try:
        grants = [("/", FS_EXECUTE | FS_READ_FILE | FS_READ_DIR)]
        grants += [(w, handled) for w in write_paths]
        for path, access in grants:
            try:
                fd = open_fn(path, getattr(os, "O_PATH", 0) | os.O_CLOEXEC)
            except OSError:
                continue  # a writable path that does not exist yet
            try:
                if not isdir(path):
                    access &= _FS_FILE_RIGHTS
                rule = path_beneath_bytes(access & handled, fd)
                rc = libc.syscall(_SYS_ADD_RULE, ctypes.c_int(ruleset),
                                  ctypes.c_int(_RULE_PATH_BENEATH),
                                  ctypes.create_string_buffer(rule, len(rule)),
                                  ctypes.c_uint32(0))
                if rc < 0:
                    raise OSError(ctypes.get_errno(),
                                  "landlock_add_rule failed for %s" % path)
            finally:
                close_fn(fd)
        if libc.prctl(_PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != 0:
            raise OSError(ctypes.get_errno(), "prctl(PR_SET_NO_NEW_PRIVS) failed")
        if libc.syscall(_SYS_RESTRICT_SELF, ctypes.c_int(ruleset),
                        ctypes.c_uint32(0)) < 0:
            raise OSError(ctypes.get_errno(), "landlock_restrict_self failed")
    finally:
        close_fn(ruleset)
    return abi


def parse_sandbox_exec_args(args, with_protect=False):
    writes = []
    protect = []
    i = 0
    while i < len(args):
        if args[i] == "--":
            if with_protect:
                return writes, protect, args[i + 1:]
            return writes, args[i + 1:]
        if args[i] in ("--write", "--protect") and i + 1 < len(args):
            (writes if args[i] == "--write" else protect).append(args[i + 1])
            i += 2
            continue
        raise ValueError("unexpected argument: %s" % args[i])
    raise ValueError("missing -- before the command")


def sandbox_exec_main(args, restrict=None, execvp=os.execvp):
    """Entry point of the hidden `_sandbox-exec` subcommand. Fails closed:
    if the restriction cannot be applied the command is not run."""
    try:
        writes, protect, argv = parse_sandbox_exec_args(args, with_protect=True)
    except ValueError as exc:
        sys.stderr.write("agent-exec: _sandbox-exec: %s\n" % exc)
        return 2
    if not argv:
        sys.stderr.write("agent-exec: _sandbox-exec: no command\n")
        return 2
    # Startup assertion: Landlock cannot deny inside an allowed root, so no
    # writable root may contain -- or sit inside -- orchestra's protected
    # state (fail closed).
    for t in protect:
        for w in writes:
            real_t, real_w = os.path.realpath(t), os.path.realpath(w)
            if _under(real_t, real_w) or _under(real_w, real_t):
                sys.stderr.write("agent-exec: _sandbox-exec: writable root %s "
                                 "contains protected %s\n" % (w, t))
                return 126
    try:
        (restrict or landlock_restrict)(writes)
    except OSError as exc:
        sys.stderr.write("agent-exec: _sandbox-exec: %s\n" % exc)
        return 126
    execvp(argv[0], argv)  # never returns
    return 127


# --- doctor ------------------------------------------------------------------


def doctor_section(cfg):
    """What this machine would use for a pi run, and why."""
    cfg = normalize_config(cfg)
    results = probes()
    spec = prepare("pi", os.getcwd(), cfg, probe_results=results)
    grants, warning = load_learned()
    return {
        "mode": cfg["mode"],
        "backend": spec["backend"],
        "enforced": spec["enforced"],
        "would_refuse": spec["refuse"],
        "reason": spec["reason"],
        "limits": spec["limits"],
        "codex": "codex-native",
        "probes": results,
        "learned": {"path": learned_path(), "count": len(grants),
                    "warning": warning},
    }


def list_section(cfg, workdir):
    """`agent-exec sandbox list`: effective writable set for a pi run in
    `workdir`, learned grants, deny_read, backend."""
    cfg = normalize_config(cfg)
    spec = prepare("pi", workdir, cfg)
    policy = spec.get("policy") or build_policy(workdir, "pi", cfg)
    grants, warning = load_learned()
    return {
        "workdir": os.path.realpath(workdir),
        "backend": spec["backend"],
        "enforced": spec["enforced"],
        "reason": spec["reason"],
        "limits": spec["limits"],
        "writable": policy["writable"],
        "deny_read": policy["deny_read"],
        "deny_write": policy.get("deny_write") or [],
        "deny_write_regex": policy.get("deny_write_regex") or [],
        "learned": grants,
        "learned_path": learned_path(),
        "warning": warning,
    }
