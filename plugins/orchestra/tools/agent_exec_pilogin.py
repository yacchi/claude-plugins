"""`agent-exec pi login`: Claude drives pi's interactive /login through
orchestra's own herdr, so the user only has to open a URL (and type a code).

pi has no non-interactive login, and the user does not run `agent-exec` or pi
in a terminal. This module starts the managed pi in a pane of a dedicated herdr
session (`orchestra-pilogin-<uid>`, never a worker session, not wrapped in
srt: it is user-initiated and only pi's auth UI runs), types the /login keys,
reads the screen, and prints the URL/code as JSON. `--wait` then polls
`pi auth check` until the login lands; `--paste` and `--cancel` finish or abort
a login in progress.

Stdlib only.
"""

import json
import os
import re
import shlex
import subprocess
import sys
import time

_HERE = os.path.dirname(os.path.abspath(__file__))

STATE_NAME = "pilogin.json"
DEFAULT_PROVIDER = "openai-codex"
DEFAULT_TIMEOUT = 600
_AUTH_POLL = 3
_STEP_TIMEOUT = 45
_START_TIMEOUT = 90
_AUTH_CHECK_TIMEOUT = 60

# provider id -> the label pi shows in "Select provider to configure".
PROVIDER_LABELS = {
    "openai-codex": "OpenAI Codex",
    "openai": "OpenAI",
    "anthropic": "Anthropic",
    "github-copilot": "GitHub Copilot",
}

_AUTH_METHOD_LABEL = "Sign in with an account"
_DEVICE_LABEL = "Device code login"
_PI_READY_MARKER = "/ commands"
_STOP_WORDS = ("cmd+click",)

USAGE = """\
Usage:
  agent-exec pi login [--provider ID] [--json]
  agent-exec pi login --wait [--timeout SEC] [--json]
  agent-exec pi login --paste TEXT [--timeout SEC] [--json]
  agent-exec pi login --cancel [--json]
"""


def _mod(name):
    if _HERE not in sys.path:
        sys.path.insert(0, _HERE)
    return __import__(name)


def _agent_exec():
    return _mod("agent_exec")


def _herdr():
    return _mod("agent_exec_herdr")


def _tools():
    return _mod("agent_exec_tools")


def _sandbox():
    return _mod("agent_exec_sandbox")


def session_name(uid=None):
    return "orchestra-pilogin-%d" % (os.getuid() if uid is None else uid)


# -- screen parsing ----------------------------------------------------------


def screen_text(out, payload=None):
    """The pane text of a `pane read`: its raw stdout, or the text of a JSON
    payload when herdr wraps it."""
    if isinstance(payload, dict):
        result = payload.get("result")
        if isinstance(result, dict) and isinstance(result.get("text"), str):
            return result["text"]
    return out or ""


def tail(text, lines=25):
    return "\n".join(l.rstrip() for l in (text or "").splitlines()[-lines:]).strip()


def _norm_label(label):
    label = re.split(r"\s+[•·]\s+", label, maxsplit=1)[0]
    label = re.sub(r"\s*\((legacy|default)\)", "", label, flags=re.IGNORECASE)
    return label.strip()


def highlighted(screen):
    """The label of the selected entry (the line marked with an arrow), or
    None. Annotations like `• not configured` and `(legacy)` are dropped."""
    for line in (screen or "").splitlines():
        match = re.match(r"^\s*[→❯]\s+(\S.*?)\s*$", line)
        if match:
            return _norm_label(match.group(1))
    return None


def extract_url(screen):
    """The login URL: from the line starting with https:// through the
    hard-wrapped continuation lines (each with a leading space), joined with no
    separator, stopping before the `Cmd+click` line, a blank line, or any line
    with whitespace inside."""
    lines = (screen or "").splitlines()
    for index, line in enumerate(lines):
        if not line.strip().startswith("https://"):
            continue
        parts = [line.strip()]
        for nxt in lines[index + 1:]:
            piece = nxt.strip()
            if (not piece or re.search(r"\s", piece)
                    or any(word in piece.lower() for word in _STOP_WORDS)):
                break
            parts.append(piece)
        return "".join(parts)
    return None


def extract_code(screen):
    match = re.search(r"Enter code:\s*([A-Za-z0-9][A-Za-z0-9-]*)", screen or "")
    return match.group(1) if match else None


def wants_paste(screen):
    lower = (screen or "").lower()
    return "paste" in lower and ("redirect url" in lower or "authorization code" in lower)


# -- providers ---------------------------------------------------------------


def configured_providers(cfg):
    """Provider prefixes of the pi models in the resolved config, in order."""
    external = cfg.get("external_executors") if isinstance(cfg, dict) else None
    pi = external.get("pi") if isinstance(external, dict) else None
    policy = pi.get("class_policy") if isinstance(pi, dict) else None
    found = []
    if isinstance(policy, dict):
        for entry in policy.values():
            model = entry.get("model") if isinstance(entry, dict) else None
            if isinstance(model, str) and "/" in model:
                provider = model.split("/", 1)[0]
                if provider and provider not in found:
                    found.append(provider)
    return found or [DEFAULT_PROVIDER]


# -- the login driver --------------------------------------------------------


class LoginError(Exception):
    """An unexpected screen: carries the step and the screen tail."""

    def __init__(self, step, screen="", reason=None):
        Exception.__init__(self, step)
        self.step = step
        self.screen = screen
        self.reason = reason


class PiLogin(object):
    def __init__(self, cfg=None, runner=subprocess.run, pi_runner=None, popen=subprocess.Popen,
                 sleep=time.sleep, clock=time.time, env=None, state_dir=None,
                 herdr_home=None, herdr_binary=None, pi_path=None, cwd=None, uid=None):
        herdr = _herdr()
        self.cfg = cfg if cfg is not None else _agent_exec()._cfg_or_defaults()
        self.runner = runner
        self.pi_runner = pi_runner or runner
        self.popen = popen
        self.sleep = sleep
        self.clock = clock
        self.env = os.environ if env is None else env
        self.state_dir = state_dir or _sandbox().state_dir()
        self.cwd = cwd or os.getcwd()
        self.sname = session_name(uid)
        self.pi_path = pi_path
        # The herdr plumbing (binary, scrubbed env, own XDG home, call parsing)
        # is HerdrExecutor's; only the server start differs (no srt).
        self.hx = herdr.HerdrExecutor(
            cfg=(self.cfg.get("herdr") if isinstance(self.cfg, dict) else None) or {},
            binary=herdr_binary, tools_cfg=self.cfg, runner=runner, popen=popen,
            clock=clock, sleep=sleep, env=self.env, herdr_home=herdr_home, poll=0)
        self.hx.sname = self.sname
        self.hx.state_dir = self.state_dir

    # -- state ---------------------------------------------------------------

    def _state_path(self):
        return os.path.join(self.state_dir, STATE_NAME)

    def load_state(self):
        try:
            with open(self._state_path(), encoding="utf-8") as fh:
                data = json.load(fh)
        except (OSError, ValueError, UnicodeDecodeError):
            return None
        return data if isinstance(data, dict) and data.get("pane") else None

    def _save_state(self, state):
        os.makedirs(self.state_dir, exist_ok=True)
        tmp = self._state_path() + ".tmp"
        with open(tmp, "w", encoding="utf-8") as fh:
            json.dump(state, fh, indent=1, sort_keys=True)
            fh.write("\n")
        os.replace(tmp, self._state_path())

    def _clear_state(self):
        try:
            os.remove(self._state_path())
        except OSError:
            pass

    # -- pi ------------------------------------------------------------------

    def _pi(self):
        if self.pi_path:
            return self.pi_path
        path = _tools().resolve("pi", self.cfg, default_bin="pi").get("path")
        if not path:
            raise LoginError("pi-not-found", reason="pi is not installed: run "
                             "`agent-exec tools install pi`")
        return path

    def _pi_env(self):
        env = dict(self.env)
        env.update(_agent_exec().PROFILES["pi"]["env"])
        overrides = _tools().pi_env_overrides(self.cfg, env=self.env)
        env.update(overrides)
        return env, overrides

    def auth_ready(self, provider):
        """True when `pi auth check` says the provider has credentials."""
        env, _ = self._pi_env()
        try:
            proc = self.pi_runner(
                [self._pi(), "auth", "check", "--provider", provider, "--json",
                 "--no-refresh"],
                capture_output=True, text=True, stdin=subprocess.DEVNULL,
                timeout=_AUTH_CHECK_TIMEOUT, env=env, cwd=self.cwd)
        except (OSError, ValueError, subprocess.TimeoutExpired):
            return False
        status = None
        for line in reversed((proc.stdout or "").strip().splitlines()):
            try:
                payload = json.loads(line)
            except ValueError:
                continue
            if isinstance(payload, dict):
                status = payload.get("status")
                break
        return proc.returncode == 0 and status != "not_ready"

    # -- herdr ---------------------------------------------------------------

    def _call(self, args, timeout=60):
        return self.hx._call(args, timeout)

    def _server_up(self):
        return self.hx._status_running()

    def _start_server(self):
        if self._server_up():
            return
        hx = self.hx
        home = hx._home()
        os.makedirs(os.path.join(home, "herdr"), mode=0o700, exist_ok=True)
        with open(os.path.join(home, "herdr", "config.toml"), "w", encoding="utf-8") as fh:
            fh.write(_herdr()._CONFIG_TOML)
        log_dir = os.path.join(self.state_dir, "herdr")
        os.makedirs(log_dir, exist_ok=True)
        log = open(os.path.join(log_dir, self.sname + ".server.log"), "ab")
        try:
            self.popen(hx._h() + ["server"], stdin=subprocess.DEVNULL, stdout=log,
                       stderr=log, start_new_session=True, env=hx._env())
        except OSError as exc:
            raise LoginError("server", reason="could not start herdr: %s" % exc)
        finally:
            log.close()
        deadline = self.clock() + 30
        while not self._server_up():
            if self.clock() >= deadline:
                raise LoginError("server", reason="herdr server did not start")
            self.sleep(1)

    def _stop_server(self):
        try:
            if self._server_up():
                self._call(["server", "stop"])
        except _herdr()._HerdrError:
            pass

    def read(self, pane):
        payload, out = self._call(["pane", "read", pane, "--source", "visible"])
        return screen_text(out, payload)

    def _keys(self, pane, *keys):
        self._call(["pane", "send-keys", pane] + list(keys))

    def _text(self, pane, text):
        self._call(["pane", "send-text", pane, text])

    def _wait_screen(self, pane, step, predicate, timeout=_STEP_TIMEOUT):
        """Poll the pane until predicate(screen) is truthy; else LoginError."""
        deadline = self.clock() + timeout
        screen = ""
        while True:
            screen = self.read(pane)
            if predicate(screen):
                return screen
            if self.clock() >= deadline:
                raise LoginError(step, screen)
            self.sleep(1)

    # -- cleanup ---------------------------------------------------------------

    def cleanup(self, state=None, escape=False):
        """Close the login pane and stop the login server. Best effort; only
        ever touches this module's own session."""
        state = state or self.load_state()
        Err = _herdr()._HerdrError
        if state:
            if escape:
                try:
                    self._keys(state["pane"], "esc")
                except Err:
                    pass
            close = (["workspace", "close", state["workspace"]] if state.get("workspace")
                     else ["pane", "close", state["pane"]])
            try:
                self._call(close)
            except Err:
                pass
        self._stop_server()
        self._clear_state()

    # -- the flow --------------------------------------------------------------

    def start(self, provider=None):
        """Drive /login up to the URL screen. Returns (exit code, result)."""
        providers = [provider] if provider else configured_providers(self.cfg)
        pending = None
        for candidate in providers:
            if not self.auth_ready(candidate):
                pending = candidate
                break
        if pending is None:
            return 0, {"status": "ready", "providers": providers}
        label = PROVIDER_LABELS.get(pending)
        if label is None:
            return 1, {"status": "manual", "step": "provider",
                       "reason": "no known /login label for provider %s" % pending,
                       "screen_tail": ""}
        previous = self.load_state()
        if previous:
            self.cleanup(previous, escape=True)
        state = None
        herdr = _herdr()
        try:
            self._start_server()
            payload, _ = self._call(["workspace", "create", "--cwd", self.cwd,
                                     "--label", "pi login", "--no-focus"])
            workspace = herdr._find(payload, "workspace_id")
            pane = herdr._find(payload, "pane_id")
            if not workspace or not pane:
                raise LoginError("workspace", reason="herdr returned no pane")
            state = {"pane": pane, "workspace": workspace, "provider": pending,
                     "session": self.sname}
            self._save_state(state)
            result = self._drive(state, label)
        except LoginError as exc:
            self.cleanup(state)
            out = {"status": "manual", "step": exc.step, "screen_tail": tail(exc.screen)}
            if exc.reason:
                out["reason"] = exc.reason
            return 1, out
        except herdr._HerdrError as exc:
            self.cleanup(state)
            return 1, {"status": "manual", "step": "herdr", "reason": str(exc),
                       "screen_tail": ""}
        self._save_state(dict(state, method=result["method"]))
        return 0, result

    def _drive(self, state, label):
        pane, provider = state["pane"], state["provider"]
        env, overrides = self._pi_env()
        assignments = ["%s=%s" % (k, shlex.quote(v)) for k, v in sorted(overrides.items())]
        assignments.append("PI_SKIP_VERSION_CHECK=1")
        self._call(["pane", "run", pane, " ".join(assignments + [shlex.quote(self._pi())])])
        self._wait_screen(pane, "start-pi", lambda s: _PI_READY_MARKER in s, _START_TIMEOUT)

        self._text(pane, "/login")
        self.sleep(1)
        self._keys(pane, "enter")
        self._wait_screen(pane, "auth-method",
                          lambda s: "Select authentication method" in s)
        screen = self.read(pane)
        if highlighted(screen) != _AUTH_METHOD_LABEL:
            raise LoginError("auth-method", screen)
        self._keys(pane, "enter")

        self._wait_screen(pane, "provider-list",
                          lambda s: "Select provider to configure" in s)
        self._text(pane, label)
        deadline = self.clock() + _STEP_TIMEOUT
        while True:
            screen = self.read(pane)
            if highlighted(screen) == label:
                break
            if self.clock() >= deadline:
                raise LoginError("provider-select", screen)
            self.sleep(1)
        self._keys(pane, "enter")

        def url_screen(s):
            return extract_url(s) is not None and bool(
                extract_code(s) or wants_paste(s) or "waiting for authentication" in s.lower())

        screen = self._wait_screen(
            pane, "login-method", lambda s: url_screen(s) or "Device code" in s)
        if not url_screen(screen):
            deadline = self.clock() + _STEP_TIMEOUT
            while not (highlighted(screen) or "").startswith(_DEVICE_LABEL):
                if self.clock() >= deadline:
                    raise LoginError("device-select", screen)
                self._keys(pane, "down")
                self.sleep(1)
                screen = self.read(pane)
            self._keys(pane, "enter")
            screen = self._wait_screen(pane, "login-url", url_screen)
        url = extract_url(screen)
        if not url:
            raise LoginError("login-url", screen)
        code = extract_code(screen)
        result = {"status": "waiting", "provider": provider,
                  "method": "device" if code else "browser", "url": url,
                  "paste_prompt": wants_paste(screen), "session": self.sname}
        if code:
            result["code"] = code
        return result

    def wait(self, timeout=DEFAULT_TIMEOUT):
        state = self.load_state()
        if not state:
            return 1, {"status": "failed", "reason": "no login in progress "
                       "(run `agent-exec pi login` first)", "screen_tail": ""}
        provider, pane = state.get("provider"), state["pane"]
        deadline = self.clock() + float(timeout)
        screen = ""
        Err = _herdr()._HerdrError
        while True:
            if self.auth_ready(provider):
                self.cleanup(state)
                return 0, {"status": "ready", "provider": provider}
            try:
                screen = self.read(pane)
            except Err:
                if self.auth_ready(provider):
                    self.cleanup(state)
                    return 0, {"status": "ready", "provider": provider}
                self.cleanup(state)
                return 1, {"status": "failed", "reason": "the login pane is gone",
                           "screen_tail": tail(screen)}
            if not self._alive(screen):
                if self.auth_ready(provider):
                    self.cleanup(state)
                    return 0, {"status": "ready", "provider": provider}
                self.cleanup(state)
                return 1, {"status": "failed", "reason": "pi exited",
                           "screen_tail": tail(screen)}
            if self.clock() >= deadline:
                return 1, {"status": "timeout", "provider": provider}
            self.sleep(_AUTH_POLL)

    @staticmethod
    def _alive(screen):
        """pi is still on its login screen (waiting, or asking for a paste)."""
        lower = (screen or "").lower()
        return any(marker in lower for marker in (
            "waiting for authentication", "paste", "enter code", "https://",
            "select ", "device code"))

    def paste(self, text, timeout=DEFAULT_TIMEOUT):
        state = self.load_state()
        if not state:
            return 1, {"status": "failed", "reason": "no login in progress "
                       "(run `agent-exec pi login` first)", "screen_tail": ""}
        try:
            self._text(state["pane"], text)
            self._keys(state["pane"], "enter")
        except _herdr()._HerdrError as exc:
            self.cleanup(state)
            return 1, {"status": "failed", "reason": str(exc), "screen_tail": ""}
        return self.wait(timeout)

    def cancel(self):
        self.cleanup(escape=True)
        return 0, {"status": "cancelled"}


# -- CLI ---------------------------------------------------------------------


def parse_args(args):
    """(options, error)."""
    opts = {"provider": None, "mode": "start", "paste": None, "timeout": DEFAULT_TIMEOUT}
    i = 0
    while i < len(args):
        arg = args[i]
        if arg in ("--provider", "--paste", "--timeout"):
            if i + 1 >= len(args):
                return None, "%s needs a value" % arg
            value = args[i + 1]
            i += 2
            if arg == "--provider":
                opts["provider"] = value
            elif arg == "--paste":
                opts["mode"], opts["paste"] = "paste", value
            else:
                try:
                    opts["timeout"] = float(value)
                except ValueError:
                    return None, "--timeout needs a number"
            continue
        if arg == "--wait":
            opts["mode"] = "wait"
        elif arg == "--cancel":
            opts["mode"] = "cancel"
        elif arg == "--json":
            pass
        else:
            return None, "unknown argument: %s" % arg
        i += 1
    return opts, None


def cmd_pi_login(args, login=None):
    opts, err = parse_args(args)
    if err is not None:
        sys.stderr.write("agent-exec: pi login: %s\n%s" % (err, USAGE))
        return 2
    login = login or PiLogin()
    try:
        if opts["mode"] == "wait":
            code, result = login.wait(opts["timeout"])
        elif opts["mode"] == "paste":
            code, result = login.paste(opts["paste"], opts["timeout"])
        elif opts["mode"] == "cancel":
            code, result = login.cancel()
        else:
            code, result = login.start(opts["provider"])
    except LoginError as exc:
        code, result = 1, {"status": "manual", "step": exc.step,
                           "reason": exc.reason or "", "screen_tail": tail(exc.screen)}
    print(json.dumps(result, ensure_ascii=False))
    return code
