# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
"""`agent-exec wave run` / `wave mark`: the deterministic outer loop.

Drives a plan (agent_exec_wave_plan) through dispatch -> self-verify ->
integrate, one wave at a time, with every state change going through
agent_exec_wave.StateStore. One wave is one batch: the packages `select`
picks run in a thread pool, then a barrier, then every package that reached
`ready` is integrated in ONE `isolate_integrate` call (plan order, skip on
conflict, gate + bisect). Anything only the instructor can decide lands on
the state's `needs` list instead of stopping the loop.

Imported lazily by agent_exec_wave's sub-verb table, so `wave status` never
pays for (or depends on) agent_exec. This module imports agent_exec itself
and calls isolate_create/isolate_refresh/isolate_integrate/cmd_check
in-process; dispatch goes through an injectable executor (CliExecutor in
production, a fake in tests) because a real dispatch is a separate process
anyway. A package the CLI dispatch hands back as `delegate` (a Claude tier)
runs in an Orca-hosted Claude session instead when Orca is available
(agent_exec_orca); corrections then go to that same live session.
"""

import concurrent.futures
import json
import os
import re
import shlex
import subprocess
import sys
import threading
import time
import yaml

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import agent_exec  # noqa: E402
import agent_exec_checks  # noqa: E402
import agent_exec_orca  # noqa: E402
import agent_exec_ui  # noqa: E402
import agent_exec_wave  # noqa: E402
import agent_exec_wave_plan  # noqa: E402

_HERE = os.path.dirname(os.path.abspath(__file__))

_NOTIFY_TIMEOUT = 30
# One `dispatch wait` poll. Short enough that no single call nears a 600 s
# foreground ceiling; the loop in CliExecutor.dispatch keeps polling.
_DISPATCH_POLL_SECONDS = 300

_PASSING_CHECK = frozenset(("pass", "no-checks", "preexisting"))

# Rounds of bisect-and-revert one red `--full` may spend, like integrate's
# `bisect_max`.
_FULL_BISECT_MAX = 3
# First line of a correction written after a red `--full` reverted the
# package: `_full_files` adds such a file to the next implement dispatch.
_POST_FULL_HEADER = "# Post-integration correction for %s"
_POST_FULL_DETAIL = "post-full revert"


def _agent_exec_argv():
    """How to invoke agent-exec: this interpreter + the sibling agent_exec.py.

    Resolved from this file, never from PATH, so the loop always drives the
    same agent-exec it was started from (and inherits this interpreter's
    pyyaml).
    """
    return [sys.executable, os.path.join(_HERE, "agent_exec.py")]


# --- executor interface ------------------------------------------------------


# A repo-relative path: contains a "/", or is name.ext with a letters/digits
# extension where name or ext has 2+ chars (so "naming." and "e.g." are not paths).
_PATH_LIKE = re.compile(
    r"^(?=.*(?:/|[\w-]{2,}\.[A-Za-z0-9]+$|[\w-]+\.[A-Za-z0-9]{2,}$))[\w@.+*?\[\]/-]+$")


class CliExecutor(object):
    """Dispatch through `agent-exec dispatch prepare` / `dispatch --token`.

    `dispatch` detaches and then polls `dispatch wait`, so a long worker run
    is never bounded by one call's timeout.
    """

    def __init__(self, argv=None, cwd=None, poll_seconds=_DISPATCH_POLL_SECONDS):
        self.argv = list(argv) if argv else _agent_exec_argv()
        self.cwd = cwd
        self.poll_seconds = poll_seconds

    def _call(self, args):
        try:
            proc = subprocess.run(
                self.argv + list(args), cwd=self.cwd, capture_output=True, text=True,
                stdin=subprocess.DEVNULL,
            )
        except (OSError, ValueError) as exc:
            return None, str(exc)
        parsed = agent_exec._extract_json_object(proc.stdout or "")
        if proc.returncode != 0 or not isinstance(parsed, dict):
            return None, agent_exec_checks.failure_excerpt((proc.stderr or "") + (proc.stdout or ""))
        return parsed, ""

    def prepare(self, prompt_files, cls, workdir, task, run_id):
        args = ["dispatch", "prepare", "--class", cls, "--workdir", workdir,
                "--isolate", "always", "--task", task, "--json"]
        for path in prompt_files:
            args.extend(["--prompt-file", path])
        if run_id:
            args.extend(["--run-id", run_id])
        parsed, error = self._call(args)
        if parsed is None or not parsed.get("token"):
            raise RuntimeError("dispatch prepare failed: %s" % error)
        return parsed["token"]

    def dispatch(self, token, cls=None, exhausted=(), no_resume=False):
        args = ["dispatch", "--token", token, "--capture", "--detach"]
        if cls:
            args.extend(["--class", cls])
        if exhausted:
            args.extend(["--exhausted", ",".join(exhausted)])
        if no_resume:
            args.append("--no-resume")
        parsed, error = self._call(args)
        if parsed is None:
            return {"status": "error", "reason": "dispatch --detach failed: %s" % error}
        if parsed.get("status") not in ("detached", "running", "done"):
            return parsed
        while True:
            parsed, error = self._call(
                ["dispatch", "wait", "--token", token, "--max-wait", str(self.poll_seconds)])
            if parsed is None:
                return {"status": "error", "reason": "dispatch wait failed: %s" % error}
            if parsed.get("status") != "running":
                return parsed


# --- per-thread stdout (cmd_check prints its result) ---------------------------


class _ThreadStdout(object):
    """sys.stdout stand-in that routes a thread's writes to its own buffer.

    `cmd_check` reports by printing JSON; running it in-process from several
    worker threads at once needs each thread's print to land in that thread's
    buffer, which `contextlib.redirect_stdout` (process-global) cannot do.
    """

    def __init__(self, fallback):
        self._fallback = fallback
        self._local = threading.local()

    def capture(self, buf):
        self._local.buf = buf

    def release(self):
        self._local.buf = None

    def write(self, text):
        buf = getattr(self._local, "buf", None)
        return (buf if buf is not None else self._fallback).write(text)

    def flush(self):
        buf = getattr(self._local, "buf", None)
        if buf is None:
            self._fallback.flush()

    def __getattr__(self, name):
        return getattr(self._fallback, name)


class _Buffer(object):
    def __init__(self):
        self.parts = []

    def write(self, text):
        self.parts.append(text)
        return len(text)

    def getvalue(self):
        return "".join(self.parts)


# --- the runner -----------------------------------------------------------------


def default_opts():
    return {
        "plan": None, "state": None, "into": None, "repo": os.getcwd(),
        "max_in_flight": 4, "gate": None, "full": None, "full_every": 1,
        "full_timeout": None, "full_retry": None, "on_green": None,
        "resume_on_reset": False,
        "no_ui": False, "notify_cmd": None,
        "stop_at": None, "max_waves": None, "max_packages": None,
        "run_id": None, "text": False,
    }


def _first_line(text):
    for line in (text or "").splitlines():
        if line.strip():
            return line.strip()
    return ""


class _Runner(object):
    def __init__(self, opts, executor, clock, orca=None, sleep=time.sleep):
        self.opts = opts
        self.executor = executor
        # OrcaExecutor, or None: Claude-tier packages stay `delegate` needs.
        self.orca = orca
        self.notes = {}
        self.clock = clock
        self.sleep = sleep
        self.state_path = os.path.abspath(opts["state"])
        self.state_dir = os.path.dirname(self.state_path)
        self.store = agent_exec_wave.StateStore(self.state_path, clock=clock)
        self.lock = threading.Lock()
        self.exhausted = set()
        self.started = 0
        self.waves = 0
        self.stop_reason = None
        self.notify_threads = []
        self.integrated_since_full = 0
        self.stdout = None
        self.refresh_info = {}
        self.answers = {}
        self.unavailable = False
        self.green_path = os.path.join(self.state_dir, "green-tree")

    # -- helpers ------------------------------------------------------------

    def _task(self, pid):
        return "pkg-" + pid

    def _int_head(self):
        rc, out = agent_exec._git(self.int_path, "rev-parse", "HEAD")
        return out.strip() if rc == 0 else None

    def _clean_integration(self, after):
        rc, out = agent_exec._git(self.int_path, "status", "--porcelain")
        files = len(out.splitlines()) if rc == 0 else 0
        if files:
            agent_exec._git(self.int_path, "reset", "-q", "--hard", "HEAD")
            agent_exec._git(self.int_path, "clean", "-fdq")
            self._emit("integration-dirty", None, {"files": files, "after": after})
        return files

    def _emit(self, name, pkg, obj):
        """One mechanical-stage event; detail is a JSON object string."""
        try:
            self.store.event(name, pkg=pkg, detail=json.dumps(
                obj, ensure_ascii=False, sort_keys=True))
        except Exception:
            pass  # observability must never take the loop down

    def _notify(self, kind, payload):
        cmd = self.opts.get("notify_cmd")
        if not cmd:
            return
        env = dict(os.environ, WAVE_EVENT=kind, WAVE_STATE=self.state_path)
        data = json.dumps(payload, ensure_ascii=False)

        def fire():
            try:
                subprocess.run(
                    ["/bin/sh", "-c", cmd], input=data, text=True, env=env,
                    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                    timeout=_NOTIFY_TIMEOUT,
                )
            except (OSError, ValueError, subprocess.TimeoutExpired):
                pass

        thread = threading.Thread(target=fire, daemon=True)
        thread.start()
        with self.lock:
            self.notify_threads.append(thread)

    def _need(self, pid, kind, detail):
        self.store.add_need(pid, kind, detail)
        self._notify("need", {"event": "need", "pkg": pid, "kind": kind,
                              "detail": detail, "at": self.clock()})

    def _stop(self, reason):
        with self.lock:
            if self.stop_reason is not None:
                return
            self.stop_reason = reason
        self.store.mark_stopped(reason)
        self._notify("stopped", {"event": "stopped", "reason": reason, "at": self.clock()})

    def _tree_files(self, pid):
        """Files the package's worktree changed, or None when it has no tree."""
        try:
            diff = agent_exec.isolate_diff(self.root, self._task(pid), with_patch=False)
        except ValueError:
            return None
        if diff.get("status") != "ok":
            return None
        return diff.get("files") or []

    # -- init ---------------------------------------------------------------

    def init(self):
        self.plan = agent_exec_wave_plan.load_plan(self.opts["plan"])
        overrides_path = os.path.join(self.state_dir, "plan-overrides.json")
        try:
            with open(overrides_path, encoding="utf-8") as fh:
                overrides = json.load(fh)
        except (OSError, ValueError):
            overrides = {}
        for pkg in self.plan["packages"]:
            extra = ((overrides.get(pkg["id"], {}) if isinstance(overrides, dict) else {})
                     .get("files_owned_add") or [])
            for glob in extra:
                if glob not in pkg["files_owned"]:
                    pkg["files_owned"].append(glob)
        plan_warnings = agent_exec_wave_plan.lint_plan(self.plan)
        self.pkgs = dict((p["id"], p) for p in self.plan["packages"])
        self.root = agent_exec.repo_root(self.opts["repo"])
        if self.root is None:
            raise RuntimeError("not a git repository: %s" % self.opts["repo"])
        self.into = agent_exec.sanitize_task_id(self.opts["into"])
        self.store.init(os.path.abspath(self.opts["plan"]),
                        [p["id"] for p in self.plan["packages"]], self.into)
        for warning in plan_warnings:
            self._emit("plan-warning", warning.get("pkg"), {"message": warning["message"]})
            sys.stderr.write("plan-warning: %s: %s\n"
                             % (warning.get("pkg"), warning["message"]))
        try:
            agent_exec_wave.register_wave(
                self.state_path, self.opts["plan"], self.root, self.into, clock=self.clock)
        except OSError:
            pass  # an unwritable registry only costs discoverability

        # Integration worktree: same create-then-tag path isolate_integrate
        # takes, so a later `isolate integrate --into` reuses it as-is.
        rc, out = agent_exec._git(self.root, "rev-parse", "HEAD")
        head = out.strip() if rc == 0 else ""
        if not head:
            raise RuntimeError("could not resolve HEAD in %s" % self.root)
        created = agent_exec.isolate_create(self.root, self.into, backend="git", onto=head)
        path = created.get("path")
        if created.get("status") == "error" or not path or not os.path.isdir(path):
            raise RuntimeError("could not create the integration worktree %s: %s"
                               % (self.into, created.get("reason")))
        agent_exec._write_role(path, "integration")
        self.int_path = path
        self.base = agent_exec._read_baseline(path) or head
        self.store.set_integration(task=self.into, path=path, base=self.base)
        if self.opts.get("full") and self._last_green() is None:
            self._record_green(self.base)

        os.makedirs(os.path.join(self.state_dir, "context"), exist_ok=True)
        os.makedirs(os.path.join(self.state_dir, "corrections"), exist_ok=True)
        os.makedirs(os.path.join(self.state_dir, "carry"), exist_ok=True)
        self._init_orca()

        # Packages a crashed run left mid-flight.
        state = self.store.load()
        for pid in self.pkgs:
            status = state["packages"].get(pid, {}).get("status")
            if status in ("implementing", "fixing"):
                files = self._tree_files(pid)
                if files:
                    self.store.set_status(pid, "verifying", detail="resumed with changes")
                else:
                    self.store.set_status(pid, "pending", detail="resumed without changes")

    def _init_orca(self):
        """Keep self.orca only when it is enabled and reachable; `enabled:
        true` with Orca unreachable stops the run before anything starts."""
        if self.orca is None:
            return
        enabled = self.orca.enabled
        if enabled is False:
            self.orca = None
            return
        ok, reason = self.orca.available()
        if not ok:
            self.orca = None
            if enabled is True:
                self._stop("orca unavailable: %s" % reason)
            return
        rc, out = agent_exec._git(self.int_path, "rev-parse", "--abbrev-ref", "HEAD")
        self.int_branch = out.strip() if rc == 0 and out.strip() != "HEAD" else self.base
        self.orca.state_dir = self.state_dir
        self.orca.on_event = lambda pkg, detail: self._emit("orca-session", pkg, detail)
        os.makedirs(os.path.join(self.state_dir, "orca"), exist_ok=True)

    # -- stop / select -------------------------------------------------------

    def _stop_check(self):
        if self.stop_reason is not None:
            return self.stop_reason
        if agent_exec_wave.stop_requested(self.state_path):
            return "stop requested"
        if self.opts.get("stop_at") is not None and self.clock() >= self.opts["stop_at"]:
            return "stop-at reached"
        if self.opts.get("max_waves") is not None and self.waves >= self.opts["max_waves"]:
            return "max waves reached"
        if (self.opts.get("max_packages") is not None
                and self.started >= self.opts["max_packages"]):
            return "max packages reached"
        return None

    # -- per package -----------------------------------------------------------

    def _refresh(self, pid, head):
        """Step 3a. Returns (ok, note)."""
        try:
            _, entry = agent_exec._resolve_worktree(
                self.root, self._task(pid), agent_exec._current_session())
        except ValueError as exc:
            self._need(pid, "dispatch-error", str(exc))
            return False, ""
        if not entry:
            self.refresh_info[pid] = {"fresh": True, "carried": False, "patch_file": None}
            return True, ""
        baseline = agent_exec._read_baseline(entry.get("path"))
        if not baseline or baseline == head:
            self.refresh_info[pid] = {"fresh": False, "carried": False, "patch_file": None}
            return True, ""
        rc, _ = agent_exec._git(self.int_path, "merge-base", "--is-ancestor", head, baseline)
        if rc == 0:
            return True, ""  # the tree already sits on top of the integration HEAD
        refreshed = agent_exec.isolate_refresh(self.root, self._task(pid), onto=head)
        status = refreshed.get("status")
        self._emit("refresh", pid, {"status": status,
                                    "files": len(refreshed.get("files") or [])})
        if status == "conflicted":
            files = [c.get("file") for c in refreshed.get("conflicts") or [] if c.get("file")]
            self._need(pid, "conflict", json.dumps(
                {"files": files, "stage": "refresh"}, ensure_ascii=False, sort_keys=True))
            return False, ""
        if status == "error":
            self._need(pid, "dispatch-error", refreshed.get("note") or "refresh failed")
            return False, ""
        if status not in ("ok", "conflicted-resolved"):
            return True, ""
        old = refreshed.get("old_baseline")
        rc, out = agent_exec._git(self.int_path, "diff", "--name-only", "%s..%s" % (old, head))
        changed = [f for f in out.splitlines() if f.strip()] if rc == 0 else []
        self.refresh_info[pid] = {
            "fresh": True,
            "carried": bool(refreshed.get("patch_file") and refreshed.get("files")),
            "patch_file": refreshed.get("patch_file"),
        }
        note = (
            "Your worktree was moved onto the current integration HEAD (%s); your "
            "earlier changes were re-applied on top. Files other packages changed "
            "since your previous base:\n%s\n" % (
                head[:12], "\n".join("- " + f for f in changed) or "- (none)")
        )
        return True, note

    def _context_path(self, pid):
        return os.path.join(self.state_dir, "context", pid + ".md")

    def _correction_path(self, pid):
        return os.path.join(self.state_dir, "corrections", pid + ".md")

    def _write_context(self, pid, refresh_note):
        pkg = self.pkgs[pid]
        tree = self.store.load()["packages"].get(pid, {}).get("tree")
        lines = [
            "# Wave package %s" % pid,
            "",
            "WORKING TREE: %s. Do every read, edit, test and command inside it; "
            "never touch files outside it." % (
                tree or "the isolated worktree you are started in (your cwd)"),
            "",
            "You own only these paths (files_owned); change nothing else:",
        ]
        lines.extend("- " + f for f in pkg["files_owned"] or ["(none listed)"])
        lines.extend([
            "",
            "Carry-over: anything you need changed outside files_owned goes, one "
            "self-contained instruction per line with file paths, into %s -- never "
            "into the code. Finish your own scope anyway." % os.path.join(
                self.state_dir, "carry", pid + ".carry.md"),
            "",
            "Read the current code before trusting the spec's paths: other packages "
            "have moved code since it was written.",
            "",
            "Do NOT commit, stash, reset or checkout. To set changes aside use "
            "`agent-exec shelf push` / `agent-exec shelf pop`, never `git stash`.",
            "",
            "If the task cannot be done as specified, reply with a first line "
            "`ESCALATE: <reason>` and change nothing further.",
            "",
            "Do not commit: leave every change uncommitted in the working tree.",
        ])
        if refresh_note:
            lines.extend(["", refresh_note])
        path = self._context_path(pid)
        with open(path, "w", encoding="utf-8") as fh:
            fh.write("\n".join(lines) + "\n")
        return path

    def _full_files(self, pid):
        files = (list(self.plan["preamble"]) + [self._context_path(pid)]
                 + [self.pkgs[pid]["spec"]])
        if self._post_full_correction(pid):
            files.append(self._correction_path(pid))
        return files

    def _post_full_correction(self, pid):
        """True when corrections/<id>.md is a not-yet-sent post-full one."""
        try:
            with open(self._correction_path(pid), encoding="utf-8") as fh:
                first = fh.readline().rstrip("\n")
        except OSError:
            return False
        return first == _POST_FULL_HEADER % pid

    def _post_full_sent(self, pid):
        """Set a sent post-full correction aside so it is never resent (and a
        later self-verify correction can reuse the path)."""
        if not self._post_full_correction(pid):
            return
        try:
            os.replace(self._correction_path(pid), os.path.join(
                self.state_dir, "corrections", pid + ".post-full.sent.md"))
        except OSError:
            pass

    def _dispatch(self, pid, prompt_files, no_resume, kind="implement", attempt=1):
        """Prepare + dispatch, bracketed by dispatch-start / dispatch-end events."""
        self._emit("dispatch-start", pid, {
            "attempt": attempt, "cls": self.pkgs[pid]["cls"], "kind": kind})
        started = time.time()
        end = {"status": "error", "executor": None}
        try:
            session = self.orca.session(pid) if self.orca is not None else None
            if session is not None:
                # A live Orca session already holds the contract and its code.
                token, result = None, self._orca_prompt(pid, session, prompt_files, None)
            else:
                token, result = self._dispatch_once(pid, prompt_files, no_resume)
                if result.get("status") == "delegate" and self.orca is not None:
                    result = self._orca_start(pid, token, result, prompt_files)
            end = {"status": result.get("status"), "executor": result.get("executor")}
            return token, result
        finally:
            self._emit("dispatch-end", pid, dict(end, seconds=round(time.time() - started, 3)))

    def _dispatch_once(self, pid, prompt_files, no_resume):
        """Prepare + dispatch with the one unavailable retry.

        Returns (token, result); result status "_stop" means the executor
        pool is exhausted and the reason is in result["reason"].
        """
        pkg = self.pkgs[pid]
        self._clean_integration("pre-dispatch")
        token = self.executor.prepare(prompt_files, pkg["cls"], self.int_path,
                                      self._task(pid), self.opts.get("run_id"))
        for attempt in (0, 1):
            with self.lock:
                exhausted = sorted(self.exhausted)
            result = self.executor.dispatch(token, cls=pkg["cls"], exhausted=exhausted,
                                            no_resume=no_resume)
            status = result.get("status")
            if status == "unavailable":
                with self.lock:
                    if result.get("executor"):
                        self.exhausted.add(result["executor"])
                    exhausted = sorted(self.exhausted)
                if attempt == 0:
                    continue
            if status in ("unavailable", "unroutable"):
                return token, {"status": "_stop", "reason": "executor unavailable: %s" % (
                    ", ".join(exhausted) or "no route")}
            return token, result
        return token, result

    def _orca_result_path(self, pid):
        return os.path.join(self.state_dir, "orca", pid + ".result.json")

    def _orca_need(self, token, exc, pid=None):
        detail = exc.detail
        if exc.kind == "stalled" and "dialog:" in detail:
            detail += (
                "\napprove it in Orca (terminal %s), then run: agent-exec wave mark "
                "--state %s --await %s"
                "\nor add the directory to orca.add_dirs / set orca.permission_mode: "
                "auto and re-dispatch with --status pending"
                % (self._orca_handle(detail), self.state_path, pid or "ID")
            )
        return {"status": "_orca_need", "kind": exc.kind, "detail": detail,
                "token": token, "executor": "orca"}

    def _orca_handle(self, detail):
        match = re.search(r"\nterminal: ([^\n]+)", detail)
        return match.group(1) if match else "unknown"

    def _orca_start(self, pid, token, delegated, prompt_files):
        """Run a `delegate` package in a new Orca session instead of handing it
        to the instructor. Returns a dispatch-shaped result."""
        # The CLI dispatch already made an orchestra tree for the task; drop
        # it (it holds nothing yet) so the task id resolves to the Orca tree.
        isolation = delegated.get("isolation") or {}
        stray = isolation.get("path")
        if isolation.get("isolate") and stray and agent_exec._adopted_task(stray) is None:
            removed = agent_exec.isolate_remove(self.root, self._task(pid))
            if removed.get("status") not in ("removed", "absent"):
                return self._orca_need(token, agent_exec_orca.OrcaNeed(
                    "error", "could not drop the dispatch worktree %s (%s)"
                    % (stray, removed.get("status"))))
        cls = self.pkgs[pid]["cls"]
        try:
            session = self.orca.start(
                pid, self.root, self.int_branch, cls,
                self.orca.model_for(cls, delegated.get("model")),
                self.opts.get("run_id"), self.state_dir, add_dirs=self._orca_add_dirs())
        except agent_exec_orca.OrcaNeed as exc:
            return self._orca_need(token, exc, pid)
        self.store.update(pid, tree=session["worktree"], executor="orca", session=True)
        if self._context_path(pid) in prompt_files:
            self._write_context(pid, self.notes.get(pid, ""))  # WORKING TREE = the Orca tree
        return self._orca_prompt(pid, session, prompt_files, token)

    def _orca_add_dirs(self):
        """Directories outside the worktree an Orca session must reach: the
        state dir (context, corrections, result files) and every directory
        holding a preamble or spec file of the plan."""
        dirs = {self.state_dir, self.root}
        for path in list(self.plan["preamble"]) + [p["spec"] for p in self.plan["packages"]]:
            dirs.add(os.path.dirname(os.path.abspath(path)))
        for path in self.orca.cfg.get("add_dirs") or []:
            expanded = os.path.expanduser(os.path.expandvars(str(path)))
            if not os.path.isabs(expanded):
                expanded = os.path.join(self.root, expanded)
            dirs.add(os.path.abspath(expanded))
        return sorted(dirs)

    def _orca_prompt(self, pid, session, prompt_files, token):
        try:
            return self.orca.prompt(session, prompt_files, self._orca_result_path(pid),
                                    self.orca.cfg["task_timeout"])
        except agent_exec_orca.OrcaNeed as exc:
            return self._orca_need(token, exc, pid)

    def _orca_await(self, pid, session):
        try:
            result = self.orca.wait_result(
                session, self._orca_result_path(pid), self.orca.cfg["task_timeout"])
            summary = str(result.get("summary") or "")
            answer = ("ESCALATE: " + summary
                      if result["status"] == "escalate" else summary)
            return {
                "status": "ok", "answer": answer, "session_id": session.get("terminal"),
                "resumed": True, "executor": "orca",
                "isolation": {"isolate": True, "path": session["worktree"],
                              "workdir": session["worktree"]},
            }
        except agent_exec_orca.OrcaNeed as exc:
            return self._orca_need(None, exc, pid)

    def _orca_close(self, pid):
        if self.orca is None:
            return
        session = self.orca.session(pid)
        if session is not None:
            self.orca.close(session, remove_worktree=True)

    def _handle(self, pid, token, result):
        """Record a dispatch result. True when the package moved to verifying."""
        status = result.get("status")
        if status == "_orca_need":
            self._need(pid, "delegate", json.dumps({
                "orca": result["kind"], "detail": result["detail"],
                "token": result.get("token") or token,
            }, ensure_ascii=False))
            return False
        if status == "_stop":
            self.store.set_status(pid, "pending", detail=result["reason"])
            if (self.opts.get("resume_on_reset")
                    and result["reason"].startswith("executor unavailable")):
                self.unavailable = True
            else:
                self._stop(result["reason"])
            return False
        isolation = result.get("isolation") or {}
        tree = isolation.get("workdir") or isolation.get("path")
        if status == "ok":
            answer = result.get("answer") or ""
            self.answers[pid] = answer
            if _first_line(answer).startswith("ESCALATE"):
                self._escalate_need(pid, answer)
                return False
            if not isolation.get("isolate") or not tree:
                self._need(pid, "dispatch-error", "dispatch did not run isolated: %s"
                           % (isolation.get("reason") or "no isolation info"))
                return False
            entry = self.store.load()["packages"].get(pid, {})
            self.store.set_status(
                pid, "verifying", executor=result.get("executor"),
                session=bool(result.get("session_id")) or bool(entry.get("session")),
                tree=tree,
            )
            return True
        if status == "delegate":
            self.store.update(pid, tree=tree, executor=result.get("executor"))
            self._need(pid, "delegate", json.dumps({
                "token": token, "agent_type": result.get("agent_type"),
                "model": result.get("model"), "effort": result.get("effort"), "tree": tree,
            }, ensure_ascii=False))
            return False
        detail = result.get("reason") or json.dumps(
            dict((k, result.get(k)) for k in ("status", "exit_code", "answer")),
            ensure_ascii=False)
        self._need(pid, "dispatch-error", agent_exec_checks.failure_excerpt(str(detail)))
        return False

    def _escalate_need(self, pid, answer):
        detail = answer
        carry_path = os.path.join(self.state_dir, "carry", pid + ".carry.md")
        try:
            with open(carry_path, encoding="utf-8") as fh:
                carry = fh.read()
        except OSError:
            carry = ""
        if carry:
            detail += "\n--- carry ---\n" + carry
        owned = self.pkgs[pid].get("files_owned") or []
        paths = []
        for line in detail.splitlines():
            for token in line.replace(",", " ").split():
                path = token.strip("`'\"()[]{}<>:;,!?").rstrip(".")
                if not path or os.path.isabs(path) or path.startswith("-"):
                    continue
                if not _PATH_LIKE.match(path):
                    continue
                if not any(agent_exec_checks.glob_match(glob, path) for glob in owned):
                    if path not in paths:
                        paths.append(path)
        if paths:
            detail += "\n--- outside files_owned ---\n" + "\n".join(paths)
            self._need(pid, "scope", detail)
        else:
            self._need(pid, "escalate", detail)

    def _empty_after_refresh(self, pid, answer):
        info = self.refresh_info.get(pid) or {}
        if not info.get("fresh") or not info.get("carried"):
            return False
        if self._tree_files(pid) != []:
            return False
        patch_file = info.get("patch_file") or "(patch file unavailable)"
        self._need(pid, "empty-after-refresh",
                   "%s\n%s" % ("\n".join((answer or "").splitlines()[:10]), patch_file))
        return True

    def _check(self, pid):
        args = ["--task", self._task(pid), "--baseline", "--json", "--repo", self.root]
        session = agent_exec._current_session()
        if session:
            args.extend(["--session", session])
        buf = _Buffer()
        tree = self.store.load()["packages"].get(pid, {}).get("tree") or ""
        self._emit("check-start", pid, {"name": "self-verify", "tree": tree})
        started = time.time()
        self.stdout.capture(buf)
        try:
            rc = agent_exec.cmd_check(args)
        finally:
            self.stdout.release()
        try:
            parsed = json.loads(buf.getvalue().strip().splitlines()[-1])
        except (ValueError, IndexError):
            parsed = {"status": "error", "checks": []}
        parsed["exit"] = rc
        self._emit("check-end", pid, {"name": "self-verify", "status": parsed.get("status"),
                                      "seconds": round(time.time() - started, 3)})
        return parsed

    def _failure_text(self, check):
        parts = []
        for item in check.get("checks") or []:
            if item.get("status") == "fail":
                parts.append("## %s\n%s" % (item.get("name"), item.get("excerpt") or ""))
        return "\n\n".join(parts) or "check status %s" % check.get("status")

    def _verify(self, pid):
        for round_no in (0, 1):
            check = self._check(pid)
            if check.get("flaky"):
                self._emit("flaky", pid, {"check": "self-verify",
                                          "files": check["flaky"]})
            status = check.get("status")
            if status in _PASSING_CHECK:
                if self._empty_after_refresh(pid, self.answers.get(pid, "")):
                    return
                self.store.set_status(pid, "ready", files_changed=check.get("files"))
                return
            if status != "fail":
                self._need(pid, "self-verify", "check could not run (status %s)" % status)
                return
            failures = self._failure_text(check)
            if round_no == 1:
                if _first_line(failures).startswith("ESCALATE"):
                    self._escalate_need(pid, failures)
                    return
                self._need(pid, "self-verify", failures)
                return
            correction = self._correction_path(pid)
            with open(correction, "w", encoding="utf-8") as fh:
                fh.write(
                    "# Correction for %s\n\nThese checks failed on your changes. Fix "
                    "exactly this, stay inside files_owned, do not commit.\n\n%s\n"
                    % (pid, failures))
            entry = self.store.load()["packages"].get(pid, {})
            self.store.set_status(pid, "fixing", detail="self-verify failed",
                                  attempts=(entry.get("attempts") or 0) + 1)
            attempt = (entry.get("attempts") or 0) + 2
            if entry.get("session"):
                token, result = self._dispatch(
                    pid, [correction], no_resume=False, kind="correction", attempt=attempt)
            else:
                token, result = self._dispatch(
                    pid, self._full_files(pid) + [correction], no_resume=True,
                    kind="correction", attempt=attempt)
            if not self._handle(pid, token, result):
                return

    def _run_package(self, pid, phase, refresh_note):
        try:
            if phase == "await":
                result = self._orca_await(pid, self.orca.session(pid))
                if not self._handle(pid, None, result):
                    return
                self._verify(pid)
                return
            if phase == "implement":
                self.notes[pid] = refresh_note
                self._write_context(pid, refresh_note)
                self.store.set_status(pid, "implementing")
                post_full = self._post_full_correction(pid)
                token, result = self._dispatch(
                    pid, self._full_files(pid),
                    no_resume=post_full or bool(
                        (self.refresh_info.get(pid) or {}).get("fresh")))
                if post_full:
                    self._post_full_sent(pid)
                if not self._handle(pid, token, result):
                    return
            self._verify(pid)
        except Exception as exc:  # a worker thread must never take the loop down
            self._need(pid, "dispatch-error", "runner error: %s: %s"
                       % (type(exc).__name__, exc))

    # -- integrate / full -----------------------------------------------------------

    def _default_gate(self, since):
        argv = " ".join(shlex.quote(a) for a in _agent_exec_argv())
        return ("%s check --path . --since %s --baseline --text; rc=$?; "
                "[ $rc -eq 0 ] || [ $rc -eq 4 ]" % (argv, shlex.quote(since)))

    def _commit_for(self, task, since):
        rc, out = agent_exec._git(self.int_path, "log", "--format=%H %s", "%s..HEAD" % since)
        if rc != 0:
            return None
        subject = "orchestra integrate %s" % task
        for line in out.splitlines():
            sha, _, rest = line.partition(" ")
            if rest == subject:
                return sha
        return None

    def _integrate(self):
        state = self.store.load()
        ready = [p["id"] for p in self.plan["packages"]
                 if state["packages"].get(p["id"], {}).get("status") == "ready"]
        if not ready:
            return 0
        since = self._int_head()
        gate = self.opts.get("gate") or self._default_gate(since)
        tasks = [self._task(pid) for pid in ready]
        by_sanitized = dict((agent_exec.sanitize_task_id(t), pid)
                            for t, pid in zip(tasks, ready))

        def progress(record):
            detail = dict(record.get("detail") or {})
            if isinstance(detail.get("tasks"), list):
                detail["tasks"] = [by_sanitized.get(t, t) for t in detail["tasks"]]
            pkg = record.get("pkg")
            self._emit(record["event"], by_sanitized.get(pkg, pkg) if pkg else None, detail)

        result = agent_exec.isolate_integrate(
            self.root, tasks, onto=self.base, into=self.into, on_conflict="skip",
            verify=gate, bisect=True, progress=progress,
        )
        self.store.event("integrate", detail="%s: %s" % (result.get("status"), result.get("note")))
        if result.get("status") == "error":
            for pid in ready:
                self._need(pid, "dispatch-error", result.get("note") or "integrate failed")
            return 0
        by_task = dict((t.get("task"), t) for t in result.get("tasks") or [])
        integrated = 0
        for pid in ready:
            entry = by_task.get(agent_exec.sanitize_task_id(self._task(pid))) or {}
            status = entry.get("status")
            if status == "applied":
                self.store.set_status(pid, "integrated",
                                      commit=self._commit_for(self._task(pid), since))
                integrated += 1
                self._orca_close(pid)
            elif status == "empty":
                self.store.set_status(pid, "integrated", detail="changed nothing")
                self._orca_close(pid)
            elif status == "conflicted":
                files = [c.get("file") for c in entry.get("conflicts") or [] if c.get("file")]
                self._need(pid, "conflict", json.dumps(
                    {"files": files, "stage": "integrate"},
                    ensure_ascii=False, sort_keys=True))
            elif status == "reverted":
                self._need(pid, "post-integration", entry.get("verify_excerpt") or "")
            else:
                self._need(pid, "dispatch-error", "integrate reported %s" % status)
        verify = result.get("verify") or {}
        if verify.get("dirtied"):
            self._emit("integration-dirty", None, {
                "files": verify["dirtied"], "after": "gate"})
        if verify.get("status") == "fail":
            if verify.get("baseline") == "fail":
                self._stop("integration tree red before this wave")
            else:
                self._stop("integration tree red after bisect")
        self.integrated_since_full += integrated
        return integrated

    def _run_cmd(self, cmd, timeout):
        return agent_exec._run_verify_cmd(self.int_path, cmd, timeout)

    def _green_tree(self, sha):
        if not sha:
            return False
        if not os.path.isdir(self.green_path):
            os.makedirs(os.path.dirname(self.green_path), exist_ok=True)
            rc, _ = agent_exec._git(
                self.root, "worktree", "add", "--detach", self.green_path, sha)
            if rc != 0:
                return False
            for rel in agent_exec.detect_carry_dirs(self.root):
                agent_exec.copy_tree_fast(
                    os.path.join(self.root, rel), os.path.join(self.green_path, rel))
        else:
            rc, _ = agent_exec._git(
                self.green_path, "checkout", "-q", "--detach", sha)
            if rc != 0:
                return False
            agent_exec._git(self.green_path, "clean", "-fdq")
        return True

    def _on_green(self, sha):
        command = self.opts.get("on_green")
        if not command:
            return
        if not self._green_tree(sha):
            self._emit("on-green", None, {"sha": sha, "exit": None, "seconds": 0.0})
            return
        started = time.time()
        command = command.replace("{sha}", sha)
        try:
            proc = subprocess.run(
                ["/bin/sh", "-c", command], cwd=self.green_path,
                env=dict(os.environ, WAVE_GREEN_SHA=sha),
                capture_output=True, text=True,
                timeout=self.opts.get("full_timeout"))
            exit_code = proc.returncode
            output = (proc.stdout or "") + (proc.stderr or "")
        except (OSError, ValueError, subprocess.TimeoutExpired) as exc:
            exit_code, output = None, str(exc)
        agent_exec._git(self.green_path, "reset", "-q", "--hard", "HEAD")
        agent_exec._git(self.green_path, "clean", "-fdq")
        self._emit("on-green", None, {
            "sha": sha, "exit": exit_code, "seconds": time.time() - started,
        })

    def _wait_for_reset(self):
        now = self.clock()
        until = now + 300
        try:
            cfg, error = agent_exec.resolve_config()
            if not error:
                expiries = agent_exec.active_cooldown_expiries(cfg, now)
                values = [expiries[name] for name in self.exhausted if name in expiries]
                if values:
                    until = min(values)
        except (OSError, TypeError, ValueError):
            pass
        stop_at = self.opts.get("stop_at")
        if stop_at is not None and stop_at <= until:
            self._stop("stop-at reached")
            return False
        self._emit("resume-wait", None, {"until": until, "reason": "executor unavailable"})
        while self.clock() < until:
            reason = self._stop_check()
            if reason is not None:
                self._stop(reason)
                return False
            self.sleep(min(30, max(0, until - self.clock())))
        self.exhausted.clear()
        self.unavailable = False
        return True

    def _green_file(self):
        return os.path.join(self.state_dir, "green.json")

    def _last_green(self):
        try:
            with open(self._green_file(), encoding="utf-8") as fh:
                sha = json.load(fh).get("sha")
        except (OSError, ValueError, AttributeError):
            return None
        return sha if isinstance(sha, str) and sha else None

    def _record_green(self, sha):
        if not sha:
            return
        tmp = self._green_file() + ".tmp"
        with open(tmp, "w", encoding="utf-8") as fh:
            json.dump({"sha": sha, "at": float(self.clock())}, fh)
            fh.write("\n")
        os.replace(tmp, self._green_file())

    def _full_green(self):
        sha = self._int_head()
        self._record_green(sha)
        self._on_green(sha)

    def _check_slots(self):
        """(slot_dir, max_parallel) shared with `agent-exec check`."""
        try:
            cfg, error = agent_exec.resolve_config()
        except (OSError, TypeError, ValueError):
            cfg, error = None, "config"
        if error or not cfg:
            return os.path.join(self.state_dir, "check-slots"), 2
        max_parallel = (cfg.get("checks") or {}).get("max_parallel", 2)
        return os.path.join(os.path.dirname(os.path.abspath(
            agent_exec._ledger_dir_from_cfg(cfg))), "check-slots"), max_parallel

    def _full_ensure(self, tree):
        try:
            cfg, error = agent_exec.resolve_config()
        except (OSError, TypeError, ValueError):
            cfg, error = None, "config"
        if error or not cfg:
            return []
        checks = cfg.get("checks") or {}
        slot_dir, max_parallel = self._check_slots()
        return agent_exec_checks.run_ensure(
            checks.get("ensure") or [], tree, slot_dir, max_parallel)

    def _ensure_failure(self, tree):
        results = self._full_ensure(tree)
        failed = next((result for result in results if result.get("status") == "fail"), None)
        if failed is None:
            return None
        excerpt = agent_exec_checks.failure_excerpt(
            "%s\n%s" % (failed.get("name", "ensure"), failed.get("excerpt") or ""))
        return {"result": failed, "excerpt": excerpt}

    def _environment_failure(self, sha, failure):
        excerpt = failure.get("excerpt") or ""
        detail = ("full verification red at the last green SHA %s — environment, "
                  "not code\nensure results: %s\n%s" % (
                      sha, failure.get("result", {}).get("name", "ensure"), excerpt))
        self._need(None, "environment", detail)
        self._emit("environment", None, {"sha": sha, "excerpt": excerpt})
        self._stop("environment: full verification red at the last green SHA")

    def _full_flaky(self, res):
        """Re-run the red full verification's failing files alone with
        `--full-retry`; True when that passed (the red was flaky)."""
        template = self.opts.get("full_retry")
        if not template:
            return False
        slot_dir, max_parallel = self._check_slots()
        retry = agent_exec_checks.rerun_failed_alone(
            res.get("excerpt") or "", template, self.int_path, slot_dir,
            max_parallel, self.opts.get("full_timeout"))
        self._clean_integration("full-retry")
        if retry["status"] != "pass":
            return False
        self._emit("flaky", None, {"check": "full", "files": retry["failed"]})
        return True

    def _full_candidates(self, green):
        """`orchestra integrate <task>` commits in green..HEAD, oldest first,
        as (task, sha); commits a later `orchestra revert <task>` undid (the
        gate's own bisect) are not candidates."""
        rc, out = agent_exec._git(
            self.int_path, "log", "--reverse", "--format=%H %s", "%s..HEAD" % green)
        if rc != 0:
            return []
        known = set(agent_exec.sanitize_task_id(self._task(pid)) for pid in self.pkgs)
        candidates = []
        for line in out.splitlines():
            sha, _, subject = line.partition(" ")
            for verb in ("integrate", "revert"):
                prefix = "orchestra %s " % verb
                task = subject[len(prefix):] if subject.startswith(prefix) else None
                if task not in known:
                    continue
                if verb == "integrate":
                    candidates.append((task, sha))
                else:
                    for i in range(len(candidates) - 1, -1, -1):
                        if candidates[i][0] == task:
                            del candidates[i]
                            break
        return candidates

    def _restore_integration(self, branch):
        """Never leave the integration tree detached, mid-revert or dirty."""
        agent_exec._git(self.int_path, "revert", "--quit")
        if branch:
            rc, out = agent_exec._git(self.int_path, "rev-parse", "--abbrev-ref", "HEAD")
            if rc != 0 or out.strip() != branch:
                agent_exec._git(self.int_path, "checkout", "-q", "-f", branch)
        self._clean_integration("full-bisect")

    def _post_full_revert(self, pid, excerpt):
        """A red `--full` was bisected to `pid` and its commit reverted:
        re-dispatch it with a correction, or hand it over the second time."""
        failed = agent_exec_checks.parse_failed_files(excerpt)
        excerpt = agent_exec_checks.failure_excerpt(excerpt)
        previous = [e for e in agent_exec_wave.read_events(self.state_path, None)
                    if e.get("event") == "status" and e.get("pkg") == pid
                    and e.get("to") == "pending" and e.get("detail") == _POST_FULL_DETAIL]
        if previous:
            self._need(pid, "post-integration", "full verification red after "
                       "integration (reverted twice); failing tests: %s\n%s"
                       % (", ".join(failed) or "(none parsed)", excerpt))
            return False
        with open(self._correction_path(pid), "w", encoding="utf-8") as fh:
            fh.write(
                (_POST_FULL_HEADER % pid) + "\n\nYour integrated change broke the full "
                "verification after integration, so it was reverted from the "
                "integration tree. Failing tests: %s\n\nExcerpt:\n\n%s\n\nFix it "
                "inside files_owned; do not commit.\n"
                % (", ".join(failed) or "(none parsed)", excerpt))
        self.store.set_status(pid, "pending", detail=_POST_FULL_DETAIL, commit=None)
        return True

    def _full_bisect(self):
        """Bisect a red `--full` over the integrate commits since the last
        green SHA, reverting culprits (`_bisect_integration`, up to
        _FULL_BISECT_MAX rounds). Returns the ids sent back to `pending`."""
        green = self._last_green() or self.base
        rc, out = agent_exec._git(self.int_path, "rev-parse", "--abbrev-ref", "HEAD")
        branch = out.strip() if rc == 0 and out.strip() != "HEAD" else None
        if branch is None:
            self._stop("full verification red")
            return []
        candidates = self._full_candidates(green)
        by_task = dict((agent_exec.sanitize_task_id(self._task(pid)), pid)
                       for pid in self.pkgs)
        results = dict((task, {"task": task, "status": "applied"})
                       for task, _ in candidates)

        def progress(record):
            pkg = record.get("pkg")
            self._emit(record["event"], by_task.get(pkg, pkg) if pkg else None,
                       dict(record.get("detail") or {}))

        original_verify = agent_exec._run_verify_cmd

        def verify_with_ensure(tree, command, timeout):
            failure = self._ensure_failure(tree)
            if failure is not None:
                return {
                    "status": "fail", "exit": failure["result"].get("exit"),
                    "seconds": failure["result"].get("seconds", 0),
                    "excerpt": failure["excerpt"], "dirtied": 0,
                    "ensure_failed": True,
                }
            return original_verify(tree, command, timeout)

        agent_exec._run_verify_cmd = verify_with_ensure
        try:
            verify = agent_exec._bisect_integration(
                self.root, self.int_path, branch, green, candidates, results,
                self.opts["full"], self.opts.get("full_timeout"), _FULL_BISECT_MAX,
                progress=progress)
        finally:
            agent_exec._run_verify_cmd = original_verify
            self._restore_integration(branch)
        if verify.get("baseline") == "fail" and verify.get("ensure_failed"):
            self._environment_failure(green, {
                "result": {"name": "ensure"},
                "excerpt": verify.get("excerpt") or "",
            })
            return []
        if verify.get("baseline") == "fail":
            self._stop("full verification red at the last green SHA")
            return []
        reset = []
        for task, _ in candidates:
            entry = results[task]
            if entry.get("status") == "reverted":
                if self._post_full_revert(by_task[task], entry.get("verify_excerpt") or ""):
                    reset.append(by_task[task])
        if verify.get("status") == "pass":
            self._full_green()
        else:
            self._stop("full verification red")
        return reset

    def _full(self):
        """Run `--full` at the integration tip. Red goes through the
        `--full-retry` flaky filter, then bisect-and-revert; returns the
        package ids a revert sent back to `pending`."""
        self.integrated_since_full = 0
        self._emit("full-start", None, {})
        ensure_failure = self._ensure_failure(self.int_path)
        if ensure_failure is None:
            res = self._run_cmd(self.opts["full"], self.opts.get("full_timeout"))
        else:
            res = {
                "status": "fail", "exit": ensure_failure["result"].get("exit"),
                "seconds": ensure_failure["result"].get("seconds", 0),
                "excerpt": ensure_failure["excerpt"], "dirtied": 0,
                "ensure_failed": True,
            }
        if res.get("dirtied"):
            self._emit("integration-dirty", None, {
                "files": res["dirtied"], "after": "full"})
        self._emit("full-end", None, {"status": res["status"], "seconds": res["seconds"]})
        self.store.event("full", detail="%s exit=%s\n%s" % (
            res["status"], res["exit"], res["excerpt"]))
        if res["status"] == "pass" or (
                not res.get("ensure_failed") and self._full_flaky(res)):
            self._full_green()
            return []
        return self._full_bisect()

    # -- the loop -------------------------------------------------------------

    def loop(self):
        while True:
            self._waves()
            if not (self.opts.get("full") and self.integrated_since_full):
                return
            # A final red `--full` that sent packages back re-enters the loop.
            if not self._full() or self.stop_reason is not None:
                return

    def _waves(self):
        while True:
            state = self.store.load()
            pkg_states = state["packages"]
            carried = [p["id"] for p in self.plan["packages"]
                       if pkg_states.get(p["id"], {}).get("status") in ("verifying", "ready")]
            awaiting = [p["id"] for p in self.plan["packages"]
                        if pkg_states.get(p["id"], {}).get("status") == "implementing"
                        and pkg_states.get(p["id"], {}).get("detail") == "await-orca"
                        and self.orca is not None and self.orca.session(p["id"]) is not None]
            selected = agent_exec_wave_plan.select(
                self.plan, pkg_states, self.opts["max_in_flight"])
            if not selected and not carried and not awaiting:
                break
            reason = self._stop_check()
            if reason is not None:
                self._stop(reason)
                selected = []
                if not carried:
                    break
            elif self.opts.get("max_packages") is not None:
                selected = selected[:max(self.opts["max_packages"] - self.started, 0)]

            self.waves += 1
            self.store.set_wave(state.get("wave", 0) + 1)
            self.store.event("wave-start", detail=",".join(carried + awaiting + selected))

            head = self._int_head()
            jobs = [(pid, "verify", "") for pid in carried
                    if pkg_states[pid]["status"] == "verifying"]
            jobs.extend((pid, "await", "") for pid in awaiting)
            for pid in selected:
                ok, note = self._refresh(pid, head)
                if ok:
                    jobs.append((pid, "implement", note))
            self.started += len(selected)

            if jobs:
                workers = max(1, min(self.opts["max_in_flight"], len(jobs)))
                with concurrent.futures.ThreadPoolExecutor(max_workers=workers) as pool:
                    futures = [pool.submit(self._run_package, *job) for job in jobs]
                    for future in futures:
                        future.result()

            if self.unavailable:
                if not self._wait_for_reset():
                    break
                continue
            self._integrate()
            full_every = self.opts.get("full_every") or 1
            if (self.opts.get("full") and self.integrated_since_full
                    and self.waves % full_every == 0 and self.stop_reason is None):
                self._full()
            if self.stop_reason is not None:
                break

    def report(self):
        state = self.store.load()
        pkg_states = state["packages"]
        order = [p["id"] for p in self.plan["packages"]]
        blocked_ids = agent_exec_wave_plan.blocked(self.plan, pkg_states)

        def with_status(status):
            return [pid for pid in order if pkg_states.get(pid, {}).get("status") == status]

        threshold = 3
        try:
            resolved, _err = agent_exec.resolve_config()
            threshold = int(((resolved or {}).get("checks") or {}).get(
                "flaky_threshold", threshold))
        except (ValueError, TypeError, AttributeError):
            pass
        flaky = {}
        for event in agent_exec_wave.read_events(self.state_path, None):
            if event.get("event") != "flaky":
                continue
            try:
                files = json.loads(event.get("detail") or "{}").get("files") or []
            except ValueError:
                files = []
            for path in files:
                flaky[path] = flaky.get(path, 0) + 1
        return {
            "status": "stopped" if self.stop_reason is not None else "done",
            "reason": self.stop_reason,
            "waves": self.waves,
            "integrated": with_status("integrated"),
            "needs": [{"id": n.get("id"), "kind": n.get("kind")}
                      for n in state.get("needs") or []],
            "pending": [pid for pid in with_status("pending") if pid not in blocked_ids],
            "blocked": [pid for pid in with_status("pending") if pid in blocked_ids],
            "flaky_tests": [{"file": path, "count": count}
                            for path, count in sorted(
                                flaky.items(), key=lambda item: (-item[1], item[0]))
                            if count >= threshold],
        }

    def join_notifications(self):
        with self.lock:
            threads = list(self.notify_threads)
        for thread in threads:
            thread.join(_NOTIFY_TIMEOUT + 5)


def run_wave(opts, executor=None, clock=time.time, orca=None, sleep=time.sleep):
    """Drive the plan to done or stopped; returns the end report dict.

    A report with status "error" (plus "reason") means the loop never
    started: bad plan, not a repository, no integration worktree.

    `orca` is an agent_exec_orca.OrcaExecutor (or None). Left at None with
    the default executor it comes from config; an injected executor without
    `orca` never touches Orca.
    """
    merged = default_opts()
    merged.update(opts)
    if orca is None and executor is None:
        orca = agent_exec_orca.OrcaExecutor.from_config()
    runner = _Runner(merged, executor or CliExecutor(), clock, orca=orca, sleep=sleep)
    try:
        runner.init()
    except agent_exec_wave_plan.PlanError as exc:
        return {"status": "error", "reason": str(exc), "kind": "plan"}
    except RuntimeError as exc:
        return {"status": "error", "reason": str(exc), "kind": "environment"}

    previous_stdout = sys.stdout
    runner.stdout = _ThreadStdout(previous_stdout)
    sys.stdout = runner.stdout
    if not merged.get("no_ui") and os.environ.get("ORCHESTRA_WAVE_NO_UI") != "1":
        try:
            url = agent_exec_ui.start_or_reuse()["url"]
            sys.stderr.write("ui: %s\n" % url)
            runner._emit("ui", None, {"url": url})
        except Exception as exc:
            sys.stderr.write("warning: could not start ui: %s\n" % exc)
    try:
        runner.loop()
    finally:
        sys.stdout = previous_stdout
    report = runner.report()
    if report["status"] == "done":
        runner._notify("done", dict(report, event="done", at=clock()))
    runner.join_notifications()
    if report["status"] == "done" and os.path.isdir(runner.green_path):
        agent_exec._git(runner.root, "worktree", "remove", "--force", runner.green_path)
    return report


def exit_code_for(report):
    status = report.get("status")
    if status == "stopped":
        return 5
    if status == "done":
        return 1 if report.get("needs") else 0
    return 2 if report.get("kind") == "plan" else 3


def format_report_text(report):
    if report.get("status") == "error":
        return "wave run error: %s" % report.get("reason")
    line = "wave run %s  waves=%d" % (report.get("status"), report.get("waves", 0))
    if report.get("reason"):
        line += "  reason: %s" % report["reason"]
    lines = [line]
    for key in ("integrated", "pending", "blocked"):
        if report.get(key):
            lines.append("  %s: %s" % (key, ", ".join(report[key])))
    if report.get("needs"):
        lines.append("  needs: %s" % ", ".join(
            "%s(%s)" % (n["id"], n["kind"]) for n in report["needs"]))
    return "\n".join(lines)


# --- CLI ---------------------------------------------------------------------------


_RUN_USAGE = (
    "usage: agent-exec wave run --plan PLAN --state STATE --into TASK [--repo P]\n"
    "                  [--max-in-flight N] [--gate CMD] [--full CMD] [--full-every N]\n"
    "                  [--full-timeout SEC] [--full-retry CMD] [--on-green CMD]\n"
    "                  [--resume-on-reset]\n"
    "                  [--no-ui] [--notify-cmd CMD]\n"
    "                  [--stop-at EPOCH] [--max-waves N] [--max-packages N]\n"
    "                  [--run-id ID] [--json|--text]\n"
)

_MARK_USAGE = (
    "usage: agent-exec wave mark --state STATE --pkg ID --status ready|pending|failed\n"
    "                  or --state STATE --await ID\n"
    "                  [--widen GLOB[,GLOB...]] | --recheck ID[,ID...]\n"
    "                  (ready enters self-verifying; pending|failed are terminal handoffs)\n"
    "                  [--detail TEXT] | --clear-environment\n"
)


def parse_run_args(args):
    """Parse `wave run` flags. Returns (options, error_message)."""
    opts = default_opts()
    seen = set()
    value_flags = {
        "--plan": ("plan", str), "--state": ("state", str), "--into": ("into", str),
        "--repo": ("repo", str), "--max-in-flight": ("max_in_flight", int),
        "--gate": ("gate", str), "--full": ("full", str),
        "--full-every": ("full_every", int), "--full-timeout": ("full_timeout", float),
        "--full-retry": ("full_retry", str),
        "--on-green": ("on_green", str), "--notify-cmd": ("notify_cmd", str),
        "--stop-at": ("stop_at", float), "--max-waves": ("max_waves", int),
        "--max-packages": ("max_packages", int), "--run-id": ("run_id", str),
    }
    i = 0
    while i < len(args):
        tok = args[i]
        if tok in seen:
            return None, "duplicate option: %s" % tok
        if tok in value_flags:
            if i + 1 >= len(args):
                return None, "missing value for %s" % tok
            key, kind = value_flags[tok]
            try:
                opts[key] = kind(args[i + 1])
            except ValueError:
                return None, "invalid %s value: %s" % (tok, args[i + 1])
            if kind in (int, float) and opts[key] < 0:
                return None, "invalid %s value: %s" % (tok, args[i + 1])
            seen.add(tok)
            i += 2
            continue
        if tok in ("--json", "--text", "--resume-on-reset", "--no-ui"):
            seen.add(tok)
            if tok == "--text":
                opts["text"] = True
            elif tok == "--resume-on-reset":
                opts["resume_on_reset"] = True
            elif tok == "--no-ui":
                opts["no_ui"] = True
            i += 1
            continue
        return None, "unknown option: %s" % tok
    if "--json" in seen and "--text" in seen:
        return None, "--json and --text are mutually exclusive"
    for flag in ("--plan", "--state", "--into"):
        if flag not in seen:
            return None, "missing required option: %s" % flag
    if opts["max_in_flight"] < 1:
        return None, "--max-in-flight must be at least 1"
    if opts["full_every"] < 1:
        return None, "--full-every must be at least 1"
    if opts["run_id"] is not None and not agent_exec._RUN_LEDGER_RUN_RE.fullmatch(opts["run_id"]):
        return None, "invalid --run-id value: %s" % opts["run_id"]
    return opts, None


def cmd_wave_run(args):
    opts, error = parse_run_args(args)
    if error is not None:
        sys.stderr.write("agent-exec: wave run: %s\n" % error)
        sys.stderr.write(_RUN_USAGE)
        return 2
    report = run_wave(opts, executor=CliExecutor(),
                      orca=agent_exec_orca.OrcaExecutor.from_config())
    if opts["text"]:
        print(format_report_text(report))
    else:
        print(json.dumps(report, ensure_ascii=False))
    return exit_code_for(report)


def cmd_wave_mark(args):
    state_path = None
    pkg = None
    status = None
    detail = ""
    widen = None
    recheck = None
    clear_environment = False
    await_pkg = None
    value_flags = {"--state": "state", "--pkg": "pkg", "--status": "status", "--detail": "detail",
                   "--widen": "widen", "--recheck": "recheck"}
    values = {}
    i = 0
    while i < len(args):
        tok = args[i]
        if tok == "--clear-environment":
            clear_environment = True
            i += 1
            continue
        if tok not in value_flags:
            if tok == "--await":
                if i + 1 >= len(args):
                    sys.stderr.write("agent-exec: wave mark: missing value for --await\n")
                    return 2
                await_pkg = args[i + 1]
                i += 2
                continue
            sys.stderr.write("agent-exec: wave mark: unknown option: %s\n" % tok)
            return 2
        if i + 1 >= len(args):
            sys.stderr.write("agent-exec: wave mark: missing value for %s\n" % tok)
            return 2
        values[value_flags[tok]] = args[i + 1]
        i += 2
    state_path = values.get("state")
    pkg = values.get("pkg")
    status = values.get("status")
    detail = values.get("detail", "")
    widen = values.get("widen")
    recheck = values.get("recheck")
    if widen is not None:
        widen = [item for item in widen.split(",") if item]
    if recheck is not None:
        recheck = [item for item in recheck.split(",") if item]
    if clear_environment and await_pkg is not None:
        sys.stderr.write(_MARK_USAGE)
        return 2
    if clear_environment:
        if not state_path or any(item in values for item in ("pkg", "status", "detail", "widen", "recheck")):
            sys.stderr.write(_MARK_USAGE)
            return 2
    elif await_pkg is not None:
        if (pkg or status or detail or widen is not None or recheck is not None
                or not state_path):
            sys.stderr.write(_MARK_USAGE)
            return 2
    elif not state_path or (not pkg and not recheck) or (not status and widen is None and recheck is None):
        sys.stderr.write(_MARK_USAGE)
        return 2
    if status is not None and status not in ("ready", "pending", "failed"):
        sys.stderr.write("agent-exec: wave mark: --status must be ready|pending|failed\n")
        return 2
    if not os.path.isfile(state_path):
        sys.stderr.write("agent-exec: wave mark: no state file at %s\n" % state_path)
        return 3
    store = agent_exec_wave.StateStore(state_path)
    if clear_environment:
        store.clear_need(None)
        store.clear_stopped()
        print(json.dumps({"environment_cleared": True}, ensure_ascii=False))
        return 0
    state = store.load()
    packages = state.get("packages", {})
    if await_pkg is not None:
        entry = packages.get(await_pkg)
        need = next((n for n in state.get("needs") or [] if n.get("id") == await_pkg), None)
        need_detail = {}
        try:
            need_detail = json.loads(need.get("detail") or "{}") if need else {}
        except (TypeError, ValueError):
            need_detail = {}
        need_kind = need.get("kind") if need else None
        is_orca_need = (need_kind in ("stalled", "timeout")
                        or (need_kind == "delegate"
                            and need_detail.get("orca") in ("stalled", "timeout")))
        if (entry is None or entry.get("status") != "needs" or not is_orca_need):
            sys.stderr.write("agent-exec: wave mark: --await requires an Orca stalled/timeout need\n")
            return 2
        orca = agent_exec_orca.OrcaExecutor.from_config()
        orca.state_dir = os.path.dirname(os.path.abspath(state_path))
        session = orca._load_registry().get(await_pkg)
        if (not isinstance(session, dict) or not session.get("terminal")
                or not session.get("worktree")
                or not os.path.isdir(session["worktree"])):
            sys.stderr.write("agent-exec: wave mark: no live Orca session for %s\n" % await_pkg)
            return 2
        store.clear_need(await_pkg)
        store.set_status(await_pkg, "implementing", detail="await-orca")
        print(json.dumps({"pkg": await_pkg, "status": "implementing",
                          "detail": "await-orca"}, ensure_ascii=False))
        return 0
    if recheck is not None:
        if any(item not in packages for item in recheck):
            bad = next(item for item in recheck if item not in packages)
            sys.stderr.write("agent-exec: wave mark: unknown package: %s\n" % bad)
            return 2
        if any(not packages[item].get("tree")
               or not os.path.isdir(packages[item].get("tree"))
               for item in recheck):
            bad = next(item for item in recheck
                       if not packages[item].get("tree")
                       or not os.path.isdir(packages[item].get("tree")))
            sys.stderr.write("agent-exec: wave mark: package has no tree: %s\n" % bad)
            return 2
        for item in recheck:
            store.clear_need(item)
            store.set_status(item, "verifying", detail=detail or "marked for recheck")
        print(json.dumps({"pkg": recheck, "status": "verifying"}, ensure_ascii=False))
        return 0
    if not pkg or pkg not in packages:
        sys.stderr.write("agent-exec: wave mark: unknown package: %s\n" % pkg)
        return 2
    if widen is not None:
        overrides_path = os.path.join(os.path.dirname(os.path.abspath(state_path)),
                                      "plan-overrides.json")
        try:
            with open(overrides_path, encoding="utf-8") as fh:
                overrides = json.load(fh)
        except (OSError, ValueError):
            overrides = {}
        entry = overrides.setdefault(pkg, {})
        owned = entry.setdefault("files_owned_add", [])
        for glob in widen:
            if glob not in owned:
                owned.append(glob)
        with open(overrides_path, "w", encoding="utf-8") as fh:
            json.dump(overrides, fh, ensure_ascii=False, indent=2)
            fh.write("\n")
        store.clear_need(pkg)
        store.set_status(pkg, "pending", detail=detail or "widened ownership")
        print(json.dumps({"pkg": pkg, "status": "pending", "widen": widen},
                         ensure_ascii=False))
        return 0
    stored_status = "verifying" if status == "ready" else status
    store.set_status(pkg, stored_status, detail=detail or "marked by the instructor")
    store.clear_need(pkg)
    print(json.dumps({"pkg": pkg, "status": stored_status}, ensure_ascii=False))
    return 0
