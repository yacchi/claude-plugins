"""Timer-driven safeguards for external executor subprocesses."""

import json
import os
import signal
import threading
import time


DEFAULT_WALL_SECONDS = {
    "light": 1200,
    "standard": 2400,
    "deep": 5400,
    "independent-review": 2400,
}


def _canonical(value):
    try:
        return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False)
    except (TypeError, ValueError):
        return repr(value)


def _preview(value):
    text = _canonical(value)
    return text[:200]


class Watchdog:
    """Observe JSONL records and terminate a process group on runaway work."""

    def __init__(self, proc, config, executor, cls="standard", clock=None):
        self.proc = proc
        self.config = config or {}
        self.executor = executor
        self.cls = cls if cls in DEFAULT_WALL_SECONDS else "standard"
        self.clock = clock or time.monotonic
        self.started = self.clock()
        self.last_record = self.started
        # In-flight tool calls keyed by call id (pi `toolCallId`, codex item
        # id): executors may run several concurrently, and the tool-idle
        # budget applies while any of them is still open.
        self._active_tools = set()
        self._tool_sequence = 0
        self.last_tool = None
        self.repeat_count = 0
        self.reason = None
        self._fired_at = None
        self._window_start = None
        self._lock = threading.Lock()
        self._stopped = threading.Event()
        self._timer = None

    @property
    def enabled(self):
        return self.config.get("enabled", True) is True

    def start(self):
        if not self.enabled:
            return
        self._timer = threading.Thread(target=self._watch, name="agent-exec-watchdog", daemon=True)
        self._timer.start()

    def stop(self):
        self._stopped.set()
        timer = self._timer
        if timer is not None and timer is not threading.current_thread():
            timer.join(timeout=1)

    def on_line(self, line):
        with self._lock:
            self.last_record = self.clock()
            try:
                event = json.loads(line)
            except (TypeError, ValueError):
                return
            if not isinstance(event, dict):
                return
            if self.executor == "pi":
                self._pi_event(event)
            else:
                self._codex_event(event)

    def _start_tool(self, name, args, call_id=None):
        key = (name, _canonical(args))
        if key == self.last_tool:
            self.repeat_count += 1
        else:
            self.last_tool = key
            self.repeat_count = 1
        if call_id is None:
            self._tool_sequence += 1
            tool_id = "anonymous-%d" % self._tool_sequence
        else:
            tool_id = call_id
        self._active_tools.add(tool_id)
        self._last_tool_info = {"name": str(name), "args_preview": _preview(args)}
        limit = self.config.get("repeat_limit", 3)
        if isinstance(limit, int) and not isinstance(limit, bool) and self.repeat_count >= limit:
            self._fire("repeat", self.clock(), self.started)

    def _end_tool(self, call_id=None):
        """Close one in-flight call. An end without an id, or whose id was
        never seen while id-less starts are open, closes what it can match
        so a malformed pairing cannot pin the tool-idle budget forever."""
        if call_id is None:
            self._active_tools.clear()
        elif call_id in self._active_tools:
            self._active_tools.discard(call_id)
        else:
            anonymous = sorted(t for t in self._active_tools
                               if isinstance(t, str) and t.startswith("anonymous-"))
            if anonymous:
                self._active_tools.discard(anonymous[0])

    @property
    def tool_running(self):
        return bool(self._active_tools)

    def _pi_event(self, event):
        etype = event.get("type")
        if etype == "tool_execution_start":
            self._start_tool(event.get("toolName", ""), event.get("args", {}), event.get("toolCallId"))
        elif etype == "tool_execution_end":
            self._end_tool(event.get("toolCallId"))
        elif etype in ("turn_end", "agent_end"):
            # Every tool of a turn has returned before the turn ends.
            self._active_tools.clear()

    def _codex_event(self, event):
        if event.get("type") in ("turn.completed", "turn.failed"):
            self._active_tools.clear()
            return
        if event.get("type") not in ("item.started", "item.completed"):
            return
        item = event.get("item")
        if not isinstance(item, dict) or item.get("type") not in ("command_execution", "command_execution_output"):
            return
        item_id = item.get("id")
        if event.get("type") == "item.started":
            self._start_tool(item.get("command", "command_execution"), item.get("command", ""), item_id)
        else:
            self._end_tool(item_id)

    # Upper bound on one wait: a config that changes nothing still gets
    # re-evaluated twice a second, and no deadline is missed by more.
    _MAX_WAIT = 0.5

    def _fire(self, reason, now, window_start):
        """Record the trip under the lock: when it fired and where the
        budget it exceeded began, so the report never includes the time
        spent terminating the process group afterwards."""
        if self.reason is None:
            self.reason = reason
            self._fired_at = now
            self._window_start = window_start

    def _check(self, now):
        """Evaluate every budget at `now` (lock held). Fires at most one
        reason; returns the seconds until the nearest deadline, or None
        once fired."""
        if self.reason is not None:
            return None
        wall = self.config.get("wall_seconds", DEFAULT_WALL_SECONDS)
        limit = wall.get(self.cls, wall.get("standard", 2400)) if isinstance(wall, dict) else 2400
        if now - self.started >= limit:
            self._fire("wall", now, self.started)
            return None
        if self.repeat_count >= self.config.get("repeat_limit", 3):
            self._fire("repeat", now, self.started)
            return None
        running = bool(self._active_tools)
        idle_key = "tool_idle_seconds" if running else "idle_seconds"
        idle_limit = self.config.get(idle_key, 900 if running else 600)
        if now - self.last_record >= idle_limit:
            self._fire("tool-idle" if running else "idle", now, self.last_record)
            return None
        return min(self.started + limit, self.last_record + idle_limit) - now

    def _watch(self):
        """Deadline-based: sleep until the nearest budget expires (capped at
        _MAX_WAIT, since a new record can move the idle deadline), never
        spin."""
        while True:
            with self._lock:
                remaining = self._check(self.clock())
            if remaining is None:
                self._terminate_group()
                return
            if self._stopped.wait(min(self._MAX_WAIT, max(0.01, remaining))):
                return

    def _terminate_group(self):
        try:
            os.killpg(os.getpgid(self.proc.pid), signal.SIGTERM)
        except OSError:
            return
        try:
            self.proc.wait(timeout=5)
        except Exception:
            try:
                os.killpg(os.getpgid(self.proc.pid), signal.SIGKILL)
            except OSError:
                pass

    def result(self):
        with self._lock:
            if self.reason is None:
                return None
            fired = self._fired_at if self._fired_at is not None else self.clock()
            window = self._window_start if self._window_start is not None else self.started
            return {
                "reason": self.reason,
                # Time spent in the budget that tripped (idle: since the last
                # record; wall/repeat: since start), measured at the trip.
                "elapsed_s": round(max(0.0, fired - window), 2),
                "runtime_s": round(max(0.0, fired - self.started), 2),
                "last_tool": getattr(self, "_last_tool_info", None),
                "repeat_count": self.repeat_count,
            }
