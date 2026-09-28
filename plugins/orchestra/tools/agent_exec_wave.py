# /// script
# requires-python = ">=3.9"
# ///
"""State store, events, and `agent-exec wave status|stop` for wave orchestration.

`agent-exec wave` coordinates a plan of independent packages dispatched over
one or more waves (see `agent-exec dispatch`/`isolate`/`integrate`). This
module owns the shared `state.json`/`events.jsonl` pair described in
shared-schema.md: a `StateStore` for every mutation, plus the read-only
`status`/`stop` sub-verbs. It is stdlib only and must NOT import agent_exec:
the runner that drives dispatch/isolate/integrate from a plan is a later
contract that imports this module, not the other way around.

state.json is safe for concurrent writers (threads within one process, and
separate processes) via an exclusive `fcntl.flock` on a sibling `.lock` file
held for the whole read-mutate-write-append cycle, with the write itself
landing through a temp-file-plus-`os.replace` so a reader never observes a
half-written file.
"""

import copy
import fcntl
import json
import os
import sys
import tempfile
import time

# --- schema ------------------------------------------------------------------

STATUSES = (
    "pending", "implementing", "verifying", "fixing", "ready",
    "integrated", "needs", "failed", "blocked",
)
_STATUS_SET = frozenset(STATUSES)
_STATUS_ORDER = {status: i for i, status in enumerate(STATUSES)}

IN_FLIGHT = frozenset({"implementing", "verifying", "fixing", "ready"})

NEED_KINDS = (
    "escalate", "conflict", "post-integration", "self-verify", "delegate", "dispatch-error",
)
_NEED_KIND_SET = frozenset(NEED_KINDS)

# Per-package keys as they appear in state.json.
_PACKAGE_FIELDS = (
    "status", "since", "attempts", "tree", "executor", "session",
    "files_changed", "commit", "detail",
)
# Fields `set_status` may additionally set via **fields (status/since/detail
# are handled through set_status's own named parameters).
_SET_STATUS_EXTRA_FIELDS = frozenset(_PACKAGE_FIELDS) - {"status", "since", "detail"}
# Fields `update` may set directly (everything except status/since, which
# only `set_status` is allowed to move).
_UPDATE_FIELDS = frozenset(_PACKAGE_FIELDS) - {"status", "since"}

_INTEGRATION_FIELDS = frozenset({"task", "path", "base"})


def _new_package(now):
    return {
        "status": "pending", "since": now, "attempts": 0, "tree": None,
        "executor": None, "session": False, "files_changed": None,
        "commit": None, "detail": "",
    }


def _events_path_for(state_path):
    directory = os.path.dirname(os.path.abspath(state_path))
    base = os.path.basename(state_path)
    if base.endswith(".json"):
        base = base[:-len(".json")]
    return os.path.join(directory, base + ".events.jsonl")


def _stop_path_for(state_path):
    return os.path.join(os.path.dirname(os.path.abspath(state_path)), "STOP")


# --- state store ---------------------------------------------------------


class StateStore(object):
    """All reads/writes of one wave's state.json + events.jsonl.

    `clock` is injectable for tests; defaults to `time.time`.
    """

    def __init__(self, path, clock=time.time):
        self.path = path
        self.clock = clock

    # -- plumbing --------------------------------------------------------

    def _events_path(self):
        return _events_path_for(self.path)

    def _read(self):
        with open(self.path, "r", encoding="utf-8") as fh:
            return json.load(fh)

    def _write(self, state):
        directory = os.path.dirname(os.path.abspath(self.path)) or "."
        os.makedirs(directory, exist_ok=True)
        fd, tmp_path = tempfile.mkstemp(prefix=".agent-exec-wave-", dir=directory)
        try:
            with os.fdopen(fd, "w", encoding="utf-8") as fh:
                json.dump(state, fh, ensure_ascii=False, indent=2, sort_keys=True)
            os.replace(tmp_path, self.path)
        except Exception:
            try:
                os.unlink(tmp_path)
            except OSError:
                pass
            raise

    def _append_event(self, record):
        with open(self._events_path(), "a", encoding="utf-8") as fh:
            fh.write(json.dumps(record, ensure_ascii=False) + "\n")

    def _mutate(self, fn):
        """Hold the lock for one read-mutate-write(-append) cycle.

        `fn(state, now)` mutates `state` in place and returns an event record
        to append, or None to append nothing. Returns a deep copy of the
        resulting state.
        """
        directory = os.path.dirname(os.path.abspath(self.path)) or "."
        os.makedirs(directory, exist_ok=True)
        lock_path = self.path + ".lock"
        handle = open(lock_path, "a+")
        try:
            fcntl.flock(handle.fileno(), fcntl.LOCK_EX)
            now = self.clock()
            state = self._read()
            event_record = fn(state, now)
            state["updated"] = now
            self._write(state)
            if event_record is not None:
                self._append_event(event_record)
            return copy.deepcopy(state)
        finally:
            fcntl.flock(handle.fileno(), fcntl.LOCK_UN)
            handle.close()

    # -- lifecycle ---------------------------------------------------------

    def init(self, plan_path, package_ids, integration_task):
        """Create state.json per schema, or add missing package ids to it.

        Existing packages and top-level fields are left untouched.
        """
        directory = os.path.dirname(os.path.abspath(self.path)) or "."
        os.makedirs(directory, exist_ok=True)
        lock_path = self.path + ".lock"
        handle = open(lock_path, "a+")
        try:
            fcntl.flock(handle.fileno(), fcntl.LOCK_EX)
            now = self.clock()
            if os.path.exists(self.path):
                state = self._read()
                packages = state.setdefault("packages", {})
                for pid in package_ids:
                    if pid not in packages:
                        packages[pid] = _new_package(now)
            else:
                state = {
                    "version": 1,
                    "plan": plan_path,
                    "integration": {"task": integration_task, "path": None, "base": None},
                    "wave": 0,
                    "packages": {pid: _new_package(now) for pid in package_ids},
                    "needs": [],
                    "stopped": None,
                    "updated": now,
                }
            state["updated"] = now
            self._write(state)
            return copy.deepcopy(state)
        finally:
            fcntl.flock(handle.fileno(), fcntl.LOCK_UN)
            handle.close()

    def load(self):
        return copy.deepcopy(self._read())

    # -- mutations -----------------------------------------------------------

    def set_status(self, pkg, status, detail="", **fields):
        if status not in _STATUS_SET:
            raise ValueError("unknown status: %r" % (status,))
        unknown = set(fields) - _SET_STATUS_EXTRA_FIELDS
        if unknown:
            raise ValueError("unknown package field(s): %s" % ", ".join(sorted(unknown)))

        def fn(state, now):
            packages = state.setdefault("packages", {})
            entry = packages.setdefault(pkg, _new_package(now))
            old_status = entry.get("status")
            if old_status != status:
                entry["since"] = now
            entry["status"] = status
            entry["detail"] = detail
            for key, value in fields.items():
                entry[key] = value
            return {"at": now, "pkg": pkg, "from": old_status, "to": status,
                    "event": "status", "detail": detail}

        return self._mutate(fn)

    def update(self, pkg, **fields):
        unknown = set(fields) - _UPDATE_FIELDS
        if unknown:
            raise ValueError("unknown package field(s): %s" % ", ".join(sorted(unknown)))

        def fn(state, now):
            packages = state.setdefault("packages", {})
            entry = packages.setdefault(pkg, _new_package(now))
            for key, value in fields.items():
                entry[key] = value
            return None

        return self._mutate(fn)

    def add_need(self, pkg, kind, detail=""):
        if kind not in _NEED_KIND_SET:
            raise ValueError("unknown need kind: %r" % (kind,))

        def fn(state, now):
            needs = state.setdefault("needs", [])
            needs.append({"id": pkg, "kind": kind, "detail": detail, "at": now})
            packages = state.setdefault("packages", {})
            entry = packages.setdefault(pkg, _new_package(now))
            old_status = entry.get("status")
            if old_status != "needs":
                entry["since"] = now
            entry["status"] = "needs"
            entry["detail"] = detail
            return {"at": now, "pkg": pkg, "from": old_status, "to": "needs",
                    "event": "status", "detail": detail}

        return self._mutate(fn)

    def clear_need(self, pkg):
        def fn(state, now):
            needs = state.get("needs") or []
            state["needs"] = [n for n in needs if n.get("id") != pkg]
            return None

        return self._mutate(fn)

    def event(self, name, pkg=None, detail=""):
        def fn(state, now):
            return {"at": now, "pkg": pkg, "from": None, "to": None,
                    "event": name, "detail": detail}

        return self._mutate(fn)

    def set_integration(self, **fields):
        unknown = set(fields) - _INTEGRATION_FIELDS
        if unknown:
            raise ValueError("unknown integration field(s): %s" % ", ".join(sorted(unknown)))

        def fn(state, now):
            integration = state.setdefault("integration", {"task": None, "path": None, "base": None})
            integration.update(fields)
            return None

        return self._mutate(fn)

    def set_wave(self, n):
        def fn(state, now):
            state["wave"] = n
            return None

        return self._mutate(fn)

    def mark_stopped(self, reason=""):
        def fn(state, now):
            state["stopped"] = {"reason": reason, "at": now}
            return {"at": now, "pkg": None, "from": None, "to": None,
                    "event": "stop", "detail": reason}

        return self._mutate(fn)


# --- module-level helpers (stop flag, events) ------------------------------


def stop_requested(state_path):
    return os.path.isfile(_stop_path_for(state_path))


def request_stop(state_path, reason=""):
    path = _stop_path_for(state_path)
    os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
    with open(path, "w", encoding="utf-8") as fh:
        fh.write(reason)


def read_events(state_path, limit):
    """Last `limit` event dicts, tolerating a truncated last line."""
    path = _events_path_for(state_path)
    if not os.path.isfile(path):
        return []
    with open(path, "r", encoding="utf-8") as fh:
        lines = fh.readlines()
    records = []
    for line in lines:
        line = line.strip()
        if not line:
            continue
        try:
            records.append(json.loads(line))
        except ValueError:
            continue  # a concurrent writer's last line may still be mid-flush
    if limit is not None and limit >= 0:
        records = records[-limit:]
    return records


# --- wave registry -------------------------------------------------------


def registry_path():
    """`$ORCHESTRA_WAVE_REGISTRY`, else `~/.claude/orchestra/waves.jsonl`."""
    override = os.environ.get("ORCHESTRA_WAVE_REGISTRY")
    if override:
        return override
    return os.path.join(os.path.expanduser("~"), ".claude", "orchestra", "waves.jsonl")


def register_wave(state, plan, repo, into, clock=time.time):
    """Append one registry line for a starting `wave run`; returns the record."""
    record = {
        "state": os.path.abspath(state), "plan": os.path.abspath(plan),
        "repo": os.path.abspath(repo), "into": into, "registered_at": clock(),
    }
    path = registry_path()
    os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)
    with open(path, "a", encoding="utf-8") as fh:
        fh.write(json.dumps(record, ensure_ascii=False, sort_keys=True) + "\n")
    return record


def list_waves():
    """Registered waves, most recent registration first.

    Deduped by `state` (last line wins), entries whose state file is gone are
    dropped, and a truncated or malformed line is skipped.
    """
    path = registry_path()
    if not os.path.isfile(path):
        return []
    with open(path, "r", encoding="utf-8") as fh:
        lines = fh.readlines()
    latest = {}
    for line in lines:
        line = line.strip()
        if not line:
            continue
        try:
            record = json.loads(line)
        except ValueError:
            continue
        if not isinstance(record, dict) or not record.get("state"):
            continue
        latest.pop(record["state"], None)
        latest[record["state"]] = record
    records = [r for r in latest.values() if os.path.isfile(r["state"])]
    records.reverse()
    return records


# --- rendering --------------------------------------------------------------


def _format_elapsed(seconds):
    seconds = max(0, int(seconds))
    if seconds < 3600:
        minutes, secs = divmod(seconds, 60)
        return "%dm%02ds" % (minutes, secs)
    hours, rest = divmod(seconds, 3600)
    minutes = rest // 60
    return "%dh%02dm" % (hours, minutes)


def _format_event_line(event_record):
    pkg = event_record.get("pkg") or "-"
    parts = [pkg, event_record.get("event", "")]
    frm, to = event_record.get("from"), event_record.get("to")
    if frm or to:
        parts.append("%s->%s" % (frm, to))
    detail = event_record.get("detail")
    if detail:
        parts.append(detail.splitlines()[0])
    return "  ".join(p for p in parts if p)


def render_status(state, now, plan_titles=None, events=None):
    """Human-readable status text: header, per-package rows, needs, recent events."""
    lines = []

    counts = {}
    for pkg in state.get("packages", {}).values():
        status = pkg.get("status")
        counts[status] = counts.get(status, 0) + 1
    count_str = " ".join("%s=%d" % (s, counts[s]) for s in STATUSES if counts.get(s))

    header = "wave %s" % state.get("wave", 0)
    if count_str:
        header += "  " + count_str
    stopped = state.get("stopped")
    if stopped:
        header += "  stopped: %s" % stopped.get("reason", "")
    lines.append(header)

    items = [
        (pid, pkg) for pid, pkg in state.get("packages", {}).items()
        if pkg.get("status") != "pending"
    ]
    items.sort(key=lambda kv: (
        _STATUS_ORDER.get(kv[1].get("status"), len(STATUSES)), kv[0],
    ))
    for pid, pkg in items:
        elapsed = _format_elapsed(now - (pkg.get("since") or now))
        row = "  %s  %s  %s  %s  %s  %s" % (
            pid, pkg.get("status"), elapsed,
            pkg.get("executor") or "-",
            pkg.get("files_changed") if pkg.get("files_changed") is not None else "-",
            pkg.get("attempts", 0),
        )
        if plan_titles and pid in plan_titles:
            row += "  " + plan_titles[pid]
        lines.append(row)

    needs = state.get("needs") or []
    if needs:
        lines.append("needs:")
        for need in needs:
            detail = need.get("detail") or ""
            first_line = detail.splitlines()[0] if detail else ""
            lines.append("  %s  %s  %s" % (need.get("id"), need.get("kind"), first_line))

    if events:
        lines.append("recent:")
        for event_record in events[-5:]:
            lines.append("  " + _format_event_line(event_record))

    return "\n".join(lines)


def status_line(state, now):
    """One line of compact JSON: wave/counts(non-zero)/needs/stopped/updated_ago."""
    counts = {}
    for pkg in state.get("packages", {}).values():
        status = pkg.get("status")
        counts[status] = counts.get(status, 0) + 1
    counts = dict((s, n) for s, n in counts.items() if n)
    payload = {
        "wave": state.get("wave", 0),
        "counts": counts,
        "needs": len(state.get("needs") or []),
        "stopped": bool(state.get("stopped")),
        "updated_ago": int(now - state.get("updated", now)),
    }
    return json.dumps(payload, sort_keys=True, ensure_ascii=False)


# --- CLI ---------------------------------------------------------------------


def _cmd_status(args):
    state_path = None
    fmt = "text"
    watch = None
    events_limit = None
    i = 0
    while i < len(args):
        tok = args[i]
        if tok == "--state":
            if i + 1 >= len(args):
                sys.stderr.write("agent-exec: wave: missing value for --state\n")
                return 2
            state_path = args[i + 1]
            i += 2
        elif tok in ("--json", "--line", "--text"):
            fmt = tok[2:]
            i += 1
        elif tok == "--watch":
            if i + 1 >= len(args):
                sys.stderr.write("agent-exec: wave: missing value for --watch\n")
                return 2
            try:
                watch = float(args[i + 1])
            except ValueError:
                sys.stderr.write("agent-exec: wave: bad --watch value: %s\n" % args[i + 1])
                return 2
            i += 2
        elif tok == "--events":
            if i + 1 >= len(args):
                sys.stderr.write("agent-exec: wave: missing value for --events\n")
                return 2
            try:
                events_limit = int(args[i + 1])
            except ValueError:
                sys.stderr.write("agent-exec: wave: bad --events value: %s\n" % args[i + 1])
                return 2
            i += 2
        else:
            sys.stderr.write("agent-exec: wave: unknown argument: %s\n" % tok)
            return 2

    if state_path is None:
        sys.stderr.write("agent-exec: wave: status: --state is required\n")
        return 2

    def render_once():
        if not os.path.isfile(state_path):
            sys.stderr.write("agent-exec: wave: no state file at %s\n" % state_path)
            return None
        state = StateStore(state_path).load()
        now = time.time()
        if fmt == "json":
            return json.dumps(state, ensure_ascii=False)
        if fmt == "line":
            return status_line(state, now)
        events = read_events(state_path, events_limit) if events_limit else None
        return render_status(state, now, events=events)

    if watch is None:
        output = render_once()
        if output is None:
            return 3
        print(output)
        return 0

    try:
        while True:
            output = render_once()
            if output is None:
                return 3
            if fmt == "text":
                sys.stdout.write("\x1b[H\x1b[2J")
            print(output)
            sys.stdout.flush()
            time.sleep(watch)
    except KeyboardInterrupt:
        return 0


def _cmd_stop(args):
    state_path = None
    reason = ""
    i = 0
    while i < len(args):
        tok = args[i]
        if tok == "--state":
            if i + 1 >= len(args):
                sys.stderr.write("agent-exec: wave: missing value for --state\n")
                return 2
            state_path = args[i + 1]
            i += 2
        elif tok == "--reason":
            if i + 1 >= len(args):
                sys.stderr.write("agent-exec: wave: missing value for --reason\n")
                return 2
            reason = args[i + 1]
            i += 2
        else:
            sys.stderr.write("agent-exec: wave: unknown argument: %s\n" % tok)
            return 2

    if state_path is None:
        sys.stderr.write("agent-exec: wave: stop: --state is required\n")
        return 2

    request_stop(state_path, reason)
    print(json.dumps({"stop_requested": True}))
    return 0


def _cmd_run(args):
    # Imported here, not at module top: agent_exec_wave_run imports agent_exec,
    # and `wave status`/`stop` must keep working without it.
    import agent_exec_wave_run
    return agent_exec_wave_run.cmd_wave_run(args)


def _cmd_mark(args):
    import agent_exec_wave_run
    return agent_exec_wave_run.cmd_wave_mark(args)


# Sub-verb dispatch table. `run`/`mark` live in agent_exec_wave_run.py.
_SUBCOMMANDS = {
    "status": _cmd_status,
    "stop": _cmd_stop,
    "run": _cmd_run,
    "mark": _cmd_mark,
}

_USAGE = (
    "usage: agent-exec wave status --state PATH [--json|--line|--text]\n"
    "                        [--watch SEC] [--events N]\n"
    "       agent-exec wave stop --state PATH [--reason TEXT]\n"
)


def cmd_wave(args):
    if not args or args[0] not in _SUBCOMMANDS:
        sys.stderr.write(_USAGE)
        return 2
    return _SUBCOMMANDS[args[0]](args[1:])
