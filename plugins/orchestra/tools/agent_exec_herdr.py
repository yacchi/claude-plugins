# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
"""herdr executor for `agent-exec wave run`: Claude sessions that stay alive.

Same job and same interface as agent_exec_orca.OrcaExecutor, hosted by
herdr (a terminal multiplexer with an `agent` API) instead of Orca. A
package whose route resolves to a Claude tier runs in an interactive
Claude Code session -- within the user's subscription, never `claude -p` --
that stays open so corrections go back to the SAME session.

Every herdr call carries `--session <S>` (S = "orchestra-" + the first 8
characters of the Claude session id, falling back to the run id), so the
user's own herdr session is never touched. The server is started detached
when it is not running and stopped by `shutdown()` only when this executor
started it. Each package gets an orchestra-owned worktree (task
`pkg-<id>`, via isolate_create), one workspace and one named agent in it.
Completion is signalled only by the result file the prompt asks Claude to
write: `idle`/`done` mean the TUI settled, not that the task finished.

Orchestra owns its herdr: XDG_CONFIG_HOME points every herdr call (and the
server) at `<orchestra state dir>/hx`, so the user's herdr config and
sessions are never touched, and the server runs under the executor sandbox
(Seatbelt / bubblewrap) so every pane and the Claude inside is confined. The
folder-trust dialog is only auto-accepted for a worktree this executor
created, under a sandboxed server.

Sessions are persisted in `<state dir>/herdr-sessions.json`. Stdlib only;
agent_exec is imported lazily.
"""

import hashlib
import json
import os
import re
import shlex
import stat
import subprocess
import sys
import threading
import time
import uuid

_HERE = os.path.dirname(os.path.abspath(__file__))

REGISTRY_NAME = "herdr-sessions.json"
WARM_KEY = "_warm"  # registry key of the warm reader records; never a package id

_STATUS_TIMEOUT = 15
_CALL_TIMEOUT = 60
_PROMPT_RETRY_WAIT = 2
# Seconds to let Claude's TUI settle once its input box is drawn.
_INPUT_SETTLE = 1
_PERMISSION_MODES = frozenset(("acceptEdits", "auto", "default", "plan"))
_TRUST_MARKERS = ("trust this folder", "is this a project you")
_NAME_RE = re.compile(r"^[a-z][a-z0-9_-]{0,31}$")
# A unix socket path must stay under sun_path (104 bytes on macOS).
_SOCK_LIMIT = 100
_HOME_DIR = "hx"
# herdr settings orchestra pins in its own config.toml: no first-run
# onboarding, toasts, sounds, update checks or agent resume on restore.
_CONFIG_TOML = """\
onboarding = false

[update]
version_check = false

[ui.toast]
delivery = "off"

[ui.sound]
enabled = false

[session]
resume_agents_on_restore = false

[experimental]
pane_history = false
"""


def _agent_exec():
    if _HERE not in sys.path:
        sys.path.insert(0, _HERE)
    import agent_exec  # noqa: E402
    return agent_exec


def _orca():
    if _HERE not in sys.path:
        sys.path.insert(0, _HERE)
    import agent_exec_orca  # noqa: E402
    return agent_exec_orca


def _tools():
    if _HERE not in sys.path:
        sys.path.insert(0, _HERE)
    import agent_exec_tools  # noqa: E402
    return agent_exec_tools


def _sandbox():
    if _HERE not in sys.path:
        sys.path.insert(0, _HERE)
    import agent_exec_sandbox  # noqa: E402
    return agent_exec_sandbox


def _need(kind, detail):
    return _orca().OrcaNeed(kind, detail)


class _HerdrError(Exception):
    """One `herdr` call failed. `code` is its error code, if any."""

    def __init__(self, message, code=None, payload=None):
        Exception.__init__(self, message)
        self.code = code
        self.payload = payload if isinstance(payload, dict) else {}


def merged_config(cfg):
    """`cfg` over agent_exec.DEFAULTS["herdr"], `enabled` normalized."""
    defaults = _agent_exec().DEFAULTS["herdr"]
    out = dict(defaults)
    out["models"] = dict(defaults["models"])
    out["warm"] = dict(defaults.get("warm") or {})
    for key, value in (cfg or {}).items():
        if key == "models" and isinstance(value, dict):
            out["models"].update(value)
        elif key == "warm" and isinstance(value, dict):
            out["warm"].update(value)
        elif value is not None:
            out[key] = value
    out["enabled"] = _orca().normalize_enabled(out.get("enabled"))
    return out


def _find(payload, key):
    """First value stored under `key` anywhere in a parsed JSON payload."""
    if isinstance(payload, dict):
        if key in payload:
            return payload[key]
        for value in payload.values():
            found = _find(value, key)
            if found is not None:
                return found
    elif isinstance(payload, list):
        for value in payload:
            found = _find(value, key)
            if found is not None:
                return found
    return None


def _strings(payload):
    if isinstance(payload, str):
        return [payload]
    if isinstance(payload, dict):
        return [s for v in payload.values() for s in _strings(v)]
    if isinstance(payload, list):
        return [s for v in payload for s in _strings(v)]
    return []


def _server_running(out):
    try:
        payload = json.loads(out or "")
    except ValueError:
        payload = None
    if payload is not None:
        for key in ("running", "server_running"):
            value = _find(payload, key)
            if isinstance(value, bool):
                return value
    text = " ".join(_strings(payload)) if payload is not None else (out or "")
    text = (text or out or "").lower()
    if re.search(r"not running|not started|stopped|no server", text):
        return False
    return "running" in text


def _is_trust(text):
    lower = text.lower()
    return any(marker in lower for marker in _TRUST_MARKERS)


def _tail(text):
    return _orca()._excerpt("\n".join(text.splitlines()[-40:]))


def _read_result(path):
    return _orca()._read_result(path)


def _sanitize(text):
    return re.sub(r"[^a-z0-9_-]+", "-", text.lower()).strip("-_")


def agent_name(pkg):
    """A herdr agent name ([a-z][a-z0-9_-]{0,31}) unique per package."""
    name = "p-" + _sanitize(pkg)
    if len(name) > 32:
        name = name[:23] + "-" + hashlib.sha1(pkg.encode("utf-8")).hexdigest()[:8]
    return name if _NAME_RE.match(name) else "p-" + hashlib.sha1(pkg.encode("utf-8")).hexdigest()[:8]


def session_name(run_id=None, env=None):
    """"orchestra-" + 8 chars of the Claude session id, else of the run id."""
    env = os.environ if env is None else env
    component = _agent_exec()._session_component(env.get("CLAUDE_CODE_SESSION_ID"))
    if component is None:
        component = re.sub(r"[^a-z0-9]", "", (run_id or "").lower())[:8] or None
    return "orchestra-" + component if component else None


def server_env(env=None):
    """The environment of every herdr call and of the server: no Claude Code
    session markers (a child Claude inherits them and stops saving
    transcripts) and nothing from the user's own herdr. CLAUDE_CONFIG_DIR stays."""
    env = os.environ if env is None else env
    return {k: v for k, v in env.items()
            if not k.startswith(("CLAUDE_CODE_", "HERDR_"))
            and k not in ("CLAUDECODE", "CLAUDE_PID", "CLAUDE_EFFORT",
                          "XDG_CONFIG_HOME")}


def resolve_home(sname, base=None, fallback_root=None, uid=None, create=True):
    """The directory used as XDG_CONFIG_HOME for orchestra's herdr:
    `<orchestra state dir>/hx`, or `<tmp>/orchestra-<uid>/hx` when the
    longest socket path under it would not fit in a unix socket name. The
    fallback parent must be a private directory of this user."""
    base = base or os.path.join(_sandbox().state_dir(), _HOME_DIR)
    longest = os.path.join(base, "herdr", "sessions", sname, "herdr-client.sock")
    if len(longest.encode("utf-8")) < _SOCK_LIMIT:
        return base
    uid = os.getuid() if uid is None else uid
    root = fallback_root or ("/private/tmp" if sys.platform == "darwin" else "/tmp")
    parent = os.path.join(root, "orchestra-%d" % uid)
    if create and not os.path.lexists(parent):
        os.mkdir(parent, 0o700)
    if os.path.lexists(parent):
        info = os.lstat(parent)
        if (not stat.S_ISDIR(info.st_mode) or info.st_uid != uid
                or info.st_mode & 0o022):
            raise _need("error", "%s is not a private directory of this user "
                                 "(herdr home fallback)" % parent)
    return os.path.join(parent, _HOME_DIR)


def host_report(cfg=None, probe_results=None):
    """Doctor view of the herdr host: its home and whether the server would
    be sandboxed (and so whether `auto_trust` could apply)."""
    agent_exec = _agent_exec()
    cwd = os.getcwd()
    try:
        home = resolve_home("orchestra-00000000", create=False)
    except Exception as exc:
        return {"home": None, "sandboxed": False, "reason": str(getattr(exc, "detail", exc))}
    sandbox_cfg = (cfg or agent_exec.DEFAULTS).get("sandbox")
    spec = _sandbox().prepare_host(
        sandbox_cfg, cwd, agent_exec.isolate_home(cwd), home, probe_results=probe_results)
    return {"home": home, "sandboxed": bool(spec["enforced"] and spec["policy"]),
            "backend": spec["backend"], "reason": spec["reason"]}


class HerdrExecutor(object):
    """Runs packages in herdr-hosted Claude Code sessions.

    `runner` (subprocess.run-like), `popen`, `clock`/`sleep` are injectable
    so tests drive the loops against a fake herdr. `on_event(pkg, detail)`
    is the runner's hook for `orca-session` events.
    """

    host_name = "herdr"

    def __init__(self, cfg=None, binary=None, tools_cfg=None, runner=subprocess.run,
                 popen=subprocess.Popen, clock=time.time, sleep=time.sleep, poll=2.0,
                 on_event=None, env=None, herdr_home=None, sandbox_probes=None):
        self.cfg = merged_config(cfg)
        self.binary = binary
        self.tools_cfg = tools_cfg
        self.runner = runner
        self.popen = popen
        self.clock = clock
        self.sleep = sleep
        self.poll = poll
        self.on_event = on_event
        self.env = env
        self._home_override = herdr_home
        self._sandbox_probes = sandbox_probes
        self._home_cache = None
        self._sandboxed = False
        self._unsandboxed = None
        self._repo = None
        self.state_dir = None
        self.lock = threading.Lock()
        self.sname = session_name(env=env)
        self.server_started = False
        self._server_ready = False
        self._warm_paths = []
        self._warm_counts = {}
        self._warm_failed = set()
        self._warm_lock = threading.RLock()

    @classmethod
    def from_config(cls, **kwargs):
        resolved, _ = _agent_exec().resolve_config()
        resolved = resolved or {}
        return cls(cfg=resolved.get("herdr") or {}, tools_cfg=resolved, **kwargs)

    @property
    def enabled(self):
        return self.cfg["enabled"]

    def model_for(self, cls, route_model=None):
        return route_model or self.cfg["models"].get(cls) or "opus"

    def _binary(self):
        if self.binary:
            return self.binary
        cfg = self.tools_cfg if self.tools_cfg is not None else _agent_exec().DEFAULTS
        return _tools().resolve("herdr", cfg).get("path")

    def available(self, cfg=None):
        """(bool, reason): enabled, resolvable, and `herdr` answers."""
        merged = self.cfg if cfg is None else merged_config(cfg)
        if merged["enabled"] is False:
            return False, "disabled by config (herdr.enabled: false)"
        binary = self._binary()
        if not binary:
            return False, "herdr binary not found"
        try:
            proc = self.runner(
                [binary, "--session", self.sname or "orchestra-probe", "--version"],
                capture_output=True, text=True, stdin=subprocess.DEVNULL,
                timeout=_STATUS_TIMEOUT, env=self._env(self.sname or "orchestra-probe"))
        except _orca().OrcaNeed as exc:
            return False, exc.detail
        except (OSError, ValueError, subprocess.TimeoutExpired) as exc:
            return False, "could not run herdr: %s" % exc
        if proc.returncode != 0:
            return False, "herdr --version failed: %s" % (proc.stderr or proc.stdout or "").strip()
        return True, ""

    # -- herdr calls -----------------------------------------------------------

    def _h(self, sname=None):
        binary = self._binary()
        sname = sname or self.sname
        if not binary or not sname:
            raise _HerdrError("herdr is not configured (binary or session name missing)")
        return [binary, "--session", sname]

    def _home(self, sname=None):
        sname = sname or self.sname
        if self._home_override:
            return self._home_override
        if self._home_cache is None or self._home_cache[0] != sname:
            self._home_cache = (sname, resolve_home(sname))
        return self._home_cache[1]

    def _env(self, sname=None):
        """The environment of every herdr process: scrubbed, and pointed at
        orchestra's own herdr home so the user's herdr is never reached."""
        env = server_env(self.env)
        env["XDG_CONFIG_HOME"] = self._home(sname)
        return env

    def _call(self, args, timeout=_CALL_TIMEOUT, sname=None):
        """One herdr call. Returns (parsed payload or {}, raw stdout);
        raises _HerdrError on a non-zero exit."""
        argv = self._h(sname) + list(args)
        label = " ".join(args[:2])
        try:
            proc = self.runner(argv, capture_output=True, text=True,
                               stdin=subprocess.DEVNULL, timeout=timeout,
                               env=self._env(sname))
        except subprocess.TimeoutExpired:
            raise _HerdrError("herdr %s timed out after %ss" % (label, timeout))
        except (OSError, ValueError) as exc:
            raise _HerdrError("could not run herdr: %s" % exc)
        out = proc.stdout or ""
        payload = None
        # herdr prints its error object on stderr and leaves stdout empty.
        for text in (out, proc.stderr or ""):
            try:
                payload = json.loads(text)
                break
            except ValueError:
                continue
        error = payload.get("error") if isinstance(payload, dict) else None
        if proc.returncode != 0 or error:
            code = error.get("code") if isinstance(error, dict) else None
            message = (error.get("message") if isinstance(error, dict) else None) or (
                proc.stderr or out).strip() or "exit %s" % proc.returncode
            raise _HerdrError("herdr %s failed: %s" % (label, _orca()._excerpt(message)),
                              code, payload)
        return (payload if isinstance(payload, dict) else {}), out

    def _event(self, pkg, action, terminal, **extra):
        if self.on_event is None:
            return
        try:
            detail = {"action": action, "terminal": terminal}
            detail.update(extra)
            self.on_event(pkg, detail)
        except Exception:
            pass  # observability must never take a package down

    # -- server ----------------------------------------------------------------

    def _status_running(self):
        try:
            _, out = self._call(["status"], _STATUS_TIMEOUT)
        except _HerdrError:
            return False
        return _server_running(out)

    def _ensure_server(self):
        with self.lock:
            if self._server_ready:
                return
            if self._status_running():
                # Not ours: nothing says it runs under the sandbox.
                self._set_sandboxed(None, "the server was already running")
                self._server_ready = True
                return
            home = self._home()
            os.makedirs(os.path.join(home, "herdr"), mode=0o700, exist_ok=True)
            with open(os.path.join(home, "herdr", "config.toml"), "w",
                      encoding="utf-8") as fh:
                fh.write(_CONFIG_TOML)
            spec = self._host_spec(home)
            argv = _sandbox().wrap_argv(self._h() + ["server"], os.getcwd(), spec)
            env = _sandbox().child_env(spec, self._env())
            self._set_sandboxed(spec)
            log_dir = os.path.join(self.state_dir or os.getcwd(), "herdr")
            os.makedirs(log_dir, exist_ok=True)
            log = open(os.path.join(log_dir, self.sname + ".server.log"), "ab")
            try:
                self.popen(argv, stdin=subprocess.DEVNULL, stdout=log,
                           stderr=log, start_new_session=True, env=env)
            except OSError as exc:
                raise _need("error", "could not start the herdr server: %s" % exc)
            finally:
                log.close()
            self.server_started = True
            deadline = self.clock() + float(self.cfg["startup_timeout"])
            while True:
                if self._status_running():
                    self._server_ready = True
                    return
                if self.clock() >= deadline:
                    raise _need("error", "herdr server did not start")
                self.sleep(min(self.poll, 1.0) if self.poll else 0)

    def _host_spec(self, home):
        agent_exec = _agent_exec()
        cfg = self.tools_cfg if self.tools_cfg is not None else agent_exec.DEFAULTS
        repo = self._repo or os.getcwd()
        spec = _sandbox().prepare_host(
            cfg.get("sandbox"), repo, agent_exec.isolate_home(repo), home,
            extra_writable=[self.state_dir], probe_results=self._sandbox_probes)
        if spec["refuse"]:
            raise _need("error", spec["reason"])
        return spec

    def _set_sandboxed(self, spec, reason=None):
        """Remember whether the server confines its panes; say so once when
        it does not."""
        if spec is not None and spec["enforced"] and spec["policy"] is not None:
            self._sandboxed, self._unsandboxed = True, None
            return
        self._sandboxed = False
        self._unsandboxed = reason or spec["reason"]
        sys.stderr.write("agent-exec: herdr not sandboxed: %s\n" % self._unsandboxed)

    def shutdown(self):
        """Stop the server, but only if this executor started it and no
        registered session is left to resume (keep_sessions). Warm worktrees
        are removed either way; warm records alone never keep the server."""
        self._warm_cleanup()
        with self.lock:
            if not self.server_started:
                return
            if (self.cfg["keep_sessions"] and self.state_dir
                    and self._package_sessions()):
                return
            self.server_started = False
            self._server_ready = False
        try:
            self._call(["server", "stop"], _CALL_TIMEOUT)
        except _HerdrError as exc:
            sys.stderr.write("agent-exec: herdr: %s\n" % exc)

    # -- registry --------------------------------------------------------------

    def _registry_path(self, state_dir=None):
        return os.path.join(state_dir or self.state_dir, REGISTRY_NAME)

    def _load_registry(self, state_dir=None):
        try:
            with open(self._registry_path(state_dir), encoding="utf-8") as fh:
                data = json.load(fh)
        except (OSError, ValueError, UnicodeDecodeError):
            return {}
        return data if isinstance(data, dict) else {}

    def _package_sessions(self, state_dir=None):
        return dict((k, v) for k, v in self._load_registry(state_dir).items()
                    if k != WARM_KEY)

    def _save(self, session):
        if session.get("ephemeral"):
            return  # the warm reader lives under WARM_KEY, not as a package
        state_dir = session.get("state_dir") or self.state_dir
        with self.lock:
            data = self._load_registry(state_dir)
            data[session["pkg"]] = session
            self._write_registry(state_dir, data)

    def _forget(self, session):
        state_dir = session.get("state_dir") or self.state_dir
        with self.lock:
            data = self._load_registry(state_dir)
            if data.pop(session["pkg"], None) is not None:
                self._write_registry(state_dir, data)

    def _write_registry(self, state_dir, data):
        path = self._registry_path(state_dir)
        tmp = path + ".tmp"
        with open(tmp, "w", encoding="utf-8") as fh:
            json.dump(data, fh, ensure_ascii=False, indent=1, sort_keys=True)
            fh.write("\n")
        os.replace(tmp, path)

    def session(self, pkg, state_dir=None):
        """The registered session of `pkg`, or None. An agent herdr no longer
        knows is dropped from the session (the next prompt opens a new one on
        the same worktree); a worktree that is gone drops the whole entry."""
        state_dir = state_dir or self.state_dir
        entry = self._load_registry(state_dir).get(pkg) if pkg != WARM_KEY else None
        if not isinstance(entry, dict) or not entry.get("worktree"):
            return None
        if not os.path.isdir(entry["worktree"]):
            self._forget(dict(entry, pkg=pkg, state_dir=state_dir))
            return None
        if entry.get("terminal"):
            try:
                self._status(entry["terminal"], entry.get("herdr_session"))
            except _HerdrError:
                entry["terminal"] = None
                entry["workspace"] = None
                self._save(entry)
        return entry

    # -- agent -----------------------------------------------------------------

    def _status(self, name, sname=None):
        payload, _ = self._call(["agent", "get", name], sname=sname)
        status = _find(payload, "agent_status")
        return status if isinstance(status, str) else "unknown"

    def _screen(self, name):
        """The visible screen of an agent as text."""
        payload, out = self._call(["agent", "read", name, "--source", "visible"])
        if payload:
            return "\n".join(_strings(payload))
        return out

    def _native_args(self, session, fork=False):
        permission_mode = self.cfg.get("permission_mode", "acceptEdits")
        template = str(self.cfg.get("args") or "")
        if permission_mode not in _PERMISSION_MODES:
            raise _need("error", "invalid herdr.permission_mode: %s" % permission_mode)
        if permission_mode == "bypassPermissions" or (
                "bypassPermissions" in template
                or "--dangerously-skip-permissions" in template):
            raise _need("error", "herdr permission bypass is not allowed")
        text = template.replace("{model}", session.get("model") or "opus")
        text = text.replace("{permission_mode}", permission_mode)
        args = shlex.split(text)
        # The prompt files and the result file live outside the worktree;
        # without --add-dir the session stops on a permission dialog.
        for directory in session.get("add_dirs") or ():
            args += ["--add-dir", directory]
        if session.get("claude_session_id"):
            args += ["--session-id", session["claude_session_id"]]
        if fork and session.get("warm_session"):
            args += ["--resume", session["warm_session"], "--fork-session"]
        return args

    def _open_agent(self, session, fork=False):
        """Workspace + agent on the session's worktree, up to a usable
        prompt. Raises OrcaNeed on the trust dialog (unless auto_trust applies
        to a worktree this executor created) or any other failed start."""
        native = self._native_args(session, fork)
        self._repo = self._repo or session.get("repo")
        self._ensure_server()
        name = agent_name(session["pkg"])
        try:
            payload, _ = self._call(
                ["workspace", "create", "--cwd", session["worktree"],
                 "--label", "wave " + session["pkg"], "--no-focus"])
        except _HerdrError as exc:
            raise _need("error", str(exc))
        workspace = _find(payload, "workspace_id")
        pane = _find(payload, "pane_id")
        if not workspace or not pane:
            raise _need("error", "herdr workspace create returned no workspace/pane")
        session.update(workspace=workspace, pane=pane, terminal=name,
                       herdr_session=self.sname)
        self._save(session)

        ms = int(float(self.cfg["startup_timeout"]) * 1000)
        timeout = float(self.cfg["startup_timeout"]) + 30
        try:
            self._call(["agent", "start", name, "--kind", "claude", "--pane", pane,
                        "--timeout", str(ms), "--"] + native, timeout=timeout)
            return name
        except _HerdrError as exc:
            if exc.code != "agent_not_ready":
                raise _need("error", str(exc))
        try:
            screen = self._screen(name)
        except _HerdrError as exc:
            raise _need("error", str(exc))
        if not _is_trust(screen):
            raise _need("error", "agent not ready:\n%s" % _tail(screen))
        if not (self.cfg["auto_trust"] and session.get("created") and self._sandboxed):
            why = ""
            if self.cfg["auto_trust"] and session.get("created"):
                why = " (auto_trust not applied: herdr not sandboxed: %s)" % self._unsandboxed
            raise _need("trust", "%s needs the folder-trust dialog accepted once; "
                                 "open the herdr session %s (herdr home %s) and accept it%s"
                        % (session["worktree"], self.sname, self._home(), why))
        # The default option is "No, exit": move down first, then Enter.
        try:
            self._call(["agent", "send-keys", name, "down", "enter"])
            # A bare `wait` returns at once while the dialog is still
            # `blocked` (launch_pending); only `idle` means it took the keys.
            self._call(["agent", "wait", name, "--until", "idle", "--timeout", str(ms)],
                       timeout=timeout)
        except _HerdrError as exc:
            try:
                tail = _tail(self._screen(name))
            except _HerdrError:
                tail = ""
            raise _need("stalled", "folder-trust dialog did not accept: %s\n%s" % (exc, tail))
        self._await_input(name)
        self._event(session["pkg"], "trust", name)
        return name

    def _await_input(self, name):
        """After the trust dialog, `agent wait --until idle` can return before
        Claude has drawn its input box; a prompt sent then is swallowed while
        herdr still reports it ok, and the run waits for a result that never
        comes. Wait until the dialog is gone and the input box is on screen."""
        deadline = self.clock() + float(self.cfg["startup_timeout"])
        while True:
            try:
                screen = self._screen(name)
            except _HerdrError:
                screen = ""
            if not _is_trust(screen) and _orca()._has_prompt_box(screen.splitlines()):
                self.sleep(_INPUT_SETTLE)
                return
            if self.clock() >= deadline:
                raise _need("stalled", "input box never appeared after the folder-trust "
                                       "dialog:\n%s" % _tail(screen))
            self.sleep(min(self.poll, 1.0) if self.poll else 0)

    def _close_agent(self, session):
        workspace = session.get("workspace")
        if not workspace:
            session["terminal"] = None
            return
        handle = session.get("terminal")
        try:
            self._call(["workspace", "close", workspace])
        except _HerdrError as exc:
            sys.stderr.write("agent-exec: herdr: %s\n" % exc)
        session["terminal"] = None
        session["workspace"] = None
        session["pane"] = None
        self._save(session)
        self._event(session["pkg"], "close", handle)

    # -- warm reader -------------------------------------------------------------

    def configure_warm(self, read_paths, model_counts):
        """The runner's hook: repo-relative files the warm reader reads, and
        how many packages of the run start on each model."""
        self._warm_paths = list(read_paths or ())
        self._warm_counts = dict(model_counts or {})

    def _warm_records(self, state_dir):
        records = self._load_registry(state_dir).get(WARM_KEY)
        return records if isinstance(records, dict) else {}

    def _warm_put(self, state_dir, model, record):
        with self.lock:
            data = self._load_registry(state_dir)
            records = data.get(WARM_KEY) if isinstance(data.get(WARM_KEY), dict) else {}
            if record is None:
                records.pop(model, None)
            else:
                records[model] = record
            if records:
                data[WARM_KEY] = records
            else:
                data.pop(WARM_KEY, None)
            self._write_registry(state_dir, data)

    def _warm_drop(self, record):
        try:
            _agent_exec().isolate_remove(record["repo"], record["task"], force=True)
        except Exception as exc:
            sys.stderr.write("agent-exec: herdr: warm worktree removal failed: %s\n" % exc)

    def _warm_cleanup(self):
        """Remove every warm worktree of this state dir (host shutdown)."""
        if not self.state_dir:
            return
        for model, record in list(self._warm_records(self.state_dir).items()):
            if isinstance(record, dict) and record.get("task") and record.get("repo"):
                self._warm_drop(record)
            self._warm_put(self.state_dir, model, None)

    def _warm_for(self, pkg, repo, base_ref, model, state_dir, add_dirs):
        """The warm record to fork `pkg`'s session from, or None. Never
        raises: a failure is a `warm-failed` event and a fresh start."""
        warm = self.cfg.get("warm") or {}
        if (not warm.get("enabled") or not self._warm_paths
                or self._warm_counts.get(model, 0) < int(warm.get("min_packages", 2))):
            return None
        with self._warm_lock:
            if model in self._warm_failed:
                return None
            try:
                return self.ensure_warm(model, base_ref, self._warm_paths, repo, state_dir,
                                        add_dirs=add_dirs)
            except Exception as exc:
                self._warm_failed.add(model)
                self._event(pkg, "warm-failed", None, model=model,
                            reason=getattr(exc, "detail", None) or str(exc))
                return None

    def _warm_usable(self, record, repo, base_rev, paths, now):
        if (record.get("invalid") or record.get("requested") != paths
                or not os.path.isdir(record.get("worktree") or "")):
            return False
        ttl = float((self.cfg.get("warm") or {}).get("ttl_seconds", 3000))
        if now - float(record.get("last_used") or 0) >= ttl:
            return False
        if record.get("base_rev") == base_rev:
            return True
        rc, out = _agent_exec()._git(repo, "diff", "--name-only", str(record.get("base_rev")),
                                     base_rev)
        if rc != 0:
            return False
        changed = [line.strip() for line in out.splitlines() if line.strip()]
        return not any(c == f or c.startswith(f.rstrip("/") + "/")
                       for c in changed for f in paths)

    def ensure_warm(self, model, base_ref, read_paths, repo, state_dir, add_dirs=()):
        """The warm reader record for `model` at `base_ref`: reused while it
        is fresh and its files are unchanged, otherwise rebuilt. Raises on
        any failure (the caller falls back to a fresh start)."""
        agent_exec = _agent_exec()
        paths = list(read_paths)
        rc, out = agent_exec._git(repo, "rev-parse", "--verify", "%s^{commit}" % base_ref)
        base_rev = out.strip() if rc == 0 else ""
        if not base_rev:
            raise _need("error", "could not resolve %s to a commit" % base_ref)
        now = self.clock()
        record = self._warm_records(state_dir).get(model)
        if isinstance(record, dict):
            if self._warm_usable(record, repo, base_rev, paths, now):
                record["last_used"] = now
                self._warm_put(state_dir, model, record)
                return record
            self._warm_drop(record)
            self._warm_put(state_dir, model, None)

        resolved, config_error = agent_exec.resolve_config()
        if config_error is not None:
            raise _need("error", config_error)
        tag = _sanitize(model) or "model"
        task = "warm-" + tag
        created = agent_exec.isolate_create(repo, task, backend="git", onto=base_rev,
                                            carry=False)
        path = created.get("path")
        if created.get("status") not in ("created", "exists") or not path \
                or not os.path.isdir(path):
            raise _need("error", "could not create the warm worktree: %s"
                        % created.get("reason"))
        built = False
        try:
            files = [p for p in paths if os.path.isfile(os.path.join(path, p))]
            if not files:
                raise _need("error", "none of the warm files exist at %s" % base_ref)
            sid = str(uuid.uuid4())
            result_path = os.path.join(state_dir, "orca", task + ".result.json")
            os.makedirs(os.path.dirname(result_path), exist_ok=True)
            try:
                os.remove(result_path)
            except OSError:
                pass
            session = {
                "pkg": task, "worktree": path, "terminal": None, "workspace": None,
                "pane": None, "repo": repo, "model": model, "state_dir": state_dir,
                "created": created.get("status") == "created", "herdr_session": self.sname,
                "add_dirs": sorted(set(os.path.abspath(d)
                                       for d in list(add_dirs) + [state_dir])),
                "claude_session_id": sid, "ephemeral": True,
            }
            line = (
                "Read each of these files with the Read tool, using exactly these relative "
                "paths, and nothing else: %s. Do not run any other tool and do not edit "
                "anything. Then write %s containing JSON {\"status\": \"ok\"}."
                % (", ".join(files), result_path))
            try:
                name = self._open_agent(session)
                self._send_prompt(session, name, line, result_path)
                data = self._wait_result(
                    session, result_path,
                    float(self.cfg["startup_timeout"]) + float(self.cfg["task_timeout"]))
            finally:
                self._close_agent(session)
            if data.get("status") != "ok":
                raise _need("error", "warm reader reported %s" % data.get("status"))
            rc, out = agent_exec._git(path, "status", "--porcelain")
            if rc != 0 or out.strip():
                raise _need("error", "warm worktree is not clean after reading")
            record = {
                "session_id": sid, "worktree": path, "base_ref": base_ref,
                "base_rev": base_rev, "files": files, "requested": paths, "repo": repo,
                "task": task, "created_at": now, "last_used": now,
            }
            self._warm_put(state_dir, model, record)
            built = True
            return record
        finally:
            if not built:
                self._warm_drop({"repo": repo, "task": task})

    def _warm_leak(self, session):
        """Files a fork left dirty in the warm worktree (the record is then
        invalidated so the next package rebuilds it); [] when clean."""
        worktree = session.get("warm_worktree")
        if not worktree or not os.path.isdir(worktree):
            return []
        rc, out = _agent_exec()._git(worktree, "status", "--porcelain")
        if rc != 0 or not out.strip():
            return []
        state_dir = session.get("state_dir") or self.state_dir
        model = session.get("model")
        record = self._warm_records(state_dir).get(model)
        if isinstance(record, dict) and record.get("session_id") == session.get("warm_session"):
            record["invalid"] = True
            self._warm_put(state_dir, model, record)
        return [line[3:] if len(line) > 3 else line for line in out.splitlines()]

    # -- the contract ------------------------------------------------------------

    def start(self, pkg_id, repo, base_ref, cls, model, run_id, state_dir, add_dirs=()):
        """Create the orchestra worktree for `pkg-<id>` on `base_ref`, open a
        workspace and a Claude agent there and wait for it to be usable."""
        agent_exec = _agent_exec()
        self.state_dir = self.state_dir or state_dir
        if self.sname is None:
            self.sname = session_name(run_id, self.env)
        if self.sname is None:
            self.sname = "orchestra-" + hashlib.sha1(
                os.path.abspath(state_dir).encode("utf-8")).hexdigest()[:8]
        rc, out = agent_exec._git(repo, "rev-parse", "--verify", "%s^{commit}" % base_ref)
        base_sha = out.strip() if rc == 0 else ""
        if not base_sha:
            raise _need("error", "could not resolve %s to a commit" % base_ref)
        resolved, config_error = agent_exec.resolve_config()
        if config_error is not None:
            raise _need("error", config_error)
        isolation = (resolved or {}).get("isolation") or agent_exec.DEFAULTS["isolation"]
        add_dirs = sorted(set(os.path.abspath(d) for d in add_dirs))
        warm = self._warm_for(pkg_id, repo, base_ref, model, state_dir, add_dirs)
        created = agent_exec.isolate_create(
            repo, "pkg-" + pkg_id, backend="git", onto=base_sha,
            carry_extra=(isolation.get("carry_extra") or ()),
            carry_files=isolation.get("carry_files"))
        path = created.get("path")
        if created.get("status") not in ("created", "exists") or not path \
                or not os.path.isdir(path):
            raise _need("error", "could not create the worktree for %s: %s"
                        % (pkg_id, created.get("reason")))
        session = {
            "pkg": pkg_id, "worktree": path, "branch": created.get("branch"),
            "terminal": None, "workspace": None, "pane": None, "repo": repo,
            "model": model, "cls": cls, "state_dir": state_dir,
            "created": created.get("status") == "created",
            "herdr_session": self.sname,
            "add_dirs": add_dirs,
        }
        if warm:
            session.update(warm_session=warm["session_id"], warm_worktree=warm["worktree"])
        self._save(session)
        if created.get("carried") or created.get("carried_files"):
            self._event(pkg_id, "carry", None, method=created.get("carry_method", "none"),
                        files=len(created.get("carried_files") or []))
        try:
            try:
                name = self._open_agent(session, fork=bool(warm))
            except _orca().OrcaNeed as exc:
                if not warm or exc.kind != "error":
                    raise
                # The fork could not start (e.g. the warm transcript is gone):
                # warm never fails a package, so start it fresh instead.
                self._warm_failed.add(model)
                self._event(pkg_id, "warm-failed", None, model=model, reason=exc.detail)
                self._close_agent(session)
                for key in ("warm_session", "warm_worktree"):
                    session.pop(key, None)
                warm = None
                name = self._open_agent(session)
        except _orca().OrcaNeed:
            self.close(session, remove_worktree=True)
            raise
        self._event(pkg_id, "start", name, **({"warm": warm["session_id"]} if warm else {}))
        session["fresh"] = True  # the first prompt is not a resume
        if warm:
            session["fork_first"] = True
        return session

    def prompt(self, session, prompt_files, result_path, timeout):
        """Send ONE prompt line and wait for its result file. Returns a
        dispatch-shaped result dict; raises OrcaNeed on a stall or timeout."""
        fresh = session.pop("fresh", False)
        resumed = bool(session.get("terminal")) and not fresh
        if resumed:
            # A live agent is reused only while herdr still knows it.
            try:
                status = self._status(session["terminal"])
            except _HerdrError:
                status = None
            if status is None or status == "unknown":
                self._close_agent(session)
                resumed = False
        if not session.get("terminal"):
            self._event(session["pkg"], "start", self._open_agent(session))
        name = session["terminal"]
        if resumed:
            self._event(session["pkg"], "reuse", name)

        try:
            os.remove(result_path)
        except OSError:
            pass
        os.makedirs(os.path.dirname(os.path.abspath(result_path)), exist_ok=True)

        try:
            status = self._status(name)
        except _HerdrError as exc:
            raise _need("stalled", "agent %s went away: %s" % (name, exc))
        if status == "blocked":
            raise self._blocked(session, name, result_path)
        line = (
            "Read the worker prompt from %s and carry it out. When you are completely done, "
            "write %s containing JSON {\"status\": \"ok\"|\"escalate\", \"summary\": "
            "\"<=15 lines\"}. If you must ESCALATE, put the reason in summary."
            % (" and ".join(prompt_files), result_path)
        )
        if session.pop("fork_first", False) and session.get("warm_worktree"):
            line = (
                "Your working tree is %s. Earlier messages in this conversation were produced "
                "in %s; any path under it maps to the same relative path under your working "
                "tree. Never read or write under %s. "
                % (session["worktree"], session["warm_worktree"], session["warm_worktree"])
            ) + line
        self._send_prompt(session, name, line, result_path)

        try:
            data = self.wait_result(session, result_path, timeout)
        finally:
            if not self.cfg["keep_sessions"]:
                self._close_agent(session)

        summary = str(data.get("summary") or "")
        answer = ("ESCALATE: " + summary) if data["status"] == "escalate" else summary
        return {
            "status": "ok", "answer": answer, "session_id": name, "resumed": resumed,
            "executor": "herdr",
            "isolation": {"isolate": True, "path": session["worktree"],
                          "workdir": session["worktree"]},
        }

    def _send_prompt(self, session, name, line, result_path):
        """Send ONE prompt line. A stalled prompt is resent once after a short
        wait; only a second stall is a need."""
        for attempt in (0, 1):
            try:
                self._call(["agent", "prompt", name, line], timeout=_CALL_TIMEOUT)
                return
            except _HerdrError as exc:
                if exc.code == "agent_blocked":
                    raise self._blocked(session, name, result_path)
                if exc.code == "agent_prompt_stalled":
                    if attempt == 0:
                        self.sleep(_PROMPT_RETRY_WAIT)
                        continue
                    raise _need("stalled", "prompt stalled: %s\nterminal: %s\nworktree: %s\n"
                                           "result: %s" % (exc, name, session.get("worktree"),
                                                           result_path))
                raise _need("error", str(exc))

    def _blocked(self, session, name, result_path):
        try:
            screen = self._screen(name)
        except _HerdrError:
            screen = ""
        return _need(
            "stalled",
            "dialog:\n%s\nterminal: %s\nworktree: %s\nresult: %s"
            % (_tail(screen), name, session.get("worktree"), result_path))

    def wait_result(self, session, result_path, timeout):
        """Wait for a result file without sending another prompt. For a
        forked package the warm worktree must still be clean afterwards: a
        fork that wrote there is a need, whatever the outcome."""
        failure = None
        data = None
        try:
            data = self._wait_result(session, result_path, timeout)
        except _orca().OrcaNeed as exc:
            failure = exc
        leaked = self._warm_leak(session)
        if leaked:
            raise _need("error", "fork wrote outside its tree: %s" % ", ".join(leaked))
        if failure is not None:
            raise failure
        return data

    def _wait_result(self, session, result_path, timeout):
        name = session.get("terminal")
        if not name:
            raise _need("stalled", "agent is no longer registered")
        deadline = self.clock() + float(timeout)
        while True:
            data = _read_result(result_path)
            if data is not None:
                return data
            try:
                status = self._status(name)
            except _HerdrError as exc:
                raise _need("stalled", "agent %s went away: %s" % (name, exc))
            if status == "blocked":
                raise self._blocked(session, name, result_path)
            # idle/done: the TUI settled, but only the result file ends the task.
            if self.clock() >= deadline:
                tail = ""
                try:
                    tail = _tail(self._screen(name))
                except _HerdrError:
                    pass
                raise _need("timeout", "no result after %ss:\n%s" % (timeout, tail))
            if self.poll:
                self.sleep(self.poll)

    def close(self, session, remove_worktree):
        """Close the workspace; with `remove_worktree`, remove the worktree
        too. Errors are logged, never raised."""
        try:
            self._close_agent(session)
            if not remove_worktree:
                return
            repo = session.get("repo")
            if repo:
                try:
                    _agent_exec().isolate_remove(repo, "pkg-" + session["pkg"], force=True)
                except Exception as exc:
                    sys.stderr.write("agent-exec: herdr: worktree removal failed: %s\n" % exc)
            self._forget(session)
        except Exception as exc:
            sys.stderr.write("agent-exec: herdr: close failed: %s\n" % exc)
