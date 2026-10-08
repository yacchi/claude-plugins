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
import os
import shutil
import struct
import subprocess
import sys

MODES = ("auto", "required", "off")

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

# Package/build caches every toolchain a worker runs writes into.
SEED_CACHE_WRITES = (
    "~/.cache",            # uv, pip, pre-commit, generic XDG cache
    "~/.npm",              # npm / npx
    "~/go/pkg/mod",        # go module cache (GOMODCACHE)
    "~/.local/share/uv/tools",  # uvx: builds its ephemeral tool env here (EPERM without)
)
SEED_CACHE_WRITES_DARWIN = (
    "~/Library/Caches",    # go-build cache, Homebrew, pip on macOS
)

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
    return out


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
        return [env.get("PI_CODING_AGENT_DIR") or "~/.pi/agent"]
    if executor == "codex":
        return [env.get("CODEX_HOME") or "~/.codex"]
    return []


def build_policy(tree, executor, cfg, home=None, env=None, platform=None):
    """The one policy every backend enforces.

    Returns {"writable": [...], "readonly": [...], "deny_read": [...]}, all
    realpath'd. `readonly` lists carve-outs INSIDE a writable path that stay
    read-only (a non-isolated checkout's own `.git`); writable entries under
    a carve-out (its `objects`) are re-allowed after it."""
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

    for p in executor_state_dirs(executor, home, env):
        writable.append(_expand(p, home))
    seeds = list(SEED_CACHE_WRITES)
    if platform == "darwin":
        seeds += SEED_CACHE_WRITES_DARWIN
    for p in seeds:
        writable.append(_expand(p, home))
    for p in cfg.get("allow_write") or []:
        writable.append(_expand(p, home))

    # The common git dir must stay read-only even when it sits inside
    # something writable (a non-isolated checkout's own `.git`, or a repo
    # that lives under temp): carve it out, re-allowing only the entries
    # above that live inside it.
    if common_git is not None and any(
            _under(common_git, w) for w in writable if w != common_git):
        readonly.append(common_git)

    deny_read = [_expand(p, home) for p in DEFAULT_DENY_READ]
    deny_read += [_expand(p, home) for p in cfg.get("deny_read") or []]
    return {
        "writable": _dedupe(writable),
        "readonly": _dedupe(readonly),
        "deny_read": _dedupe(deny_read),
    }


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


def prepare(executor, tree, cfg, probe_results=None):
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
    policy = build_policy(tree, executor, cfg)
    if results["seatbelt"]["ok"]:
        spec.update(backend="seatbelt", enforced=True,
                    reason="macOS Seatbelt (sandbox-exec)")
    elif results["bwrap"]["ok"]:
        spec.update(backend="bwrap", enforced=True,
                    reason="bubblewrap (user namespaces)")
    elif results["landlock"]["ok"]:
        abi = results["landlock"]["abi"]
        limits = ["deny_read"]
        if abi < 6:
            limits.append("signal")
        if policy["readonly"]:
            limits.append("readonly")
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
    rules.append("(deny file-write* (require-not (require-any %s)))"
                 % " ".join(param("W", w) for w in policy["writable"]))
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


def parse_sandbox_exec_args(args):
    writes = []
    i = 0
    while i < len(args):
        if args[i] == "--":
            return writes, args[i + 1:]
        if args[i] == "--write" and i + 1 < len(args):
            writes.append(args[i + 1])
            i += 2
            continue
        raise ValueError("unexpected argument: %s" % args[i])
    raise ValueError("missing -- before the command")


def sandbox_exec_main(args, restrict=None, execvp=os.execvp):
    """Entry point of the hidden `_sandbox-exec` subcommand. Fails closed:
    if the restriction cannot be applied the command is not run."""
    try:
        writes, argv = parse_sandbox_exec_args(args)
    except ValueError as exc:
        sys.stderr.write("agent-exec: _sandbox-exec: %s\n" % exc)
        return 2
    if not argv:
        sys.stderr.write("agent-exec: _sandbox-exec: no command\n")
        return 2
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
    return {
        "mode": cfg["mode"],
        "backend": spec["backend"],
        "enforced": spec["enforced"],
        "would_refuse": spec["refuse"],
        "reason": spec["reason"],
        "limits": spec["limits"],
        "codex": "codex-native",
        "probes": results,
    }
