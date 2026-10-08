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
        self.tool_started = None
        self._active_tools = set()
        self.last_tool = None
        self.repeat_count = 0
        self.reason = None
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
        tool_id = call_id if call_id is not None else True
        self._active_tools.add(tool_id)
        self.tool_started = tool_id
        self._last_tool_info = {"name": str(name), "args_preview": _preview(args)}
        limit = self.config.get("repeat_limit", 3)
        if isinstance(limit, int) and not isinstance(limit, bool) and self.repeat_count >= limit:
            self.reason = "repeat"

    def _end_tool(self, call_id=None):
        if call_id is None:
            self._active_tools.clear()
        else:
            self._active_tools.discard(call_id)
        self.tool_started = next(iter(self._active_tools), None)

    def _pi_event(self, event):
        etype = event.get("type")
        if etype == "tool_execution_start":
            self._start_tool(event.get("toolName", ""), event.get("args", {}), event.get("toolCallId"))
        elif etype == "tool_execution_end":
            self._end_tool(event.get("toolCallId"))

    def _codex_event(self, event):
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

    def _watch(self):
        while not self._stopped.wait(0.05):
            now = self.clock()
            with self._lock:
                if self.reason is None:
                    wall = self.config.get("wall_seconds", DEFAULT_WALL_SECONDS)
                    limit = wall.get(self.cls, wall.get("standard", 2400)) if isinstance(wall, dict) else 2400
                    if now - self.started >= limit:
                        self.reason = "wall"
                    elif self.repeat_count >= self.config.get("repeat_limit", 3):
                        self.reason = "repeat"
                    else:
                        idle_key = "tool_idle_seconds" if self.tool_started is not None else "idle_seconds"
                        idle_limit = self.config.get(idle_key, 900 if self.tool_started is not None else 600)
                        if now - self.last_record >= idle_limit:
                            self.reason = "tool-idle" if self.tool_started is not None else "idle"
                reason = self.reason
            if reason is not None:
                self._terminate_group()
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
            return {
                "reason": self.reason,
                "elapsed_s": max(0, int(self.clock() - self.started)),
                "last_tool": getattr(self, "_last_tool_info", None),
                "repeat_count": self.repeat_count,
            }
