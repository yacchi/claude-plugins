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
anyway.
"""

import concurrent.futures
import json
import os
import shlex
import subprocess
import sys
import threading
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import agent_exec  # noqa: E402
import agent_exec_checks  # noqa: E402
import agent_exec_wave  # noqa: E402
import agent_exec_wave_plan  # noqa: E402

_HERE = os.path.dirname(os.path.abspath(__file__))

_NOTIFY_TIMEOUT = 30
# One `dispatch wait` poll. Short enough that no single call nears a 600 s
# foreground ceiling; the loop in CliExecutor.dispatch keeps polling.
_DISPATCH_POLL_SECONDS = 300

_PASSING_CHECK = frozenset(("pass", "no-checks", "preexisting"))


def _agent_exec_argv():
    """How to invoke agent-exec: this interpreter + the sibling agent_exec.py.

    Resolved from this file, never from PATH, so the loop always drives the
    same agent-exec it was started from (and inherits this interpreter's
    pyyaml).
    """
    return [sys.executable, os.path.join(_HERE, "agent_exec.py")]


# --- executor interface ------------------------------------------------------


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
        "full_timeout": None, "after_green": None, "notify_cmd": None,
        "stop_at": None, "max_waves": None, "max_packages": None,
        "run_id": None, "text": False,
    }


def _first_line(text):
    for line in (text or "").splitlines():
        if line.strip():
            return line.strip()
    return ""


class _Runner(object):
    def __init__(self, opts, executor, clock):
        self.opts = opts
        self.executor = executor
        self.clock = clock
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
        self.pkgs = dict((p["id"], p) for p in self.plan["packages"])
        self.root = agent_exec.repo_root(self.opts["repo"])
        if self.root is None:
            raise RuntimeError("not a git repository: %s" % self.opts["repo"])
        self.into = agent_exec.sanitize_task_id(self.opts["into"])
        self.store.init(os.path.abspath(self.opts["plan"]),
                        [p["id"] for p in self.plan["packages"]], self.into)
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

        os.makedirs(os.path.join(self.state_dir, "context"), exist_ok=True)
        os.makedirs(os.path.join(self.state_dir, "corrections"), exist_ok=True)
        os.makedirs(os.path.join(self.state_dir, "carry"), exist_ok=True)

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
            return True, ""
        baseline = agent_exec._read_baseline(entry.get("path"))
        if not baseline or baseline == head:
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
        if status != "ok":
            return True, ""
        old = refreshed.get("old_baseline")
        rc, out = agent_exec._git(self.int_path, "diff", "--name-only", "%s..%s" % (old, head))
        changed = [f for f in out.splitlines() if f.strip()] if rc == 0 else []
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
        return (list(self.plan["preamble"]) + [self._context_path(pid)]
                + [self.pkgs[pid]["spec"]])

    def _dispatch(self, pid, prompt_files, no_resume, kind="implement", attempt=1):
        """Prepare + dispatch, bracketed by dispatch-start / dispatch-end events."""
        self._emit("dispatch-start", pid, {
            "attempt": attempt, "cls": self.pkgs[pid]["cls"], "kind": kind})
        started = time.time()
        end = {"status": "error", "executor": None}
        try:
            token, result = self._dispatch_once(pid, prompt_files, no_resume)
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

    def _handle(self, pid, token, result):
        """Record a dispatch result. True when the package moved to verifying."""
        status = result.get("status")
        if status == "_stop":
            self.store.set_status(pid, "pending", detail=result["reason"])
            self._stop(result["reason"])
            return False
        isolation = result.get("isolation") or {}
        tree = isolation.get("workdir") or isolation.get("path")
        if status == "ok":
            answer = result.get("answer") or ""
            if _first_line(answer).startswith("ESCALATE"):
                self._need(pid, "escalate", answer)
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
            status = check.get("status")
            if status in _PASSING_CHECK:
                self.store.set_status(pid, "ready", files_changed=check.get("files"))
                return
            if status != "fail":
                self._need(pid, "self-verify", "check could not run (status %s)" % status)
                return
            failures = self._failure_text(check)
            if round_no == 1:
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
            if phase == "implement":
                self._write_context(pid, refresh_note)
                self.store.set_status(pid, "implementing")
                token, result = self._dispatch(pid, self._full_files(pid), no_resume=False)
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
            elif status == "empty":
                self.store.set_status(pid, "integrated", detail="changed nothing")
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

    def _full(self):
        self.integrated_since_full = 0
        self._emit("full-start", None, {})
        res = self._run_cmd(self.opts["full"], self.opts.get("full_timeout"))
        if res.get("dirtied"):
            self._emit("integration-dirty", None, {
                "files": res["dirtied"], "after": "full"})
        self._emit("full-end", None, {"status": res["status"], "seconds": res["seconds"]})
        self.store.event("full", detail="%s exit=%s\n%s" % (
            res["status"], res["exit"], res["excerpt"]))
        if res["status"] != "pass":
            self._stop("full verification red")
            return
        if self.opts.get("after_green"):
            ag = self._run_cmd(self.opts["after_green"], None)
            if ag.get("dirtied"):
                self._emit("integration-dirty", None, {
                    "files": ag["dirtied"], "after": "after-green"})
            # _run_verify_cmd only excerpts failures; that is what matters here.
            self._emit("after-green", None, {"exit": ag["exit"]})

    # -- the loop -------------------------------------------------------------

    def loop(self):
        while True:
            state = self.store.load()
            pkg_states = state["packages"]
            carried = [p["id"] for p in self.plan["packages"]
                       if pkg_states.get(p["id"], {}).get("status") in ("verifying", "ready")]
            selected = agent_exec_wave_plan.select(
                self.plan, pkg_states, self.opts["max_in_flight"])
            if not selected and not carried:
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
            self.store.event("wave-start", detail=",".join(carried + selected))

            head = self._int_head()
            jobs = [(pid, "verify", "") for pid in carried
                    if pkg_states[pid]["status"] == "verifying"]
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

            self._integrate()
            full_every = self.opts.get("full_every") or 1
            if (self.opts.get("full") and self.integrated_since_full
                    and self.waves % full_every == 0 and self.stop_reason is None):
                self._full()
            if self.stop_reason is not None:
                break

        if self.opts.get("full") and self.integrated_since_full:
            self._full()

    def report(self):
        state = self.store.load()
        pkg_states = state["packages"]
        order = [p["id"] for p in self.plan["packages"]]
        blocked_ids = agent_exec_wave_plan.blocked(self.plan, pkg_states)

        def with_status(status):
            return [pid for pid in order if pkg_states.get(pid, {}).get("status") == status]

        return {
            "status": "stopped" if self.stop_reason is not None else "done",
            "reason": self.stop_reason,
            "waves": self.waves,
            "integrated": with_status("integrated"),
            "needs": [{"id": n.get("id"), "kind": n.get("kind")}
                      for n in state.get("needs") or []],
            "pending": [pid for pid in with_status("pending") if pid not in blocked_ids],
            "blocked": [pid for pid in with_status("pending") if pid in blocked_ids],
        }

    def join_notifications(self):
        with self.lock:
            threads = list(self.notify_threads)
        for thread in threads:
            thread.join(_NOTIFY_TIMEOUT + 5)


def run_wave(opts, executor=None, clock=time.time):
    """Drive the plan to done or stopped; returns the end report dict.

    A report with status "error" (plus "reason") means the loop never
    started: bad plan, not a repository, no integration worktree.
    """
    merged = default_opts()
    merged.update(opts)
    runner = _Runner(merged, executor or CliExecutor(), clock)
    try:
        runner.init()
    except agent_exec_wave_plan.PlanError as exc:
        return {"status": "error", "reason": str(exc), "kind": "plan"}
    except RuntimeError as exc:
        return {"status": "error", "reason": str(exc), "kind": "environment"}

    previous_stdout = sys.stdout
    runner.stdout = _ThreadStdout(previous_stdout)
    sys.stdout = runner.stdout
    try:
        runner.loop()
    finally:
        sys.stdout = previous_stdout
    report = runner.report()
    if report["status"] == "done":
        runner._notify("done", dict(report, event="done", at=clock()))
    runner.join_notifications()
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
    "                  [--full-timeout SEC] [--after-green CMD] [--notify-cmd CMD]\n"
    "                  [--stop-at EPOCH] [--max-waves N] [--max-packages N]\n"
    "                  [--run-id ID] [--json|--text]\n"
)

_MARK_USAGE = (
    "usage: agent-exec wave mark --state STATE --pkg ID --status ready|pending|failed\n"
    "                  (ready enters self-verifying; pending|failed are terminal handoffs)\n"
    "                  [--detail TEXT]\n"
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
        "--after-green": ("after_green", str), "--notify-cmd": ("notify_cmd", str),
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
        if tok in ("--json", "--text"):
            seen.add(tok)
            opts["text"] = tok == "--text"
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
    report = run_wave(opts, executor=CliExecutor())
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
    value_flags = {"--state": "state", "--pkg": "pkg", "--status": "status", "--detail": "detail"}
    values = {}
    i = 0
    while i < len(args):
        tok = args[i]
        if tok not in value_flags:
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
    if not state_path or not pkg or not status:
        sys.stderr.write(_MARK_USAGE)
        return 2
    if status not in ("ready", "pending", "failed"):
        sys.stderr.write("agent-exec: wave mark: --status must be ready|pending|failed\n")
        return 2
    if not os.path.isfile(state_path):
        sys.stderr.write("agent-exec: wave mark: no state file at %s\n" % state_path)
        return 3
    store = agent_exec_wave.StateStore(state_path)
    if pkg not in store.load().get("packages", {}):
        sys.stderr.write("agent-exec: wave mark: unknown package: %s\n" % pkg)
        return 3
    stored_status = "verifying" if status == "ready" else status
    store.set_status(pkg, stored_status, detail=detail or "marked by the instructor")
    store.clear_need(pkg)
    print(json.dumps({"pkg": pkg, "status": stored_status}, ensure_ascii=False))
    return 0
