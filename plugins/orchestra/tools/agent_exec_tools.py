"""Orchestra-managed executor tools (pi, optionally others) and pi's dedicated
agent directory.

Orchestra installs pinned tool versions into its own state dir
(`<state_dir>/tools`) so a user's personal install cannot change worker
behavior. Every external effect (subprocess, PATH lookup, clock, home) is
injectable so the module is testable without network or a real toolchain.

Config (`tools:` in orchestra.yaml):

    tools:
      installer: auto          # auto | mise | npm
      pi:
        source: auto           # auto | managed | system
        spec: "npm:@earendil-works/pi-coding-agent"
        version: "1.2.0"
        bin: pi
"""

import datetime
import json
import os
import shutil
import subprocess
import tempfile

import agent_exec_sandbox

MANIFEST_VERSION = 1
INSTALL_HINT = "run: agent-exec tools install %s"
ERROR_TAIL = 500


# --- paths and manifest ------------------------------------------------------


def tools_root(home=None):
    return os.path.join(agent_exec_sandbox.state_dir(home), "tools")


def manifest_path(home=None):
    return os.path.join(tools_root(home), "manifest.json")


def load_manifest(home=None):
    """The installed-tools record. Missing or corrupt means empty; never raises."""
    empty = {"version": MANIFEST_VERSION, "tools": {}}
    try:
        with open(manifest_path(home), "r", encoding="utf-8") as fh:
            data = json.load(fh)
    except (OSError, ValueError):
        return empty
    if not isinstance(data, dict) or not isinstance(data.get("tools"), dict):
        return empty
    tools = {k: v for k, v in data["tools"].items() if isinstance(v, dict)}
    return {"version": MANIFEST_VERSION, "tools": tools}


def save_manifest(manifest, home=None):
    """Atomic write: temp file in the same directory, then os.replace."""
    path = manifest_path(home)
    directory = os.path.dirname(path)
    os.makedirs(directory, exist_ok=True)
    fd, tmp = tempfile.mkstemp(prefix=".manifest-", suffix=".tmp", dir=directory)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            json.dump(manifest, fh, indent=2, sort_keys=True)
            fh.write("\n")
        os.replace(tmp, path)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


# --- config ------------------------------------------------------------------


def _tools_section(cfg):
    section = cfg.get("tools") if isinstance(cfg, dict) else None
    return section if isinstance(section, dict) else {}


def tool_names(cfg):
    return [k for k, v in _tools_section(cfg).items() if isinstance(v, dict)]


def tool_config(name, cfg, default_bin=None):
    """Normalized per-tool config. A tool absent from `tools` is a plain
    system binary."""
    entry = _tools_section(cfg).get(name)
    if not isinstance(entry, dict):
        return {"source": "system", "spec": None, "version": None,
                "bin": default_bin or name}
    spec = entry.get("spec")
    version = entry.get("version")
    source = entry.get("source")
    if source not in ("auto", "managed", "system"):
        source = "auto"
    return {
        "source": source,
        "spec": spec if isinstance(spec, str) and spec else None,
        "version": str(version) if version not in (None, "") else None,
        "bin": entry.get("bin") if isinstance(entry.get("bin"), str) and entry.get("bin")
        else (default_bin or name),
    }


def _executable(path):
    return isinstance(path, str) and os.path.isabs(path) and os.path.isfile(path) \
        and os.access(path, os.X_OK)


# --- resolution --------------------------------------------------------------


def _record_state(record, tcfg):
    """(state, record): 'valid' | 'stale' | 'none'."""
    if not isinstance(record, dict):
        return "none"
    if record.get("spec") != tcfg["spec"] or record.get("version") != tcfg["version"]:
        return "stale"
    if not _executable(record.get("path")):
        return "stale"
    return "valid"


def resolve(name, cfg, which=None, home=None, default_bin=None):
    """Where does `name` run from? See the module docstring for the rules."""
    which = which or shutil.which
    tcfg = tool_config(name, cfg, default_bin)
    record = load_manifest(home)["tools"].get(name)
    state = _record_state(record, tcfg)
    hint = INSTALL_HINT % name
    has_spec = bool(tcfg["spec"] and tcfg["version"])
    out = {"name": name, "path": None, "source": None, "version": None,
           "wanted": tcfg["version"], "reason": None, "hint": None}

    def managed(reason=None, with_hint=False):
        out.update(path=record["path"], source="managed",
                   version=record.get("version"), reason=reason,
                   hint=hint if with_hint else None)
        return out

    def system():
        found = which(tcfg["bin"])
        if not found:
            return None
        out.update(path=os.path.abspath(found), source="system")
        return out

    source = tcfg["source"]
    if source == "managed":
        if state == "valid":
            return managed()
        out.update(reason="managed-not-installed" if record is None else "managed-stale",
                   hint=hint)
        return out
    if source == "system":
        if system() is None:
            out["reason"] = "not-on-path"
        return out
    # auto
    if state == "valid":
        return managed()
    if state == "stale" and _executable(record.get("path")):
        return managed(reason="managed-stale", with_hint=True)
    if system() is not None:
        if has_spec:
            out["hint"] = hint
        return out
    out.update(reason="not-installed", hint=hint if has_spec else None)
    return out


def status(cfg, which=None, home=None, known=None):
    """`resolve` for every configured tool plus every known executor.
    `known` maps executor name to its binary."""
    known = known or {}
    names = list(tool_names(cfg))
    names += [n for n in known if n not in names]
    return [resolve(n, cfg, which=which, home=home, default_bin=known.get(n))
            for n in names]


# --- installation ------------------------------------------------------------


def _tail(text):
    text = (text or "").strip()
    return text[-ERROR_TAIL:]


def _result(name, status_, installer=None, version=None, path=None, error=None):
    return {"name": name, "status": status_, "installer": installer,
            "version": version, "path": path, "error": error}


def mise_env(base_env, home=None):
    """(env, cwd): a fully private mise world so the user's global or project
    mise config is neither read nor modified."""
    root = os.path.join(tools_root(home), "mise")
    config = os.path.join(root, "config")
    env = dict(base_env)
    env.update({
        "MISE_DATA_DIR": os.path.join(root, "data"),
        "MISE_CONFIG_DIR": config,
        "MISE_GLOBAL_CONFIG_FILE": os.path.join(config, "config.toml"),
        "MISE_CACHE_DIR": os.path.join(root, "cache"),
        "MISE_STATE_DIR": os.path.join(root, "state"),
        "MISE_CEILING_PATHS": config,
        "MISE_YES": "1",
    })
    return env, config


def _install_mise(tcfg, run, base_env, home):
    env, cwd = mise_env(base_env, home)
    os.makedirs(cwd, exist_ok=True)
    target = "%s@%s" % (tcfg["spec"], tcfg["version"])
    for argv in (["mise", "install", target],
                 ["mise", "exec", target, "--", "which", tcfg["bin"]]):
        try:
            proc = run(argv, capture_output=True, text=True, env=env, cwd=cwd)
        except OSError as exc:
            return None, str(exc)
        if proc.returncode != 0:
            return None, _tail(proc.stderr) or "%s exited %d" % (argv[1], proc.returncode)
    lines = [ln.strip() for ln in (proc.stdout or "").splitlines() if ln.strip()]
    path = lines[-1] if lines else ""
    if not _executable(path):
        return None, "mise did not report an executable path for %s" % tcfg["bin"]
    return path, None


def _install_npm(name, tcfg, run, base_env, home):
    spec = tcfg["spec"]
    if not spec.startswith("npm:"):
        return None, "npm installer only supports npm: specs"
    prefix = os.path.join(tools_root(home), "npm", name, tcfg["version"])
    os.makedirs(prefix, exist_ok=True)
    argv = ["npm", "install", "--prefix", prefix, "--no-fund", "--no-audit",
            "%s@%s" % (spec[len("npm:"):], tcfg["version"])]
    try:
        proc = run(argv, capture_output=True, text=True, env=dict(base_env))
    except OSError as exc:
        return None, str(exc)
    if proc.returncode != 0:
        return None, _tail(proc.stderr) or "npm exited %d" % proc.returncode
    path = os.path.join(prefix, "node_modules", ".bin", tcfg["bin"])
    if not _executable(path):
        return None, "npm did not produce an executable at %s" % path
    return path, None


def install(names, cfg, installer=None, force=False, run=None,
            which=None, home=None, now=None, env=None, known=None):
    """Install each named tool; one result dict per name. The manifest is only
    touched after a successful install."""
    run = run or subprocess.run
    which = which or shutil.which
    base_env = os.environ if env is None else env
    known = known or {}
    chosen = installer or _tools_section(cfg).get("installer") or "auto"
    results = []
    for name in names:
        tcfg = tool_config(name, cfg, known.get(name))
        if not (tcfg["spec"] and tcfg["version"]):
            results.append(_result(name, "skipped"))
            continue
        record = load_manifest(home)["tools"].get(name)
        if not force and _record_state(record, tcfg) == "valid":
            results.append(_result(name, "already", record.get("installer"),
                                   record.get("version"), record.get("path")))
            continue
        how = chosen
        if how == "auto":
            how = "mise" if which("mise") else "npm"
        if how == "mise":
            path, error = _install_mise(tcfg, run, base_env, home)
        elif how == "npm":
            path, error = _install_npm(name, tcfg, run, base_env, home)
        else:
            path, error = None, "unknown installer: %s" % how
        if error is not None:
            results.append(_result(name, "failed", how, tcfg["version"], None, error))
            continue
        stamp = now() if now is not None else datetime.datetime.now(
            datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
        manifest = load_manifest(home)
        manifest["tools"][name] = {
            "spec": tcfg["spec"], "version": tcfg["version"], "installer": how,
            "path": path, "installed_at": stamp,
        }
        save_manifest(manifest, home)
        results.append(_result(name, "installed", how, tcfg["version"], path))
    return results


# --- pi's dedicated agent dir ------------------------------------------------


def pi_agent_dir(cfg, home=None):
    """(mode, dir): mode is managed | user | path; dir is None for user."""
    external = cfg.get("external_executors") if isinstance(cfg, dict) else None
    pi = external.get("pi") if isinstance(external, dict) else None
    value = pi.get("agent_dir") if isinstance(pi, dict) else None
    if value in (None, "", "managed"):
        return "managed", os.path.join(agent_exec_sandbox.state_dir(home), "pi-agent")
    if value == "user":
        return "user", None
    if isinstance(value, str):
        return "path", os.path.abspath(os.path.expanduser(value))
    return "managed", os.path.join(agent_exec_sandbox.state_dir(home), "pi-agent")


def pi_context_files(cfg):
    external = cfg.get("external_executors") if isinstance(cfg, dict) else None
    pi = external.get("pi") if isinstance(external, dict) else None
    return not (isinstance(pi, dict) and pi.get("context_files") is False)


def ensure_pi_agent_dir(directory, env=None, home=None):
    """Create the managed pi dir (0700) and, only when `auth.json` is absent,
    symlink it to the user's existing one so a login carries over. Never
    overwrites or removes an existing file or symlink."""
    env = os.environ if env is None else env
    os.makedirs(directory, mode=0o700, exist_ok=True)
    auth = os.path.join(directory, "auth.json")
    if os.path.lexists(auth):
        return
    override = env.get("PI_CODING_AGENT_DIR")
    if override and os.path.realpath(override) != os.path.realpath(directory):
        source = override
    else:
        source = os.path.join(home or os.path.expanduser("~"), ".pi", "agent")
    source_auth = os.path.join(source, "auth.json")
    if os.path.isfile(source_auth) and \
            os.path.realpath(source_auth) != os.path.realpath(auth):
        try:
            os.symlink(source_auth, auth)
        except OSError:
            pass


def pi_env_overrides(cfg, env=None, home=None):
    """Env additions for a pi child (ensuring the managed dir exists first).
    Empty in `user` mode."""
    mode, directory = pi_agent_dir(cfg, home)
    if mode == "user":
        return {}
    if mode == "managed":
        ensure_pi_agent_dir(directory, env=env, home=home)
    return {"PI_CODING_AGENT_DIR": directory}
