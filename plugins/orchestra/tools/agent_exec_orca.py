# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
"""Orca executor for `agent-exec wave run`: Claude sessions that stay alive.

A package whose route resolves to a Claude tier normally comes back from
`wave run` as a `delegate` need. When Orca (a desktop app with an `orca`
CLI) is installed and running, the package runs instead in an interactive
Claude Code session Orca hosts -- within the user's subscription, never
`claude -p` -- and that session stays open so self-verify corrections and
post-integration fixes go back to the SAME session, which already holds
the code it wrote.

Each package gets an Orca-created worktree (`orca worktree create`; Orca
cannot open a terminal in a tree it did not create), adopted as task
`pkg-<id>` via `isolate_adopt` so diff/check/refresh/integrate reach it by
task id. Completion is signalled only by the result file the prompt asks
Claude to write: `terminal wait --for tui-idle` can report idle before the
TUI started or while a dialog is up, so idle alone never means "done".

Sessions are persisted in `<state dir>/orca-sessions.json` (pkg -> session)
so a resumed `wave run` reuses a live terminal. Stdlib only; agent_exec is
imported lazily.
"""

import hashlib
import json
import os
import re
import shlex
import shutil
import subprocess
import sys
import threading
import time

_HERE = os.path.dirname(os.path.abspath(__file__))


REGISTRY_NAME = "orca-sessions.json"

_TRUST_MARKER = "Is this a project you created or one you trust"
_DOWN = "\x1b[B"
_TRUST_YES_RE = re.compile(r"^\s*❯\s*(?:\d+\.\s*)?Yes, I trust")
_TRUST_KEY_LIMIT = 6
# One `terminal wait` call; the loops keep calling it.
_WAIT_CHUNK_SECONDS = 60
_SEND_WAIT_SUBMIT = 30
_STATUS_TIMEOUT = 15
_CALL_TIMEOUT = 60
# A selected option of a numbered choice dialog ("❯ 1. Yes") or Claude's
# permission question; the bare input prompt line ("❯ ") matches neither.
_CHOICE_RE = re.compile(r"^\s*❯\s*\d+\.")
_PERMISSION_RE = re.compile(r"Do you want to ")


def _agent_exec():
    if _HERE not in sys.path:
        sys.path.insert(0, _HERE)
    import agent_exec  # noqa: E402
    return agent_exec


def _excerpt(text):
    if _HERE not in sys.path:
        sys.path.insert(0, _HERE)
    import agent_exec_checks  # noqa: E402
    return agent_exec_checks.failure_excerpt(text)


class OrcaNeed(Exception):
    """The package cannot continue in Orca; the instructor has to step in.

    `kind` is one of trust|stalled|timeout|error; `detail` is a short
    human-readable reason (often the screen tail).
    """

    def __init__(self, kind, detail):
        Exception.__init__(self, "%s: %s" % (kind, detail))
        self.kind = kind
        self.detail = detail


class _OrcaError(Exception):
    """One `orca` call failed. `payload` is its parsed JSON, if any."""

    def __init__(self, message, payload=None):
        Exception.__init__(self, message)
        self.payload = payload if isinstance(payload, dict) else {}


def normalize_enabled(value):
    """`enabled` as True, False or "auto". YAML 1.1 hands booleans for
    true/false/yes/no; anything unrecognized means "auto"."""
    if value is True or value is False:
        return value
    if isinstance(value, str):
        word = value.strip().lower()
        if word in ("true", "yes", "on", "required"):
            return True
        if word in ("false", "no", "off", "never"):
            return False
    return "auto"


def merged_config(cfg):
    """`cfg` over agent_exec.DEFAULTS["orca"] (the single source of the
    defaults), with `enabled` normalized."""
    defaults = _agent_exec().DEFAULTS["orca"]
    out = dict(defaults)
    out["models"] = dict(defaults["models"])
    for key, value in (cfg or {}).items():
        if key == "models" and isinstance(value, dict):
            out["models"].update(value)
        elif value is not None:
            out[key] = value
    out["enabled"] = normalize_enabled(out.get("enabled"))
    return out


def _run(binary, args, timeout):
    """Run one `orca` call with --json. Returns the `result` object or raises
    _OrcaError."""
    try:
        proc = subprocess.run(
            [binary] + list(args) + ["--json"], capture_output=True, text=True,
            stdin=subprocess.DEVNULL, timeout=timeout,
        )
    except subprocess.TimeoutExpired:
        raise _OrcaError("orca %s timed out after %ss" % (" ".join(args[:2]), timeout))
    except (OSError, ValueError) as exc:
        raise _OrcaError("could not run orca: %s" % exc)
    try:
        payload = json.loads(proc.stdout or "")
    except ValueError:
        payload = None
    if not isinstance(payload, dict) or payload.get("ok") is not True or proc.returncode != 0:
        message = ""
        if isinstance(payload, dict):
            error = payload.get("error")
            if isinstance(error, dict):
                message = error.get("message") or json.dumps(error, ensure_ascii=False)
            elif error:
                message = str(error)
        message = message or (proc.stderr or proc.stdout or "").strip() or "exit %s" % proc.returncode
        raise _OrcaError("orca %s failed: %s" % (" ".join(args[:2]), _excerpt(message)), payload)
    result = payload.get("result")
    return result if isinstance(result, dict) else {}


def available(cfg, binary="orca"):
    """(bool, reason): Orca is enabled, on PATH, and its runtime is up."""
    cfg = merged_config(cfg)
    if cfg["enabled"] is False:
        return False, "disabled by config (orca.enabled: false)"
    if shutil.which(binary) is None:
        return False, "%s is not on PATH" % binary
    try:
        _run(binary, ["status"], _STATUS_TIMEOUT)
    except _OrcaError as exc:
        return False, "orca status not ok: %s" % exc
    return True, ""


def _request_id(result):
    send = result.get("send") if isinstance(result, dict) else None
    prompt = send.get("prompt") if isinstance(send, dict) else None
    value = prompt.get("requestId") if isinstance(prompt, dict) else None
    return value if isinstance(value, str) and value else None


def _sanitize_name(text):
    return re.sub(r"[^a-z0-9._-]+", "-", text.lower()).strip("-.") or "wave"


def _is_trust(screen):
    return any(_TRUST_MARKER in line for line in screen)


def _trust_yes_selected(screen):
    return any(_TRUST_YES_RE.match(line) for line in screen)


def _has_prompt_box(screen):
    return any(line.lstrip().startswith("❯") for line in screen)


def _is_dialog(screen):
    return any(_CHOICE_RE.match(line) or _PERMISSION_RE.search(line) for line in screen)


def _tail(screen):
    return _excerpt("\n".join(screen[-40:]))


def _read_result(path):
    try:
        with open(path, encoding="utf-8") as fh:
            data = json.load(fh)
    except (OSError, ValueError, UnicodeDecodeError):
        return None
    if not isinstance(data, dict) or data.get("status") not in ("ok", "escalate"):
        return None
    return data


class OrcaExecutor(object):
    """Runs packages in Orca-hosted Claude Code sessions.

    `clock`/`sleep`/`poll` are injectable so tests drive the wait loops
    against a fake `orca` without real delays. `on_event(pkg, detail)` is the
    runner's hook for `orca-session` events.
    """

    def __init__(self, cfg=None, binary="orca", clock=time.time, sleep=time.sleep,
                 poll=1.0, on_event=None):
        self.cfg = merged_config(cfg)
        self.binary = binary
        self.clock = clock
        self.sleep = sleep
        self.poll = poll
        self.on_event = on_event
        self.state_dir = None
        self.lock = threading.Lock()

    @classmethod
    def from_config(cls, **kwargs):
        """Build from the resolved 4-layer config's `orca` key."""
        resolved, _ = _agent_exec().resolve_config()
        return cls(cfg=(resolved or {}).get("orca") or {}, **kwargs)

    @property
    def enabled(self):
        return self.cfg["enabled"]

    def available(self, cfg=None):
        return available(self.cfg if cfg is None else cfg, binary=self.binary)

    def model_for(self, cls, route_model=None):
        return route_model or self.cfg["models"].get(cls) or "opus"

    def _orca(self, args, timeout=_CALL_TIMEOUT):
        return _run(self.binary, args, timeout)

    def _event(self, pkg, action, terminal):
        if self.on_event is None:
            return
        try:
            self.on_event(pkg, {"action": action, "terminal": terminal})
        except Exception:
            pass  # observability must never take a package down

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

    def _save(self, session):
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
        """The registered session of `pkg`, or None.

        A terminal that no longer answers `terminal read` is dropped from the
        session (the next prompt opens a new one on the same worktree); a
        worktree that is gone drops the whole entry.
        """
        state_dir = state_dir or self.state_dir
        entry = self._load_registry(state_dir).get(pkg)
        if not isinstance(entry, dict) or not entry.get("worktree"):
            return None
        if not os.path.isdir(entry["worktree"]):
            self._forget(dict(entry, pkg=pkg, state_dir=state_dir))
            return None
        if entry.get("terminal"):
            try:
                self._screen(entry["terminal"])
            except _OrcaError:
                entry["terminal"] = None
                self._save(entry)
        return entry

    # -- terminal ----------------------------------------------------------------

    def _screen(self, handle):
        result = self._orca(["terminal", "read", "--terminal", handle, "--screen"])
        terminal = result.get("terminal") or {}
        tail = terminal.get("tail") or []
        return [str(line) for line in tail] if isinstance(tail, list) else str(tail).splitlines()

    def _wait_idle(self, handle, seconds):
        ms = int(max(1, min(seconds, _WAIT_CHUNK_SECONDS)) * 1000)
        try:
            self._orca(["terminal", "wait", "--terminal", handle, "--for", "tui-idle",
                        "--timeout-ms", str(ms)], timeout=ms / 1000.0 + 30)
        except _OrcaError:
            pass  # a wait that did not settle is just another poll
        if self.poll:
            self.sleep(self.poll)

    def _open_terminal(self, session):
        """`terminal create` on the session's worktree, then wait for the
        prompt box. Raises OrcaNeed on the trust dialog (without auto_trust)
        or when startup_timeout passes."""
        command = self.cfg["command"].replace("{model}", session.get("model") or "opus")
        # The prompt files and the result file live outside the worktree;
        # without --add-dir the session stops on a read/write permission
        # dialog that nobody is there to answer.
        for directory in session.get("add_dirs") or ():
            command += " --add-dir " + shlex.quote(directory)
        try:
            result = self._orca(["terminal", "create", "--worktree", "path:" + session["worktree"],
                                 "--title", "wave " + session["pkg"], "--command", command])
        except _OrcaError as exc:
            raise OrcaNeed("error", str(exc))
        handle = (result.get("terminal") or {}).get("handle")
        if not handle:
            raise OrcaNeed("error", "orca terminal create returned no handle")
        session["terminal"] = handle
        self._save(session)

        deadline = self.clock() + float(self.cfg["startup_timeout"])
        trust_keys = 0
        screen = []
        while True:
            try:
                screen = self._screen(handle)
            except _OrcaError as exc:
                raise OrcaNeed("error", str(exc))
            if _is_trust(screen):
                if not self.cfg["auto_trust"]:
                    raise OrcaNeed("trust", "%s needs the folder-trust dialog accepted once; "
                                            "open it in Orca or trust ~/orca/workspaces"
                                   % session["worktree"])
                # Arrow and Enter go in separate sends, Enter only once the
                # screen shows "Yes" selected: sent together, Enter lands
                # before the cursor moves and picks the preselected "No, exit".
                if trust_keys >= _TRUST_KEY_LIMIT:
                    raise OrcaNeed("stalled", "folder-trust dialog did not accept:\n%s"
                                   % _tail(screen))
                keys = ["--enter"] if _trust_yes_selected(screen) else ["--text", _DOWN]
                try:
                    self._orca(["terminal", "send", "--terminal", handle] + keys)
                except _OrcaError as exc:
                    raise OrcaNeed("error", str(exc))
                if trust_keys == 0:
                    self._event(session["pkg"], "trust", handle)
                trust_keys += 1
            elif _has_prompt_box(screen):
                return handle
            remaining = deadline - self.clock()
            if remaining <= 0:
                raise OrcaNeed("stalled", "no Claude prompt after %ss:\n%s"
                               % (self.cfg["startup_timeout"], _tail(screen)))
            self._wait_idle(handle, remaining)

    # -- the contract ------------------------------------------------------------

    def start(self, pkg_id, repo, base_ref, cls, model, run_id, state_dir, add_dirs=()):
        """Create an Orca worktree on `base_ref`, adopt it as `pkg-<id>`,
        open a Claude terminal there and wait for its prompt box."""
        agent_exec = _agent_exec()
        self.state_dir = self.state_dir or state_dir
        rc, out = agent_exec._git(repo, "rev-parse", "--verify", "%s^{commit}" % base_ref)
        base_sha = out.strip() if rc == 0 else ""
        if not base_sha:
            raise OrcaNeed("error", "could not resolve %s to a commit" % base_ref)
        run8 = re.sub(r"[^a-z0-9]", "", (run_id or "").lower())[:8]
        if not run8:
            run8 = hashlib.sha1(os.path.abspath(state_dir).encode("utf-8")).hexdigest()[:8]
        name = _sanitize_name("wave-%s-%s" % (run8, pkg_id))
        try:
            result = self._orca(["worktree", "create", "--repo", "path:" + repo, "--name", name,
                                 "--base-branch", base_ref, "--setup", "skip", "--no-parent"],
                                timeout=300)
        except _OrcaError as exc:
            raise OrcaNeed("error", str(exc))
        worktree = result.get("worktree") or {}
        path = worktree.get("path")
        if not path or not os.path.isdir(path):
            raise OrcaNeed("error", "orca worktree create returned no usable path")
        session = {
            "pkg": pkg_id, "worktree": path, "branch": worktree.get("branch"),
            "terminal": None, "request_ids": [], "repo": repo, "model": model,
            "cls": cls, "state_dir": state_dir,
            "add_dirs": sorted(set(os.path.abspath(d) for d in add_dirs)),
        }
        self._save(session)
        adopted = agent_exec.isolate_adopt(repo, "pkg-" + pkg_id, path, baseline=base_sha,
                                           session_id=None)
        if adopted.get("status") not in ("adopted", "exists"):
            self.close(session, remove_worktree=True)
            raise OrcaNeed("error", "could not adopt %s: %s" % (path, adopted.get("note")))
        try:
            handle = self._open_terminal(session)
        except OrcaNeed:
            self.close(session, remove_worktree=True)
            raise
        self._event(pkg_id, "start", handle)
        session["fresh"] = True  # the first prompt is not a resume
        return session

    def prompt(self, session, prompt_files, result_path, timeout):
        """Send ONE prompt line and wait for its result file.

        Returns a dispatch-shaped result dict; raises OrcaNeed on timeout or
        on a permission/choice dialog nobody will answer.
        """
        fresh = session.pop("fresh", False)
        resumed = bool(session.get("terminal")) and not fresh
        if resumed:
            # A live terminal is not a live Claude: after an exit (e.g. the
            # trust dialog answered "No") it is a bare shell, and the prompt
            # line would be typed into it. Reuse only a visible prompt box.
            try:
                screen = self._screen(session["terminal"])
            except _OrcaError:
                screen = []
            if _is_trust(screen) or not _has_prompt_box(screen):
                self._close_terminal(session)
                resumed = False
        if not session.get("terminal"):
            # Closed after the last prompt (keep_sessions: false) or gone:
            # a new terminal on the same Orca worktree.
            self._event(session["pkg"], "start", self._open_terminal(session))
        handle = session["terminal"]
        if resumed:
            self._event(session["pkg"], "reuse", handle)

        try:
            os.remove(result_path)
        except OSError:
            pass
        os.makedirs(os.path.dirname(os.path.abspath(result_path)), exist_ok=True)
        line = (
            "Read the worker prompt from %s and carry it out. When you are completely done, "
            "write %s containing JSON {\"status\": \"ok\"|\"escalate\", \"summary\": "
            "\"<=15 lines\"}. If you must ESCALATE, put the reason in summary."
            % (" and ".join(prompt_files), result_path)
        )
        send = ["terminal", "send", "--terminal", handle, "--text", line, "--enter",
                "--wait-submit", str(_SEND_WAIT_SUBMIT)]
        try:
            request_id = _request_id(self._orca(send, timeout=_SEND_WAIT_SUBMIT + 30))
        except _OrcaError as exc:
            request_id = _request_id(exc.payload.get("result"))
            if not request_id:
                raise OrcaNeed("error", str(exc))
            # Orca de-duplicates by request id: the prompt runs once.
            try:
                self._orca(send + ["--retry-request", request_id], timeout=_SEND_WAIT_SUBMIT + 30)
            except _OrcaError as retry_exc:
                raise OrcaNeed("error", str(retry_exc))
        if request_id:
            session.setdefault("request_ids", []).append(request_id)
            self._save(session)

        deadline = self.clock() + float(timeout)
        try:
            while True:
                data = _read_result(result_path)
                if data is not None:
                    break
                try:
                    screen = self._screen(handle)
                except _OrcaError as exc:
                    raise OrcaNeed("stalled", "terminal %s went away: %s" % (handle, exc))
                if _is_dialog(screen) and not _is_trust(screen):
                    raise OrcaNeed("stalled", "waiting on a dialog:\n%s" % _tail(screen))
                remaining = deadline - self.clock()
                if remaining <= 0:
                    raise OrcaNeed("timeout", "no result after %ss:\n%s" % (timeout, _tail(screen)))
                self._wait_idle(handle, remaining)
        finally:
            if not self.cfg["keep_sessions"]:
                self._close_terminal(session)

        summary = str(data.get("summary") or "")
        answer = ("ESCALATE: " + summary) if data["status"] == "escalate" else summary
        return {
            "status": "ok", "answer": answer, "session_id": handle, "resumed": resumed,
            "executor": "orca",
            "isolation": {"isolate": True, "path": session["worktree"],
                          "workdir": session["worktree"]},
        }

    def _close_terminal(self, session):
        handle = session.get("terminal")
        if not handle:
            return
        try:
            self._orca(["terminal", "close", "--terminal", handle, "--tab"])
        except _OrcaError as exc:
            sys.stderr.write("agent-exec: orca: %s\n" % exc)
        session["terminal"] = None
        self._save(session)
        self._event(session["pkg"], "close", handle)

    def close(self, session, remove_worktree):
        """Close the terminal; with `remove_worktree`, unadopt and remove the
        Orca worktree too. Errors are logged, never raised."""
        try:
            self._close_terminal(session)
            if not remove_worktree:
                return
            repo = session.get("repo")
            if repo:
                try:
                    _agent_exec().isolate_unadopt(repo, "pkg-" + session["pkg"], session_id=None)
                except Exception as exc:
                    sys.stderr.write("agent-exec: orca: unadopt failed: %s\n" % exc)
            try:
                self._orca(["worktree", "rm", "--worktree", "path:" + session["worktree"],
                            "--force"], timeout=300)
            except _OrcaError as exc:
                sys.stderr.write("agent-exec: orca: %s\n" % exc)
            self._forget(session)
        except Exception as exc:
            sys.stderr.write("agent-exec: orca: close failed: %s\n" % exc)
