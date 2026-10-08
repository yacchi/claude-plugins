# /// script
# requires-python = ">=3.9"
# dependencies = ["pyyaml"]
# ///
"""Unit tests for agent_exec_ui.py (contract U2).

Run with: uv run test_agent_exec_ui.py
"""

import http.client
import json
import os
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import unittest
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import agent_exec_ui  # noqa: E402
import agent_exec_wave  # noqa: E402

UI_PY = os.path.join(os.path.dirname(os.path.abspath(__file__)), "agent_exec_ui.py")
TOKEN = "a" * 32


class UiTestBase(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="ui-test-")
        self.home = os.path.join(self.tmp, "home")
        os.makedirs(self.home)
        self.registry = os.path.join(self.tmp, "waves.jsonl")
        self.ui_state = os.path.join(self.tmp, "ui.json")
        self._saved = dict(os.environ)
        os.environ["HOME"] = self.home
        os.environ.pop("CLAUDE_CONFIG_DIR", None)
        os.environ["ORCHESTRA_WAVE_REGISTRY"] = self.registry
        os.environ["ORCHESTRA_UI_STATE"] = self.ui_state
        self.addCleanup(self._restore)

    def _restore(self):
        os.environ.clear()
        os.environ.update(self._saved)
        shutil.rmtree(self.tmp, ignore_errors=True)

    def make_wave(self, name="w1", detail="", register=True):
        wave_dir = os.path.join(self.tmp, name)
        os.makedirs(wave_dir, exist_ok=True)
        state = os.path.join(wave_dir, "state.json")
        with open(state, "w") as fh:
            json.dump({
                "version": 1, "plan": "/x/plan.json",
                "integration": {"task": "wave-int", "path": None, "base": None},
                "wave": 3,
                "packages": {
                    "A-1": {"status": "implementing", "since": time.time() - 5, "attempts": 1,
                            "tree": None, "executor": "codex", "session": False,
                            "files_changed": 2, "commit": None, "detail": detail},
                    "A-2": {"status": "pending", "since": time.time(), "attempts": 0,
                            "tree": None, "executor": None, "session": False,
                            "files_changed": None, "commit": None, "detail": ""},
                },
                "needs": [{"id": "A-1", "kind": "escalate", "detail": "line one\nline two", "at": 1.0}],
                "stopped": None, "updated": time.time(),
            }, fh)
        events_file = os.path.join(wave_dir, "state.events.jsonl")
        with open(events_file, "w") as fh:
            for record in EVENTS:
                fh.write(json.dumps(record) + "\n")
        if register:
            self.register(state)
        return state

    def register(self, state, repo=None):
        with open(self.registry, "a") as fh:
            fh.write(json.dumps({
                "state": state, "plan": "/x/plan.json", "repo": repo or self.tmp,
                "into": "wave-int", "registered_at": time.time(),
            }) + "\n")

    def start(self, idle_seconds=600.0):
        server = agent_exec_ui.make_server(0, TOKEN, idle_seconds)
        thread = threading.Thread(target=agent_exec_ui.run_server, args=(server,), daemon=True)
        thread.start()
        self.addCleanup(self._stop, server, thread)
        return server

    @staticmethod
    def _stop(server, thread):
        server.shutdown()
        thread.join(5)

    def request(self, server, method, path, headers=None, body=None, host=None):
        port = server.server_address[1]
        conn = http.client.HTTPConnection("127.0.0.1", port, timeout=10)
        hdrs = dict(headers or {})
        hdrs["Host"] = host or "127.0.0.1:%d" % port
        conn.request(method, path, body=body, headers=hdrs)
        resp = conn.getresponse()
        data = resp.read()
        conn.close()
        return resp.status, data


EVENTS = [
        {"at": 1.0, "pkg": "A-1", "from": None, "to": None, "event": "dispatch-start",
         "detail": json.dumps({"attempt": 1, "cls": "light", "kind": "implement"})},
        {"at": 2.0, "pkg": None, "from": None, "to": None, "event": "verify-end",
         "detail": "not json"},
]


class AuthTests(UiTestBase):
    def test_token_required(self):
        server = self.start()
        self.assertEqual(self.request(server, "GET", "/healthz")[0], 403)
        self.assertEqual(self.request(server, "GET", "/healthz?t=" + "b" * 32)[0], 403)
        self.assertEqual(self.request(server, "GET", "/healthz?t=" + TOKEN)[0], 200)
        status, _ = self.request(server, "GET", "/healthz", headers={"X-Orchestra-Token": TOKEN})
        self.assertEqual(status, 200)
        self.assertEqual(self.request(server, "GET", "/api/snapshot")[0], 403)

    def test_host_header_checked(self):
        server = self.start()
        port = server.server_address[1]
        self.assertEqual(
            self.request(server, "GET", "/healthz?t=" + TOKEN, host="evil.example:%d" % port)[0], 403)
        self.assertEqual(
            self.request(server, "GET", "/healthz?t=" + TOKEN, host="127.0.0.1:1")[0], 403)
        self.assertEqual(
            self.request(server, "GET", "/healthz?t=" + TOKEN, host="localhost:%d" % port)[0], 200)


class RegistryTests(UiTestBase):
    def test_dedupe_missing_and_truncated(self):
        a = self.make_wave("a", register=False)
        b = self.make_wave("b", register=False)
        self.register(a, repo="/old")
        self.register(b)
        self.register(a, repo="/new")
        self.register(os.path.join(self.tmp, "gone", "state.json"))
        with open(self.registry, "a") as fh:
            fh.write('{"state": "/trunc')
        entries = agent_exec_ui.read_registry()
        self.assertEqual([e["state"] for e in entries], [b, a])
        self.assertEqual(entries[1]["repo"], "/new")

    def test_missing_registry_is_empty(self):
        self.assertEqual(agent_exec_ui.read_registry(), [])


class SnapshotTests(UiTestBase):
    def get_snapshot(self, server):
        status, data = self.request(server, "GET", "/api/snapshot?t=" + TOKEN)
        self.assertEqual(status, 200)
        return json.loads(data)

    def test_shape(self):
        state = self.make_wave()
        snap = self.get_snapshot(self.start())
        for key in ("generated_at", "waves", "worktrees", "dispatch", "cooldown",
                    "running", "usage"):
            self.assertIn(key, snap)
        wave = snap["waves"][0]
        self.assertEqual(wave["state"], state)
        self.assertEqual(wave["into"], "wave-int")
        self.assertEqual(wave["wave"], 3)
        self.assertEqual(wave["counts"], {"implementing": 1, "pending": 1})
        pkg = [p for p in wave["packages"] if p["id"] == "A-1"][0]
        self.assertEqual(sorted(pkg), sorted(
            ["id", "status", "since", "executor", "files_changed", "files_live",
             "attempts", "detail", "spec", "context", "correction"]))
        self.assertEqual(wave["needs"][0]["kind"], "escalate")
        self.assertEqual(wave["events"][0]["detail"]["kind"], "implement")
        self.assertEqual(wave["events"][1]["detail"], "not json")
        self.assertEqual([w["repo"] for w in snap["worktrees"]], [self.tmp])
        self.assertEqual(snap["dispatch"], [])
        self.assertEqual(snap["cooldown"], {})

    def test_dispatch_and_cooldown_read_from_home(self):
        self.make_wave()
        runs = os.path.join(self.home, ".claude", "orchestra", "runs")
        os.makedirs(runs)
        with open(os.path.join(runs, "r1.jsonl"), "w") as fh:
            fh.write(json.dumps({"executor": "pi", "model": "m", "cls": "light",
                                 "status": "ok", "paths": ["x"], "duration_s": 1.5}) + "\n")
        snap = self.get_snapshot(self.start())
        if isinstance(snap["dispatch"], dict):
            self.fail("dispatch errored: %r" % snap["dispatch"])
        self.assertEqual(snap["dispatch"][0]["executor"], "pi")
        self.assertNotIn("paths", snap["dispatch"][0])
        self.assertEqual(snap["dispatch"][0]["duration_s"], 1.5)

    def test_failing_source_becomes_error_not_500(self):
        self.make_wave()
        os.makedirs(os.path.join(self.home, ".claude"))
        with open(os.path.join(self.home, ".claude", "orchestra.yaml"), "w") as fh:
            fh.write("a: [unclosed\n")
        snap = self.get_snapshot(self.start())
        self.assertIn("error", snap["dispatch"])
        self.assertIn("error", snap["cooldown"])
        self.assertEqual(len(snap["waves"]), 1)

    def test_broken_state_file_is_per_wave_error(self):
        state = self.make_wave()
        with open(state, "w") as fh:
            fh.write("{broken")
        snap = self.get_snapshot(self.start())
        self.assertIn("error", snap["waves"][0])

    def test_wave_level_need_with_null_id_is_listed_and_labelled(self):
        state = self.make_wave()
        with open(state) as fh:
            data = json.load(fh)
        data["needs"].append({"id": None, "kind": "environment",
                              "detail": "ensure failed\nmore", "at": 2.0})
        with open(state, "w") as fh:
            json.dump(data, fh)
        server = self.start()
        snap = self.get_snapshot(server)
        needs = snap["waves"][0]["needs"]
        self.assertIn({"id": None, "kind": "environment",
                       "detail": "ensure failed\nmore", "at": 2.0}, needs)
        status, page = self.request(server, "GET", "/?t=" + TOKEN)
        self.assertEqual(status, 200)
        self.assertIn('n.id === null ? txt("(wave)")', page.decode("utf-8"))

    def test_html_page_served_and_escapes_via_textnodes(self):
        state = self.make_wave(detail="<script>alert(1)</script>")
        server = self.start()
        status, page = self.request(server, "GET", "/?t=" + TOKEN)
        self.assertEqual(status, 200)
        page = page.decode("utf-8")
        self.assertIn("prefers-color-scheme", page)
        self.assertNotIn("innerHTML", page)
        self.assertIn("createTextNode", page)
        self.assertNotIn("http://", page.replace("http://www.w3.org", ""))
        # the payload travels only as JSON data, never inside the HTML document
        self.assertNotIn("<script>alert(1)</script>", page)
        snap = self.get_snapshot(server)
        pkg = [p for p in snap["waves"][0]["packages"] if p["id"] == "A-1"][0]
        self.assertEqual(pkg["detail"], "<script>alert(1)</script>")
        self.assertEqual(state, snap["waves"][0]["state"])


class StreamTests(UiTestBase):
    def read_event(self, resp):
        data = None
        while True:
            line = resp.readline().decode("utf-8")
            if line.startswith("data: "):
                data = line[len("data: "):]
            if line == "\n" and data is not None:
                return json.loads(data)
            if line == "":
                return None

    def test_first_snapshot_then_update_on_change(self):
        saved = agent_exec_ui.SSE_POLL_SECONDS
        agent_exec_ui.SSE_POLL_SECONDS = 0.1
        self.addCleanup(setattr, agent_exec_ui, "SSE_POLL_SECONDS", saved)
        state = self.make_wave()
        server = self.start()
        port = server.server_address[1]
        conn = http.client.HTTPConnection("127.0.0.1", port, timeout=10)
        conn.request("GET", "/api/stream?t=" + TOKEN, headers={"Host": "127.0.0.1:%d" % port})
        resp = conn.getresponse()
        self.assertEqual(resp.status, 200)
        first = self.read_event(resp)
        self.assertEqual(first["waves"][0]["counts"]["implementing"], 1)
        with open(state) as fh:
            doc = json.load(fh)
        doc["packages"]["A-1"]["status"] = "ready"
        with open(state, "w") as fh:
            json.dump(doc, fh)
        second = self.read_event(resp)
        self.assertEqual(second["waves"][0]["counts"].get("ready"), 1)
        conn.close()


class StopTests(UiTestBase):
    def post(self, server, state, header=True):
        body = json.dumps({"state": state})
        path = "/api/stop"
        headers = {"Content-Type": "application/json"}
        if header:
            headers["X-Orchestra-Token"] = TOKEN
        else:
            path += "?t=" + TOKEN
        return self.request(server, "POST", path, headers=headers, body=body)

    def test_stop_registered(self):
        state = self.make_wave()
        server = self.start()
        status, data = self.post(server, state)
        self.assertEqual(status, 200)
        self.assertEqual(json.loads(data), {"stop_requested": True})
        self.assertTrue(agent_exec_wave.stop_requested(state))

    def test_unregistered_is_404(self):
        state = self.make_wave(register=False)
        server = self.start()
        self.assertEqual(self.post(server, state)[0], 404)
        self.assertFalse(agent_exec_wave.stop_requested(state))

    def test_query_token_only_is_403(self):
        state = self.make_wave()
        server = self.start()
        self.assertEqual(self.post(server, state, header=False)[0], 403)
        self.assertFalse(agent_exec_wave.stop_requested(state))


class V1Base(UiTestBase):
    def setUp(self):
        super().setUp()
        os.environ["ORCHESTRA_ALIVE_DIR"] = os.path.join(self.tmp, "alive")
        import agent_exec
        self.agent_exec = agent_exec
        self._saved_alive = agent_exec._heartbeat_dir_cache
        agent_exec._heartbeat_dir_cache = os.path.join(self.tmp, "alive")
        self.addCleanup(self._restore_alive)

    def _restore_alive(self):
        self.agent_exec._heartbeat_dir_cache = self._saved_alive

    def cfg(self):
        cfg, err = self.agent_exec.resolve_config()
        self.assertFalse(err)
        return cfg

    def post(self, server, path, payload, header=True):
        headers = {"Content-Type": "application/json"}
        if header:
            headers["X-Orchestra-Token"] = TOKEN
        else:
            path += "?t=" + TOKEN
        return self.request(server, "POST", path, headers=headers, body=json.dumps(payload))

    def snapshot(self, server):
        status, data = self.request(server, "GET", "/api/snapshot?t=" + TOKEN)
        self.assertEqual(status, 200)
        return json.loads(data)


class ListDetachedTests(V1Base):
    def plant(self, token, pid, spec=True, **extra):
        cfg = self.cfg()
        detach = self.agent_exec._detach_dir_from_cfg(cfg)
        os.makedirs(detach, exist_ok=True)
        with open(os.path.join(detach, token + ".json"), "w") as fh:
            json.dump(dict({"pid": pid, "token": token, "started": 123.0}, **extra), fh)
        if spec:
            tokens = self.agent_exec._token_dir_from_cfg(cfg)
            os.makedirs(tokens, exist_ok=True)
            with open(os.path.join(tokens, token + ".json"), "w") as fh:
                json.dump({"class": "light", "archetype": "default", "workdir": "/w",
                           "run_id": None, "isolate": "auto", "task": "T-1",
                           "prompt_files": [], "executor": "codex", "model": "m1"}, fh)

    def test_alive_dead_corrupt_and_missing_token(self):
        dead = subprocess.Popen([sys.executable, "-c", "pass"])
        dead.wait()
        self.plant("dsp-000000000001", os.getpid())
        self.plant("dsp-000000000002", dead.pid, spec=False)
        detach = self.agent_exec._detach_dir_from_cfg(self.cfg())
        with open(os.path.join(detach, "dsp-000000000003.json"), "w") as fh:
            fh.write("{corrupt")
        with open(os.path.join(detach, "notes.txt"), "w") as fh:
            fh.write("ignored")
        items = {i["token"]: i for i in self.agent_exec.list_detached_dispatches(self.cfg())}
        self.assertEqual(sorted(items), ["dsp-000000000001", "dsp-000000000002"])
        live = items["dsp-000000000001"]
        self.assertTrue(live["alive"])
        self.assertEqual(live["pid"], os.getpid())
        self.assertEqual(live["started"], 123.0)
        self.assertEqual((live["executor"], live["model"], live["class"], live["task"]),
                         ("codex", "m1", "light", "T-1"))
        gone = items["dsp-000000000002"]
        self.assertFalse(gone["alive"])
        self.assertEqual((gone["executor"], gone["model"], gone["class"], gone["task"]),
                         (None, None, None, None))

    def test_missing_directory_is_empty(self):
        self.assertEqual(self.agent_exec.list_detached_dispatches(self.cfg()), [])


class RunningSnapshotTests(ListDetachedTests):
    def test_running_section(self):
        state = self.make_wave()
        self.plant("dsp-00000000000a", os.getpid(), executor="pi")
        self.plant("dsp-00000000000b", 2 ** 22 + 12345, spec=False)
        with open(os.path.join(os.path.dirname(state), "orca-sessions.json"), "w") as fh:
            json.dump([{"wave_state": state, "pkg": "A-1", "terminal": "term-1",
                        "worktree": "/wt/a1"}], fh)
        running = self.snapshot(self.start())["running"]
        self.assertEqual([d["token"] for d in running["dispatches"]], ["dsp-00000000000a"])
        self.assertEqual(running["by_executor"], {"codex": 1})
        self.assertEqual(running["orca_sessions"], [{
            "wave_state": state, "pkg": "A-1", "terminal": "term-1", "worktree": "/wt/a1"}])
        self.assertEqual(running["waves_in_flight"], 1)


class LiveSnapshotTests(V1Base):
    def test_files_live_in_snapshot(self):
        tree = os.path.join(self.tmp, "tree")
        os.makedirs(tree)
        env = dict(os.environ, GIT_CONFIG_GLOBAL="/dev/null", GIT_CONFIG_SYSTEM="/dev/null")
        subprocess.run(["git", "init", "-q", "."], cwd=tree, check=True, env=env)
        with open(os.path.join(tree, "x.txt"), "w") as fh:
            fh.write("x\n")
        state = self.make_wave()
        with open(state) as fh:
            data = json.load(fh)
        data["packages"]["A-1"]["tree"] = tree
        with open(state, "w") as fh:
            json.dump(data, fh)
        agent_exec_ui._LIVE_CACHE.clear()
        wave = self.snapshot(self.start())["waves"][0]
        pkgs = {p["id"]: p for p in wave["packages"]}
        self.assertEqual(pkgs["A-1"]["files_live"], 1)
        self.assertIsNone(pkgs["A-2"]["files_live"])


class FileRouteTests(V1Base):
    def setUp(self):
        super().setUp()
        self.state = self.make_wave()
        self.wdir = os.path.dirname(self.state)
        self.plan_dir = os.path.join(self.tmp, "plan")
        os.makedirs(os.path.join(self.plan_dir, "specs"))
        self.plan = os.path.join(self.plan_dir, "plan.json")
        with open(self.plan, "w") as fh:
            json.dump({"preamble": ["preamble.md"],
                       "packages": [{"id": "A-1", "spec": "specs/a1.md"}]}, fh)
        self.spec = self.write(os.path.join(self.plan_dir, "specs", "a1.md"), "SPEC")
        self.preamble = self.write(os.path.join(self.plan_dir, "preamble.md"), "PRE")
        self.unrelated = self.write(os.path.join(self.tmp, "unrelated.md"), "SECRET")
        with open(self.state) as fh:
            data = json.load(fh)
        data["plan"] = self.plan
        with open(self.state, "w") as fh:
            json.dump(data, fh)
        for name in ("context", "corrections", "carry"):
            os.makedirs(os.path.join(self.wdir, name))
        self.server = self.start()

    @staticmethod
    def write(path, text):
        with open(path, "w") as fh:
            fh.write(text)
        return path

    def get(self, path):
        import urllib.parse
        return self.request(self.server, "GET",
                            "/api/file?t=%s&path=%s" % (TOKEN, urllib.parse.quote(path, safe="")))

    def test_allowed_files(self):
        ctx = self.write(os.path.join(self.wdir, "context", "A-1.md"), "CTX")
        cor = self.write(os.path.join(self.wdir, "corrections", "A-1.md"), "COR")
        car = self.write(os.path.join(self.wdir, "carry", "n.md"), "CAR")
        for path, text in ((self.spec, "SPEC"), (self.preamble, "PRE"), (ctx, "CTX"),
                           (cor, "COR"), (car, "CAR")):
            status, data = self.get(path)
            self.assertEqual((status, data.decode()), (200, text), path)

    def test_content_type_and_token_required(self):
        port = self.server.server_address[1]
        conn = http.client.HTTPConnection("127.0.0.1", port, timeout=10)
        conn.request("GET", "/api/file?t=%s&path=%s" % (TOKEN, self.spec),
                     headers={"Host": "127.0.0.1:%d" % port})
        resp = conn.getresponse()
        resp.read()
        self.assertEqual(resp.getheader("Content-Type"), "text/plain; charset=utf-8")
        conn.close()
        status, _ = self.request(self.server, "GET", "/api/file?path=" + self.spec)
        self.assertEqual(status, 403)

    def test_refuses_unrelated_dotdot_plan_and_directory(self):
        self.assertEqual(self.get(self.unrelated)[0], 404)
        self.assertEqual(self.get(os.path.join(self.wdir, "context", "..", "..",
                                               "unrelated.md"))[0], 404)
        self.assertEqual(self.get(os.path.join(self.plan_dir, "specs", "..", "specs",
                                               "a1.md"))[0], 404)
        self.assertEqual(self.get(self.plan)[0], 404)
        self.assertEqual(self.get(os.path.join(self.wdir, "context"))[0], 404)
        self.assertEqual(self.get(self.plan_dir)[0], 404)
        self.assertEqual(self.get("")[0], 404)

    def test_refuses_symlink_escape(self):
        link = os.path.join(self.wdir, "context", "link.md")
        os.symlink(self.unrelated, link)
        self.assertEqual(self.get(link)[0], 404)

    def test_oversize_is_413(self):
        big = os.path.join(self.wdir, "context", "big.md")
        with open(big, "wb") as fh:
            fh.write(b"x" * (1024 * 1024 + 1))
        self.assertEqual(self.get(big)[0], 413)

    def test_snapshot_links(self):
        ctx = self.write(os.path.join(self.wdir, "context", "A-1.md"), "CTX")
        pkgs = {p["id"]: p for p in self.snapshot(self.server)["waves"][0]["packages"]}
        self.assertEqual(pkgs["A-1"]["spec"], self.spec)
        self.assertEqual(pkgs["A-1"]["context"], ctx)
        self.assertIsNone(pkgs["A-1"]["correction"])
        self.assertIsNone(pkgs["A-2"]["spec"])


class UsageSlotTests(V1Base):
    def setUp(self):
        super().setUp()
        agent_exec_ui._USAGE_CACHE = (0.0, None)
        self.addCleanup(setattr, agent_exec_ui, "_USAGE_CACHE", (0.0, None))
        self.original = self.agent_exec.build_usage_report
        self.addCleanup(setattr, self.agent_exec, "build_usage_report", self.original)

    def test_error_is_isolated(self):
        self.make_wave()

        def boom(*args, **kwargs):
            raise RuntimeError("usage exploded")

        self.agent_exec.build_usage_report = boom
        snap = self.snapshot(self.start())
        self.assertEqual(snap["usage"], {"error": "usage exploded"})
        self.assertEqual(len(snap["waves"]), 1)
        self.assertIn("cooldown", snap)

    def test_totals_per_executor_model(self):
        self.agent_exec.build_usage_report = lambda *a, **k: {
            "codex": {"by_model": {"m1": {"input_tokens": 5, "output_tokens": 2}}}}
        snap = self.snapshot(self.start())
        self.assertEqual(snap["usage"]["codex/m1"]["input_tokens"], 5)


class CooldownClearTests(V1Base):
    def plant(self):
        path = self.agent_exec.cooldown_state_path(self.cfg())
        os.makedirs(os.path.dirname(path), exist_ok=True)
        until = time.time() + 1000
        with open(path, "w") as fh:
            json.dump({"codex": {"until": until, "reason": "quota"},
                       "pi": {"until": until, "reason": "quota"}}, fh)
        return path

    def load(self, path):
        try:
            with open(path) as fh:
                return json.load(fh)
        except OSError:
            return {}

    def test_clear_one_then_all(self):
        path = self.plant()
        self.assertTrue(path.startswith(self.home))
        server = self.start()
        status, data = self.post(server, "/api/cooldown/clear", {"executor": "codex"})
        self.assertEqual((status, json.loads(data)), (200, {"cleared": "codex"}))
        self.assertEqual(list(self.load(path)), ["pi"])
        status, data = self.post(server, "/api/cooldown/clear", {"executor": None})
        self.assertEqual((status, json.loads(data)), (200, {"cleared": "all"}))
        self.assertEqual(self.load(path), {})

    def test_unknown_executor_400_and_state_untouched(self):
        path = self.plant()
        status, _ = self.post(self.start(), "/api/cooldown/clear", {"executor": "nope"})
        self.assertEqual(status, 400)
        self.assertEqual(sorted(self.load(path)), ["codex", "pi"])

    def test_header_token_required(self):
        path = self.plant()
        status, _ = self.post(self.start(), "/api/cooldown/clear", {"executor": None},
                              header=False)
        self.assertEqual(status, 403)
        self.assertEqual(sorted(self.load(path)), ["codex", "pi"])


class SweepRouteTests(V1Base):
    def setUp(self):
        super().setUp()
        self.repo = os.path.join(self.tmp, "repo")
        os.makedirs(self.repo)
        env = dict(os.environ, GIT_CONFIG_GLOBAL="/dev/null", GIT_CONFIG_SYSTEM="/dev/null")
        for args in (["init", "-q", "-b", "main", "."], ["config", "user.email", "t@e.com"],
                     ["config", "user.name", "T"]):
            subprocess.run(["git"] + args, cwd=self.repo, check=True, env=env)
        with open(os.path.join(self.repo, "README.md"), "w") as fh:
            fh.write("hi\n")
        subprocess.run(["git", "add", "-A"], cwd=self.repo, check=True, env=env)
        subprocess.run(["git", "commit", "-q", "-m", "init"], cwd=self.repo, check=True, env=env)
        self.old_session = os.environ.pop("CLAUDE_CODE_SESSION_ID", None)
        self.addCleanup(self._restore_session)

    def _restore_session(self):
        os.environ.pop("CLAUDE_CODE_SESSION_ID", None)
        if self.old_session is not None:
            os.environ["CLAUDE_CODE_SESSION_ID"] = self.old_session

    def plant(self, task, session):
        os.environ["CLAUDE_CODE_SESSION_ID"] = session
        created = self.agent_exec.isolate_create(self.repo, task, backend="git")
        os.environ.pop("CLAUDE_CODE_SESSION_ID", None)
        self.assertEqual(created.get("status"), "created", created)
        return created["path"]

    def test_unregistered_repo_404(self):
        self.make_wave()
        clean = self.plant("alpha", "aaaaaaaa")
        status, _ = self.post(self.start(), "/api/sweep", {"repo": self.repo, "apply": True})
        self.assertEqual(status, 404)
        self.assertTrue(os.path.isdir(clean))

    def test_header_token_required(self):
        self.make_wave(register=False)
        self.register(os.path.join(self.tmp, "w1", "state.json"), repo=self.repo)
        clean = self.plant("alpha", "aaaaaaaa")
        status, _ = self.post(self.start(), "/api/sweep", {"repo": self.repo, "apply": True},
                              header=False)
        self.assertEqual(status, 403)
        self.assertTrue(os.path.isdir(clean))

    def test_dry_run_then_apply_never_forces(self):
        state = self.make_wave(register=False)
        self.register(state, repo=self.repo)
        clean = self.plant("alpha", "aaaaaaaa")
        dirty = self.plant("beta", "bbbbbbbb")
        with open(os.path.join(dirty, "worker.txt"), "w") as fh:
            fh.write("uncollected\n")
        server = self.start()
        status, data = self.post(server, "/api/sweep", {"repo": self.repo, "apply": False})
        self.assertEqual(status, 200)
        self.assertEqual(json.loads(data)["status"], "ok")
        self.assertTrue(os.path.isdir(clean))
        self.assertTrue(os.path.isdir(dirty))
        status, data = self.post(server, "/api/sweep", {"repo": self.repo, "apply": True})
        self.assertEqual(status, 200)
        result = json.loads(data)
        self.assertEqual(result["summary"]["removed"], 1)
        self.assertEqual(result["summary"]["dirty"], 1)
        self.assertFalse(os.path.isdir(clean))
        self.assertTrue(os.path.isfile(os.path.join(dirty, "worker.txt")))


class PageSafetyTests(UiTestBase):
    def test_no_innerhtml_or_inline_data_in_page(self):
        server = self.start()
        status, page = self.request(server, "GET", "/?t=" + TOKEN)
        page = page.decode("utf-8")
        self.assertEqual(status, 200)
        for needle in ("innerHTML", "outerHTML", "insertAdjacentHTML", "document.write"):
            self.assertNotIn(needle, page)
        for needle in ("/api/file", "/api/cooldown/clear", "/api/sweep", "Usage (24h)", "Running"):
            self.assertIn(needle, page)


class IdleTests(UiTestBase):
    def test_idle_shutdown(self):
        server = agent_exec_ui.make_server(0, TOKEN, 0.3)
        thread = threading.Thread(target=agent_exec_ui.run_server, args=(server,), daemon=True)
        started = time.time()
        thread.start()
        thread.join(10)
        self.assertFalse(thread.is_alive())
        self.assertLess(time.time() - started, 8)


class CmdUiTests(UiTestBase):
    def tearDown(self):
        subprocess.run([sys.executable, UI_PY, "--stop"], env=dict(os.environ),
                       capture_output=True, timeout=30)
        for _ in range(50):
            if not os.path.exists(self.ui_state):
                break
            time.sleep(0.1)

    def run_ui(self, *args):
        return subprocess.run([sys.executable, UI_PY] + list(args), env=dict(os.environ),
                              capture_output=True, text=True, timeout=60)

    def test_reuse_and_stop(self):
        first = self.run_ui("--json")
        self.assertEqual(first.returncode, 0, first.stderr)
        one = json.loads(first.stdout)
        self.assertFalse(one["reused"])
        self.assertRegex(one["url"], r"^http://127\.0\.0\.1:\d+/\?t=[0-9a-f]{32}$")
        with urllib.request.urlopen(one["url"].replace("/?t=", "/healthz?t="), timeout=5) as resp:
            self.assertEqual(resp.status, 200)
        second = json.loads(self.run_ui("--json").stdout)
        self.assertTrue(second["reused"])
        self.assertEqual(second["pid"], one["pid"])
        self.assertEqual(second["url"], one["url"])
        with open(self.ui_state) as fh:
            info = json.load(fh)
        self.assertEqual(info["pid"], one["pid"])
        stopped = self.run_ui("--stop")
        self.assertEqual(stopped.returncode, 0)
        self.assertFalse(os.path.exists(self.ui_state))
        with self.assertRaises(OSError):
            os.kill(one["pid"], 0)

    def test_bad_argument_exits_2(self):
        self.assertEqual(self.run_ui("--bogus").returncode, 2)


if __name__ == "__main__":
    unittest.main()
