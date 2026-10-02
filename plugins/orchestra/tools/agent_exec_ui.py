"""agent-exec ui: a zero-token local dashboard for orchestra (contract U2).

`agent-exec ui` starts (or reuses) a detached HTTP server bound to 127.0.0.1
that renders every registered `agent-exec wave` run, the worktrees, recent
dispatches and cooldowns from files already on disk. Nothing here calls a
model; the page is one self-contained HTML document fed by Server-Sent Events.

Security: every request needs a random token (query `t` or the
`X-Orchestra-Token` header) and a loopback Host header. The one mutating
route (POST /api/stop) needs the token in the header, never the query.
"""

import atexit
import hashlib
import hmac
import http.server
import json
import os
import secrets
import signal
import subprocess
import sys
import threading
import time
import urllib.parse
import urllib.request

_HERE = os.path.dirname(os.path.abspath(__file__))
if _HERE not in sys.path:
    sys.path.insert(0, _HERE)

EVENT_LIMIT = 80
DISPATCH_LIMIT = 50
SSE_POLL_SECONDS = 2.0
SSE_PING_SECONDS = 15.0
IDLE_MINUTES_DEFAULT = 30.0
START_WAIT_SECONDS = 10.0
_LIVE_CACHE = {}
_USAGE_CACHE = (0.0, None)

UI_USAGE = """\
Usage:
  agent-exec ui [--open] [--stop] [--idle-minutes N] [--json]
"""


# --- paths ------------------------------------------------------------------


def _orchestra_home():
    return os.path.join(os.path.expanduser("~"), ".claude", "orchestra")


def info_path():
    override = os.environ.get("ORCHESTRA_UI_STATE")
    if override:
        return os.path.abspath(os.path.expanduser(override))
    return os.path.join(_orchestra_home(), "ui.json")


def log_path():
    if os.environ.get("ORCHESTRA_UI_STATE"):
        return os.path.join(os.path.dirname(info_path()), "ui.log")
    return os.path.join(_orchestra_home(), "ui.log")


def registry_path():
    override = os.environ.get("ORCHESTRA_WAVE_REGISTRY")
    if override:
        return os.path.abspath(os.path.expanduser(override))
    return os.path.join(_orchestra_home(), "waves.jsonl")


# --- registry + snapshot ------------------------------------------------------


def read_registry():
    """Registered waves, deduped by state path (last line wins).

    Entries whose state file no longer exists are skipped; a truncated or
    non-object line is ignored.
    """
    path = registry_path()
    try:
        with open(path, "r", encoding="utf-8", errors="replace") as fh:
            lines = fh.readlines()
    except OSError:
        return []
    by_state = {}
    order = []
    for line in lines:
        line = line.strip()
        if not line:
            continue
        try:
            entry = json.loads(line)
        except ValueError:
            continue
        if not isinstance(entry, dict) or not isinstance(entry.get("state"), str):
            continue
        state = entry["state"]
        if state in by_state:
            order.remove(state)
        by_state[state] = entry
        order.append(state)
    return [by_state[s] for s in order if os.path.isfile(s)]


def _load_json_file(path):
    with open(path, "r", encoding="utf-8") as fh:
        value = json.load(fh)
    return value if isinstance(value, dict) else {}


def _parse_event(record):
    if isinstance(record, dict) and isinstance(record.get("detail"), str):
        try:
            parsed = json.loads(record["detail"])
        except ValueError:
            return record
        if isinstance(parsed, (dict, list)):
            record = dict(record)
            record["detail"] = parsed
    return record


def _events_for(state_path):
    import agent_exec_wave
    return [_parse_event(r) for r in agent_exec_wave.read_events(state_path, EVENT_LIMIT)]


def _wave_snapshot(entry):
    state_path = entry["state"]
    out = {
        "state": state_path,
        "repo": entry.get("repo"),
        "into": entry.get("into"),
        "wave": None,
        "counts": {},
        "stopped": None,
        "packages": [],
        "needs": [],
        "events": [],
    }
    try:
        state = _load_json_file(state_path)
        out["wave"] = state.get("wave")
        out["stopped"] = state.get("stopped")
        out["needs"] = state.get("needs") if isinstance(state.get("needs"), list) else []
        counts = {}
        packages = []
        raw = state.get("packages")
        for pid, pkg in (raw.items() if isinstance(raw, dict) else []):
            pkg = pkg if isinstance(pkg, dict) else {}
            status = pkg.get("status")
            counts[str(status)] = counts.get(str(status), 0) + 1
            packages.append({
                "id": pid,
                "status": status,
                "since": pkg.get("since"),
                "executor": pkg.get("executor"),
                "files_changed": pkg.get("files_changed"),
                "attempts": pkg.get("attempts"),
                "detail": pkg.get("detail"),
            })
        import agent_exec_wave
        live = {}
        for pid, pkg in (state.get("packages") or {}).items():
            tree = pkg.get("tree") if isinstance(pkg, dict) else None
            if not isinstance(tree, str):
                continue
            key = os.path.abspath(tree)
            cached = _LIVE_CACHE.get(key)
            if cached and time.time() - cached[0] < 5:
                count = cached[1]
            else:
                count = agent_exec_wave.live_changes({
                    "packages": {pid: pkg},
                }).get(pid)
                _LIVE_CACHE[key] = (time.time(), count)
            live[pid] = count
        plan = state.get("plan")
        plan_dir = os.path.dirname(os.path.abspath(plan)) if isinstance(plan, str) else None
        plan_data = {}
        if isinstance(plan, str):
            try:
                with open(plan, "r", encoding="utf-8") as fh:
                    plan_data = json.load(fh)
            except (OSError, ValueError):
                pass
        specs = plan_data.get("packages", []) if isinstance(plan_data, dict) else []
        specs = {item.get("id"): item for item in specs if isinstance(item, dict)}
        context_dir = os.path.join(os.path.dirname(state_path), "context")
        correction_dir = os.path.join(os.path.dirname(state_path), "corrections")
        for package in packages:
            pid = package["id"]
            package["files_live"] = live.get(pid)
            spec = specs.get(pid)
            if isinstance(spec, dict):
                spec = spec.get("spec") or spec.get("path")
            if isinstance(spec, str) and plan_dir:
                package["spec"] = os.path.abspath(os.path.join(plan_dir, spec))
            else:
                package["spec"] = None
            package["context"] = os.path.join(context_dir, "%s.md" % pid) if os.path.isfile(
                os.path.join(context_dir, "%s.md" % pid)) else None
            package["correction"] = os.path.join(correction_dir, "%s.md" % pid) if os.path.isfile(
                os.path.join(correction_dir, "%s.md" % pid)) else None
        out["counts"] = counts
        out["packages"] = packages
    except (OSError, ValueError) as exc:
        out["error"] = str(exc)
    try:
        out["events"] = _events_for(state_path)
    except Exception as exc:  # noqa: BLE001 - a broken events file must not hide the wave
        out["events_error"] = str(exc)
    return out


def _worktrees_snapshot(waves):
    import agent_exec
    seen = []
    for entry in waves:
        repo = entry.get("repo")
        if isinstance(repo, str) and repo not in seen:
            seen.append(repo)
    out = []
    for repo in seen:
        try:
            out.append({"repo": repo, "items": agent_exec.isolate_list(repo)})
        except Exception as exc:  # noqa: BLE001
            out.append({"repo": repo, "items": [], "error": str(exc)})
    return out


_DISPATCH_KEYS = ("executor", "model", "cls", "class", "status", "corr")
_DISPATCH_HINTS = ("time", "duration", "seconds", "_at", "ts", "date")


def _keep_dispatch_key(key):
    if key in _DISPATCH_KEYS:
        return True
    lowered = key.lower()
    return any(hint in lowered for hint in _DISPATCH_HINTS)


def _resolved_config():
    import agent_exec
    cfg, err = agent_exec.resolve_config()
    if err:
        raise RuntimeError(err)
    return cfg


def _dispatch_snapshot():
    import agent_exec
    cfg = _resolved_config()
    records = []
    for path in agent_exec._ledger_paths(agent_exec._ledger_dir_from_cfg(cfg)):
        records.extend(agent_exec._read_ledger_file(path))
    records = records[-DISPATCH_LIMIT:]
    records.reverse()
    return [dict((k, v) for k, v in r.items() if _keep_dispatch_key(k)) for r in records]


def _cooldown_snapshot():
    import agent_exec
    cfg = _resolved_config()
    return agent_exec.load_cooldown_state(agent_exec.cooldown_state_path(cfg))


def _guarded(fn, *args):
    try:
        return fn(*args)
    except Exception as exc:  # noqa: BLE001 - the snapshot never 500s
        return {"error": str(exc) or exc.__class__.__name__}


def build_snapshot():
    entries = _guarded(read_registry)
    if isinstance(entries, dict):
        waves, worktrees = entries, entries
    else:
        waves = [_guarded(_wave_snapshot, e) for e in entries]
        worktrees = _guarded(_worktrees_snapshot, entries)
    running = _running_snapshot(entries)
    usage = _usage_snapshot()
    return {
        "generated_at": time.time(),
        "waves": waves,
        "worktrees": worktrees,
        "dispatch": _guarded(_dispatch_snapshot),
        "cooldown": _guarded(_cooldown_snapshot),
        "running": running,
        "usage": usage,
    }


def _running_snapshot(entries):
    import agent_exec
    try:
        cfg = _resolved_config()
        dispatches = agent_exec.list_detached_dispatches(cfg)
    except Exception as exc:  # noqa: BLE001
        dispatches = {"error": str(exc) or exc.__class__.__name__}
    if not isinstance(dispatches, list):
        dispatches = []
    alive = [item for item in dispatches if item.get("alive")]
    alive.sort(key=lambda item: item.get("started") or 0, reverse=True)
    sessions = []
    in_flight = 0
    by_executor = {}
    for entry in entries:
        try:
            state = _load_json_file(entry["state"])
            in_flight += sum(1 for pkg in (state.get("packages") or {}).values()
                             if pkg.get("status") in ("implementing", "verifying", "fixing", "ready"))
            path = os.path.join(os.path.dirname(entry["state"]), "orca-sessions.json")
            with open(path, "r", encoding="utf-8") as fh:
                records = json.load(fh)
            if isinstance(records, dict):
                records = list(records.values())
            for record in records if isinstance(records, list) else []:
                if isinstance(record, dict):
                    sessions.append({k: record.get(k) for k in
                                     ("wave_state", "pkg", "terminal", "worktree")})
                    sessions[-1]["wave_state"] = sessions[-1]["wave_state"] or entry["state"]
        except (OSError, ValueError, TypeError):
            pass
    for item in alive:
        name = item.get("executor")
        if name:
            by_executor[name] = by_executor.get(name, 0) + 1
    return {"dispatches": alive, "orca_sessions": sessions,
            "by_executor": by_executor, "waves_in_flight": in_flight}


def _usage_snapshot():
    global _USAGE_CACHE
    if time.time() - _USAGE_CACHE[0] < 60 and _USAGE_CACHE[1] is not None:
        return _USAGE_CACHE[1]
    try:
        import agent_exec
        now = agent_exec.datetime.now(agent_exec.timezone.utc)
        report = agent_exec.build_usage_report(
            now - agent_exec.timedelta(hours=24), now,
            list(agent_exec._USAGE_SOURCES), all_projects=True,
            cfg=_resolved_config(),
        )
        totals = {}
        for executor, value in report.items():
            if not isinstance(value, dict):
                continue
            by_model = value.get("by_model") or {"unknown": value.get("tokens") or {}}
            for model, tokens in by_model.items():
                key = "%s/%s" % (executor, model)
                totals[key] = dict(tokens)
                if "cost_micro_usd" in value:
                    totals[key]["cost_micro_usd"] = value["cost_micro_usd"]
        _USAGE_CACHE = (time.time(), totals)
        return totals
    except Exception as exc:  # noqa: BLE001
        return {"error": str(exc) or exc.__class__.__name__}


def _snapshot_body(snapshot):
    return json.dumps(snapshot, ensure_ascii=False, sort_keys=True, default=str)


def _snapshot_hash(snapshot):
    """Hash of the snapshot minus its timestamp so an idle system sends nothing."""
    stable = dict((k, v) for k, v in snapshot.items() if k != "generated_at")
    return hashlib.sha256(_snapshot_body(stable).encode("utf-8")).hexdigest()


def _allowed_file(path):
    candidate = os.path.abspath(os.path.expanduser(path))
    real = os.path.realpath(candidate)
    if candidate != path or not os.path.isfile(candidate):
        return None
    for entry in read_registry():
        try:
            state = _load_json_file(entry["state"])
            plan = state.get("plan")
            if not isinstance(plan, str):
                continue
            with open(plan, "r", encoding="utf-8") as fh:
                data = json.load(fh)
            allowed = []
            if isinstance(data, dict):
                preambles = data.get("preamble", [])
                if isinstance(preambles, str):
                    preambles = [preambles]
                for value in preambles:
                    if isinstance(value, str):
                        allowed.append(os.path.realpath(os.path.abspath(
                            os.path.join(os.path.dirname(plan), value))))
                for key in ("spec", "preamble"):
                    value = data.get(key)
                    if isinstance(value, str) and key != "preamble":
                        allowed.append(os.path.realpath(os.path.abspath(
                            os.path.join(os.path.dirname(plan), value))))
                for package in data.get("packages", []):
                    if isinstance(package, dict) and isinstance(package.get("spec"), str):
                        allowed.append(os.path.realpath(os.path.abspath(
                            os.path.join(os.path.dirname(plan), package["spec"]))))
            state_dir = os.path.dirname(os.path.abspath(entry["state"]))
            for dirname in ("context", "corrections", "carry"):
                root = os.path.realpath(os.path.join(state_dir, dirname))
                if os.path.dirname(real) == root:
                    allowed.append(real)
            if real in allowed:
                return candidate
        except (OSError, ValueError, TypeError):
            continue
    return None


# --- HTTP server ----------------------------------------------------------------


class _Server(http.server.ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True

    def __init__(self, address, handler, token, idle_seconds):
        http.server.ThreadingHTTPServer.__init__(self, address, handler)
        self.token = token
        self.idle_seconds = idle_seconds
        self.last_activity = time.time()
        self.open_streams = 0
        self.lock = threading.Lock()

    def touch(self):
        self.last_activity = time.time()

    def stream_delta(self, delta):
        with self.lock:
            self.open_streams += delta
        self.touch()

    def idle_expired(self):
        with self.lock:
            if self.open_streams > 0:
                return False
        return time.time() - self.last_activity >= self.idle_seconds


class _Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "orchestra-ui"

    def log_message(self, fmt, *args):  # keep the log file quiet
        return

    # -- helpers
    def _port(self):
        return self.server.server_address[1]

    def _query_token(self):
        query = urllib.parse.urlparse(self.path).query
        values = urllib.parse.parse_qs(query).get("t")
        return values[0] if values else ""

    def _authorized(self, header_only=False):
        supplied = self.headers.get("X-Orchestra-Token") or ""
        if not supplied and not header_only:
            supplied = self._query_token()
        return hmac.compare_digest(supplied.encode("utf-8"), self.server.token.encode("utf-8"))

    def _host_ok(self):
        host = (self.headers.get("Host") or "").strip().lower()
        port = self._port()
        return host in ("127.0.0.1:%d" % port, "localhost:%d" % port)

    def _send(self, code, body, content_type="text/plain; charset=utf-8"):
        data = body if isinstance(body, bytes) else body.encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Cache-Control", "no-store")
        self.send_header("X-Content-Type-Options", "nosniff")
        self.send_header("Referrer-Policy", "no-referrer")
        self.end_headers()
        self.wfile.write(data)

    def _send_json(self, code, obj):
        self._send(code, _snapshot_body(obj), "application/json; charset=utf-8")

    def _gate(self, header_only=False):
        self.server.touch()
        if not self._host_ok() or not self._authorized(header_only):
            self._send(403, "forbidden")
            return False
        return True

    # -- routes
    def do_GET(self):
        if not self._gate():
            return
        route = urllib.parse.urlparse(self.path).path
        if route == "/healthz":
            self._send(200, "ok")
        elif route == "/":
            self._send(200, PAGE_HTML, "text/html; charset=utf-8")
        elif route == "/api/snapshot":
            self._send_json(200, build_snapshot())
        elif route == "/api/stream":
            self._stream()
        elif route == "/api/file":
            query = urllib.parse.parse_qs(urllib.parse.urlparse(self.path).query)
            requested = query.get("path", [""])[0]
            allowed = _allowed_file(requested)
            if allowed is None:
                self._send(404, "not found")
                return
            try:
                if os.path.getsize(allowed) > 1024 * 1024:
                    self._send(413, "file too large")
                    return
                with open(allowed, "rb") as fh:
                    self._send(200, fh.read())
            except OSError:
                self._send(404, "not found")
        else:
            self._send(404, "not found")

    def do_POST(self):
        if not self._gate(header_only=True):
            return
        route = urllib.parse.urlparse(self.path).path
        if route not in ("/api/stop", "/api/cooldown/clear", "/api/sweep"):
            self._send(404, "not found")
            return
        try:
            length = int(self.headers.get("Content-Length") or 0)
            payload = json.loads(self.rfile.read(min(length, 65536)).decode("utf-8") or "{}")
        except (ValueError, TypeError):
            self._send_json(400, {"error": "invalid body"})
            return
        if route == "/api/cooldown/clear":
            import agent_exec
            cfg = _resolved_config()
            executor = payload.get("executor") if isinstance(payload, dict) else None
            result = agent_exec.clear_cooldown(cfg, executor)
            if "error" in result:
                self._send_json(400, result)
            else:
                self._send_json(200, result)
            return
        if route == "/api/sweep":
            import agent_exec
            repo = payload.get("repo") if isinstance(payload, dict) else None
            registered = [os.path.realpath(e.get("repo")) for e in read_registry()
                          if isinstance(e.get("repo"), str)]
            if not isinstance(repo, str) or os.path.realpath(repo) not in registered:
                self._send_json(404, {"error": "unknown repo"})
                return
            result = agent_exec.isolate_sweep(
                repo, dry_run=not bool(payload.get("apply")),
                older_than=None, include_current=False, branches=False,
                force=False, include_live=False,
            )
            self._send_json(200, result)
            return
        state = payload.get("state") if isinstance(payload, dict) else None
        registered = [e["state"] for e in read_registry()]
        if not isinstance(state, str) or state not in registered:
            self._send_json(404, {"error": "unknown state"})
            return
        try:
            import agent_exec_wave
            agent_exec_wave.request_stop(state, "stopped from ui")
        except Exception as exc:  # noqa: BLE001
            self._send_json(500, {"error": str(exc)})
            return
        self._send_json(200, {"stop_requested": True})

    def _stream(self):
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-store")
        self.send_header("Connection", "close")
        self.end_headers()
        self.close_connection = True
        self.server.stream_delta(1)
        try:
            last_hash = None
            last_ping = time.time()
            while True:
                snapshot = build_snapshot()
                digest = _snapshot_hash(snapshot)
                if digest != last_hash:
                    last_hash = digest
                    self.wfile.write(
                        ("event: snapshot\ndata: %s\n\n" % _snapshot_body(snapshot)).encode("utf-8")
                    )
                    self.wfile.flush()
                    last_ping = time.time()
                elif time.time() - last_ping >= SSE_PING_SECONDS:
                    self.wfile.write(b": ping\n\n")
                    self.wfile.flush()
                    last_ping = time.time()
                self.server.touch()
                time.sleep(SSE_POLL_SECONDS)
        except (OSError, ValueError):
            pass
        finally:
            self.server.stream_delta(-1)


def make_server(port, token, idle_seconds):
    return _Server(("127.0.0.1", port), _Handler, token, idle_seconds)


def _watch_idle(server):
    interval = min(1.0, max(0.02, server.idle_seconds / 4.0))
    while True:
        time.sleep(interval)
        if server.idle_expired():
            server.shutdown()
            return


def run_server(server):
    """Serve until idle; returns after shutdown."""
    watcher = threading.Thread(target=_watch_idle, args=(server,), daemon=True)
    watcher.start()
    try:
        server.serve_forever(poll_interval=0.1)
    finally:
        server.server_close()


def _write_info(path, info):
    os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
    tmp = "%s.%d.tmp" % (path, os.getpid())
    with open(tmp, "w", encoding="utf-8") as fh:
        json.dump(info, fh)
    os.chmod(tmp, 0o600)
    os.replace(tmp, path)


def _remove_info_if_ours(path):
    try:
        with open(path, "r", encoding="utf-8") as fh:
            info = json.load(fh)
        if isinstance(info, dict) and info.get("pid") == os.getpid():
            os.remove(path)
    except (OSError, ValueError):
        pass


def _cmd_serve(port, idle_minutes):
    token = secrets.token_hex(16)
    server = make_server(port, token, idle_minutes * 60.0)
    path = info_path()
    _write_info(path, {
        "pid": os.getpid(), "port": server.server_address[1],
        "token": token, "started": time.time(),
    })
    atexit.register(_remove_info_if_ours, path)

    def _on_term(signum, frame):
        raise SystemExit(0)

    signal.signal(signal.SIGTERM, _on_term)
    run_server(server)
    return 0


# --- client side ---------------------------------------------------------------


def _read_info():
    try:
        with open(info_path(), "r", encoding="utf-8") as fh:
            info = json.load(fh)
    except (OSError, ValueError):
        return None
    if not isinstance(info, dict):
        return None
    if not all(k in info for k in ("pid", "port", "token")):
        return None
    return info


def _pid_alive(pid):
    if not isinstance(pid, int) or isinstance(pid, bool) or pid <= 0:
        return False
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    except OSError:
        return False
    return True


def _url(info):
    return "http://127.0.0.1:%s/?t=%s" % (info["port"], info["token"])


def _healthy(info):
    url = "http://127.0.0.1:%s/healthz?t=%s" % (info["port"], info["token"])
    try:
        with urllib.request.urlopen(url, timeout=2) as resp:
            return resp.status == 200
    except (OSError, ValueError):
        return False


def _live_info():
    info = _read_info()
    if info and _pid_alive(info.get("pid")) and _healthy(info):
        return info
    return None


def start_or_reuse(idle_minutes=IDLE_MINUTES_DEFAULT):
    """Start the dashboard if needed and return its live connection info."""
    info = _live_info()
    reused = info is not None
    if info is None:
        _spawn_server(idle_minutes)
        deadline = time.time() + START_WAIT_SECONDS
        while time.time() < deadline:
            info = _live_info()
            if info:
                break
            time.sleep(0.1)
    if info is None:
        raise RuntimeError("server did not start (see %s)" % log_path())
    return {"url": _url(info), "pid": info["pid"], "reused": reused}


def _spawn_server(idle_minutes):
    path = log_path()
    os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
    with open(path, "ab") as log:
        subprocess.Popen(
            [sys.executable, os.path.join(_HERE, "agent_exec_ui.py"),
             "--serve", "--port", "0", "--idle-minutes", repr(idle_minutes)],
            stdin=subprocess.DEVNULL, stdout=log, stderr=log,
            start_new_session=True, cwd=_HERE,
        )


def _open_url(url):
    opener = "open" if sys.platform == "darwin" else "xdg-open"
    try:
        subprocess.Popen([opener, url], stdin=subprocess.DEVNULL,
                         stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    except OSError as exc:
        sys.stderr.write("agent-exec ui: could not run %s: %s\n" % (opener, exc))


def _stop_server():
    info = _read_info()
    stopped = False
    if info and _pid_alive(info.get("pid")):
        try:
            os.kill(info["pid"], signal.SIGTERM)
            stopped = True
        except OSError:
            pass
        deadline = time.time() + 3.0
        while time.time() < deadline and _pid_alive(info["pid"]):
            time.sleep(0.05)
    try:
        os.remove(info_path())
    except OSError:
        pass
    return stopped


def _parse_args(args):
    opts = {"open": False, "stop": False, "json": False, "serve": False,
            "port": 0, "idle": IDLE_MINUTES_DEFAULT}
    i = 0
    while i < len(args):
        tok = args[i]
        if tok in ("--open", "--stop", "--json", "--serve"):
            opts[tok[2:]] = True
        elif tok in ("--idle-minutes", "--port"):
            if i + 1 >= len(args):
                return None, "%s needs a value" % tok
            try:
                value = float(args[i + 1]) if tok == "--idle-minutes" else int(args[i + 1])
            except ValueError:
                return None, "%s: invalid value %r" % (tok, args[i + 1])
            if value < 0:
                return None, "%s must not be negative" % tok
            opts["idle" if tok == "--idle-minutes" else "port"] = value
            i += 1
        elif tok in ("-h", "--help"):
            return None, ""
        else:
            return None, "unknown argument %r" % tok
        i += 1
    return opts, None


def cmd_ui(args):
    opts, err = _parse_args(args)
    if opts is None:
        if err:
            sys.stderr.write("agent-exec ui: %s\n" % err)
        sys.stderr.write(UI_USAGE)
        return 2 if err else 0
    if opts["serve"]:
        return _cmd_serve(opts["port"], opts["idle"])
    if opts["stop"]:
        stopped = _stop_server()
        if opts["json"]:
            print(json.dumps({"stopped": stopped}))
        else:
            print("stopped" if stopped else "not running")
        return 0

    try:
        info = start_or_reuse(opts["idle"])
    except RuntimeError as exc:
        sys.stderr.write("agent-exec ui: %s\n" % exc)
        return 1
    url = info["url"]
    if opts["open"]:
        _open_url(url)
    if opts["json"]:
        print(json.dumps(info))
    else:
        print(url)
    return 0


# --- page -----------------------------------------------------------------------

PAGE_HTML = r"""<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>orchestra</title>
<style>
:root {
  color-scheme: light dark;
  --bg: #f6f7f9; --fg: #1c2026; --muted: #667085; --card: #ffffff; --line: #d9dde3;
  --ok: #1a7f37; --run: #0969da; --warn: #9a6700; --bad: #cf222e; --idle: #6e7781;
}
@media (prefers-color-scheme: dark) {
  :root {
    --bg: #0f1216; --fg: #e6e9ee; --muted: #9aa4b2; --card: #181c22; --line: #2b313a;
    --ok: #3fb950; --run: #58a6ff; --warn: #d29922; --bad: #f85149; --idle: #8b949e;
  }
}
* { box-sizing: border-box; }
body { margin: 0; padding: 12px; background: var(--bg); color: var(--fg);
  font: 14px/1.45 system-ui, -apple-system, sans-serif; }
h1 { font-size: 18px; margin: 0 0 8px; }
h2 { font-size: 15px; margin: 0 0 6px; }
section { background: var(--card); border: 1px solid var(--line); border-radius: 8px;
  padding: 12px; margin-bottom: 12px; overflow-x: auto; }
.muted { color: var(--muted); }
.row { display: flex; flex-wrap: wrap; gap: 8px; align-items: center; }
.spacer { flex: 1; }
table { border-collapse: collapse; width: 100%; }
th, td { text-align: left; padding: 4px 8px; border-bottom: 1px solid var(--line); white-space: nowrap; }
.badge { display: inline-block; padding: 1px 8px; border-radius: 10px; font-size: 12px;
  border: 1px solid currentColor; }
.s-pending { color: var(--idle); } .s-implementing, .s-verifying, .s-fixing { color: var(--run); }
.s-ready, .s-integrated { color: var(--ok); } .s-needs, .s-blocked { color: var(--warn); }
.s-failed, .stopped { color: var(--bad); }
button { font: inherit; padding: 3px 10px; border-radius: 6px; cursor: pointer;
  border: 1px solid var(--bad); background: transparent; color: var(--bad); }
.need { cursor: pointer; padding: 3px 0; }
.need pre, pre.detail { white-space: pre-wrap; word-break: break-word; margin: 4px 0 0; font-size: 12px; }
.tl { font-family: ui-monospace, Menlo, monospace; font-size: 12px; }
.tl div { padding: 1px 0; white-space: nowrap; }
.e-fail, .e-bad { color: var(--bad); } .e-pass, .e-ok { color: var(--ok); }
.e-run { color: var(--run); } .e-warn { color: var(--warn); }
#conn { font-size: 12px; }
</style>
</head>
<body>
<div class="row"><h1>orchestra</h1><span class="spacer"></span><span id="conn" class="muted">connecting</span></div>
<div id="root"></div>
<script>
"use strict";
var TOKEN = new URLSearchParams(location.search).get("t") || "";
var root = document.getElementById("root");
var conn = document.getElementById("conn");
var openNeeds = {};
var last = null, poller = null;

function el(tag, attrs, children) {
  var node = document.createElement(tag);
  if (attrs) Object.keys(attrs).forEach(function (k) {
    if (k === "class") node.className = attrs[k];
    else if (k === "onclick") node.addEventListener("click", attrs[k]);
    else node.setAttribute(k, attrs[k]);
  });
  (children || []).forEach(function (c) {
    if (c === null || c === undefined) return;
    node.appendChild(typeof c === "object" ? c : document.createTextNode(String(c)));
  });
  return node;
}
function txt(v) { return v === null || v === undefined ? "" : (typeof v === "object" ? JSON.stringify(v) : String(v)); }
function elapsed(since) {
  if (typeof since !== "number") return "";
  var s = Math.max(0, Math.round(Date.now() / 1000 - since));
  if (s < 60) return s + "s";
  if (s < 3600) return Math.floor(s / 60) + "m" + (s % 60) + "s";
  return Math.floor(s / 3600) + "h" + Math.floor((s % 3600) / 60) + "m";
}
function clock(at) {
  if (typeof at !== "number") return "";
  return new Date(at * 1000).toLocaleTimeString();
}
function badge(status) {
  return el("span", { "class": "badge s-" + txt(status).replace(/[^a-z]/g, "") }, [txt(status)]);
}
function table(head, rows) {
  var thead = el("tr", null, head.map(function (h) { return el("th", null, [h]); }));
  var body = rows.map(function (r) {
    return el("tr", null, r.map(function (c) { return el("td", null, [c]); }));
  });
  return el("table", null, [el("thead", null, [thead]), el("tbody", null, body)]);
}
function fileLink(path, label) {
  if (!path) return null;
  return el("a", { href: "/api/file?path=" + encodeURIComponent(path) + "&t=" + encodeURIComponent(TOKEN),
    target: "_blank", rel: "noopener" }, [label]);
}

var EVENT_STYLE = [
  [/^dispatch-/, "▶", "e-run"], [/^check-/, "☑", "e-run"],
  [/^integrate-/, "⇄", "e-run"], [/^verify-/, "✔", "e-run"],
  [/^bisect-probe$/, "⌕", "e-warn"], [/^revert$/, "↺", "e-warn"],
  [/^full-/, "▣", "e-run"], [/^on-green$/, "✨", "e-ok"]
];
function eventLine(ev) {
  var style = null;
  EVENT_STYLE.forEach(function (s) { if (!style && s[0].test(ev.event || "")) style = s; });
  if (!style) return null;
  var d = (ev.detail && typeof ev.detail === "object") ? ev.detail : {};
  var cls = style[2];
  if (d.status === "fail" || d.result === "fail" || d.status === "failed" || d.result === "conflicted") cls = "e-fail";
  else if (d.status === "pass" || d.result === "pass" || d.status === "applied") cls = "e-pass";
  var parts = [clock(ev.at), style[1], ev.event];
  if (ev.pkg) parts.push(ev.pkg);
  if (d.name) parts.push(d.name);
  if (d.status) parts.push(d.status);
  if (d.result) parts.push(d.result);
  if (typeof d.seconds === "number") parts.push(d.seconds.toFixed(1) + "s");
  if (typeof ev.detail === "string" && ev.detail) parts.push(ev.detail);
  return el("div", { "class": cls }, [parts.join(" ")]);
}

function stopWave(state) {
  if (!confirm("Stop this wave?\n" + state)) return;
  fetch("/api/stop", {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-Orchestra-Token": TOKEN },
    body: JSON.stringify({ state: state })
  }).then(function (r) { if (!r.ok) alert("stop failed: " + r.status); });
}
function sweepRepo(repo) {
  fetch("/api/sweep", {
    method: "POST", headers: {"Content-Type": "application/json", "X-Orchestra-Token": TOKEN},
    body: JSON.stringify({repo: repo, apply: false})
  }).then(function (r) { return r.json(); }).then(function (dry) {
    if (confirm("Sweep preview:\n" + JSON.stringify(dry) + "\nApply this sweep?")) {
      fetch("/api/sweep", {
        method: "POST", headers: {"Content-Type": "application/json", "X-Orchestra-Token": TOKEN},
        body: JSON.stringify({repo: repo, apply: true})
      });
    }
  });
}

function renderWave(w) {
  if (w.error && !w.packages.length) {
    return el("section", null, [el("h2", null, [w.state]), el("div", { "class": "e-fail" }, [w.error])]);
  }
  var counts = Object.keys(w.counts || {}).map(function (k) { return k + " " + w.counts[k]; }).join(" · ");
  var head = el("div", { "class": "row" }, [
    el("h2", null, [txt(w.repo)]),
    el("span", { "class": "muted" }, ["into " + txt(w.into) + " · wave " + txt(w.wave)]),
    w.stopped ? el("span", { "class": "badge stopped" }, ["STOPPED"]) : null,
    el("span", { "class": "spacer" }),
    el("button", { onclick: function () { stopWave(w.state); } }, ["Stop wave"])
  ]);
  var pkgs = table(["id", "status", "elapsed", "executor", "files", "attempts"],
    w.packages.map(function (p) {
      var links = [fileLink(p.spec, p.id)];
      if (p.context) links.push(fileLink(p.context, "context"));
      if (p.correction) links.push(fileLink(p.correction, "correction"));
      return [el("span", null, links.filter(Boolean)), badge(p.status), elapsed(p.since), txt(p.executor),
        txt(p.files_live !== null && p.files_live !== undefined ? p.files_live + "*" : p.files_changed),
        txt(p.attempts)];
    }));
  var kids = [head, el("div", { "class": "muted" }, [counts]), pkgs];
  (w.packages || []).forEach(function (p) {
    if (p.detail) kids.push(el("div", { "class": "muted" }, [p.id + ": " + String(p.detail).split("\n")[0]]));
  });
  if ((w.needs || []).length) {
    kids.push(el("h2", null, ["Needs"]));
    w.needs.forEach(function (n, i) {
      var key = w.state + "#" + i + n.id;
      var full = txt(n.detail);
      var line = full.split("\n")[0];
      var box = el("div", { "class": "need" }, [
        el("div", null, [el("span", { "class": "badge s-needs" }, [txt(n.kind)]), " ",
          n.id === null ? txt("(wave)") :
            fileLink((w.packages || []).filter(function (p) { return p.id === n.id; })[0] &&
              (w.packages || []).filter(function (p) { return p.id === n.id; })[0].spec, txt(n.id)), " " + line])
      ]);
      if (openNeeds[key]) box.appendChild(el("pre", null, [full]));
      box.addEventListener("click", function () { openNeeds[key] = !openNeeds[key]; render(last); });
      kids.push(box);
    });
  }
  var lines = (w.events || []).map(eventLine).filter(Boolean).slice(-40);
  if (lines.length) {
    kids.push(el("h2", null, ["Mechanical stages"]));
    kids.push(el("div", { "class": "tl" }, lines));
  }
  return el("section", null, kids);
}

function renderErr(v) {
  return v && v.error ? el("div", { "class": "e-fail" }, [txt(v.error)]) : null;
}

function render(snap) {
  if (!snap) return;
  last = snap;
  var kids = [];
  var running = snap.running || {};
  var runRows = (running.dispatches || []).map(function (d) {
    return [txt(d.executor), txt(d.model), txt(d.task), txt(d.pid)];
  });
  kids.push(el("section", null, [
    el("h2", null, ["Running"]),
    el("div", { "class": "muted" }, [txt(running.waves_in_flight) + " packages in flight"]),
    table(["executor", "model", "task", "pid"], runRows)
  ]));
  if (renderErr(snap.waves)) kids.push(el("section", null, [renderErr(snap.waves)]));
  else if (!snap.waves.length) kids.push(el("section", { "class": "muted" }, ["No waves registered yet."]));
  else snap.waves.forEach(function (w) { kids.push(renderWave(w)); });

  var wt = [el("h2", null, ["Worktrees"])];
  if (renderErr(snap.worktrees)) wt.push(renderErr(snap.worktrees));
  else (snap.worktrees || []).forEach(function (g) {
    wt.push(el("div", { "class": "muted" }, [g.repo]));
    if (g.error) wt.push(el("div", { "class": "e-fail" }, [g.error]));
    wt.push(table(["task", "branch", "path"], (g.items || []).map(function (it) {
      return [txt(it.task), txt(it.branch), txt(it.path)];
    })));
  });
  kids.push(el("section", null, wt));

  var dp = [el("h2", null, ["Recent dispatches"])];
  if (renderErr(snap.dispatch)) dp.push(renderErr(snap.dispatch));
  else {
    var keys = [];
    snap.dispatch.forEach(function (r) { Object.keys(r).forEach(function (k) { if (keys.indexOf(k) < 0) keys.push(k); }); });
    dp.push(table(keys, snap.dispatch.map(function (r) { return keys.map(function (k) { return txt(r[k]); }); })));
  }
  kids.push(el("section", null, dp));

  var usage = [el("h2", null, ["Usage (24h)"])];
  if (renderErr(snap.usage)) usage.push(renderErr(snap.usage));
  else usage.push(table(["executor/model", "input", "output", "cached", "cost"],
    Object.keys(snap.usage || {}).filter(function (k) { return k !== "error"; }).map(function (k) {
      var v = snap.usage[k] || {};
      return [k, txt(v.input_tokens), txt(v.output_tokens), txt(v.cached_input_tokens), txt(v.cost_micro_usd)];
    })));
  kids.push(el("section", null, usage));

  var cd = [el("h2", null, ["Cooldown"])];
  if (renderErr(snap.cooldown)) cd.push(renderErr(snap.cooldown));
  else if (!Object.keys(snap.cooldown || {}).length) cd.push(el("div", { "class": "muted" }, ["none"]));
  else cd.push(el("pre", { "class": "detail" }, [JSON.stringify(snap.cooldown, null, 2)]));
  kids.push(el("section", null, cd));

  var actions = el("section", null, [
    el("h2", null, ["Actions"]),
    el("button", { onclick: function () {
      if (confirm("Clear all cooldowns?")) fetch("/api/cooldown/clear", {
        method: "POST", headers: {"Content-Type": "application/json", "X-Orchestra-Token": TOKEN},
        body: JSON.stringify({executor: null})
      });
    } }, ["Clear cooldowns"])
  ]);
  (snap.worktrees || []).forEach(function (g) {
    actions.appendChild(el("button", { onclick: function () { sweepRepo(g.repo); } }, ["Sweep " + g.repo]));
  });
  kids.push(actions);

  root.textContent = "";
  kids.forEach(function (k) { root.appendChild(k); });
}

function poll() {
  fetch("/api/snapshot", { headers: { "X-Orchestra-Token": TOKEN } })
    .then(function (r) { return r.json(); })
    .then(function (s) { conn.textContent = "polling"; render(s); })
    .catch(function () { conn.textContent = "offline"; });
}
function startPolling() {
  if (poller) return;
  poll();
  poller = setInterval(poll, 5000);
}
if (window.EventSource) {
  var es = new EventSource("/api/stream?t=" + encodeURIComponent(TOKEN));
  es.addEventListener("snapshot", function (e) {
    if (poller) { clearInterval(poller); poller = null; }
    conn.textContent = "live";
    try { render(JSON.parse(e.data)); } catch (err) { conn.textContent = "bad data"; }
  });
  es.onerror = function () { conn.textContent = "reconnecting"; startPolling(); };
} else {
  startPolling();
}
setInterval(function () { if (last) render(last); }, 1000);
</script>
</body>
</html>
"""


if __name__ == "__main__":
    sys.exit(cmd_ui(sys.argv[1:]))
