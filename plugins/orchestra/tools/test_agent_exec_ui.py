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
        for key in ("generated_at", "waves", "worktrees", "dispatch", "cooldown"):
            self.assertIn(key, snap)
        wave = snap["waves"][0]
        self.assertEqual(wave["state"], state)
        self.assertEqual(wave["into"], "wave-int")
        self.assertEqual(wave["wave"], 3)
        self.assertEqual(wave["counts"], {"implementing": 1, "pending": 1})
        pkg = [p for p in wave["packages"] if p["id"] == "A-1"][0]
        self.assertEqual(sorted(pkg), sorted(
            ["id", "status", "since", "executor", "files_changed", "attempts", "detail"]))
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
            fh.write(json.dumps({"executor": "copilot", "model": "m", "cls": "light",
                                 "status": "ok", "paths": ["x"], "duration_s": 1.5}) + "\n")
        snap = self.get_snapshot(self.start())
        if isinstance(snap["dispatch"], dict):
            self.fail("dispatch errored: %r" % snap["dispatch"])
        self.assertEqual(snap["dispatch"][0]["executor"], "copilot")
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
